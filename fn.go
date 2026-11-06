package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/fastschema/qjs"
)

var (
	ErrTimeout = errors.New("handler exceeded its time limit")
	ErrBusy    = errors.New("too many handlers running, try again")
)

const (
	fnMemoryLimit = 32 << 20
	fnStackLimit  = 1 << 20
)

// Handlers run in QuickJS compiled to wasm under wazero (no cgo). The guest has no
// network and no process access, and an empty directory is its only filesystem.
// The wasm engine's MemoryLimit only stops large single allocations, so the
// context deadline and the concurrency cap are what actually bound a runaway handler.
const fnPrelude = `
delete globalThis.os;
delete globalThis.std;
globalThis.env = Object.freeze(JSON.parse(__env));
globalThis.kv = {
  get: k => { const r = __kv('get', String(k)); return r === null ? undefined : JSON.parse(r); },
  set: (k, v) => { __kv('set', String(k), JSON.stringify(v)); },
  delete: k => { __kv('delete', String(k)); },
  list: () => JSON.parse(__kv('list')),
};
`

const fnInvoke = `(async () => {
  if (typeof handle !== 'function') throw new Error('handler must define handle(req)');
  const r = await handle(JSON.parse(__req));
  return JSON.stringify(r === undefined ? null : r);
})()`

type FnRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	User    string            `json:"user"`
}

type FnResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

type kvBackend interface {
	KVAll(name string) (map[string]json.RawMessage, error)
	KVSet(name, key string, val json.RawMessage) error
}

type Runner struct {
	sem      chan struct{}
	timeout  time.Duration
	emptyDir string
	cacheDir string
}

func NewRunner(dataDir string, concurrency int, timeout time.Duration) (*Runner, error) {
	empty, err := os.MkdirTemp("", "nephos-empty-")
	if err != nil {
		return nil, err
	}
	r := &Runner{sem: make(chan struct{}, concurrency), timeout: timeout, emptyDir: empty, cacheDir: dataDir + "/wasm-cache"}
	// First runtime fixes the engine-wide wasm config, so make it ours and pay the compile now.
	if _, err := r.Run(context.Background(), "function handle(){}", FnRequest{}, nil, "", nil); err != nil {
		return nil, fmt.Errorf("warm up js runtime: %w", err)
	}
	return r, nil
}

func (r *Runner) Run(parent context.Context, src string, req FnRequest, kv kvBackend, app string, env map[string]string) (resp FnResponse, err error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-parent.Done():
		return resp, ErrBusy
	case <-time.After(r.timeout):
		return resp, ErrBusy
	}

	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	// qjs panics when wazero closes the module on deadline.
	defer func() {
		if p := recover(); p != nil {
			if ctx.Err() != nil {
				err = ErrTimeout
				return
			}
			err = fmt.Errorf("handler crashed: %v", p)
		}
	}()

	rt, err := qjs.New(qjs.Option{
		Context:            ctx,
		CloseOnContextDone: true,
		CWD:                r.emptyDir,
		CacheDir:           r.cacheDir,
		MemoryLimit:        fnMemoryLimit,
		MaxStackSize:       fnStackLimit,
	})
	if err != nil {
		return resp, err
	}
	defer func() {
		defer func() { _ = recover() }()
		rt.Close()
	}()
	jsctx := rt.Context()

	reqJSON, err := json.Marshal(req)
	if err != nil {
		return resp, err
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return resp, err
	}
	if env == nil {
		envJSON = []byte("{}")
	}
	jsctx.Global().SetPropertyStr("__req", jsctx.NewString(string(reqJSON)))
	jsctx.Global().SetPropertyStr("__env", jsctx.NewString(string(envJSON)))
	jsctx.SetFunc("__kv", func(this *qjs.This) (*qjs.Value, error) {
		return kvCall(this, kv, app)
	})

	for _, step := range []struct{ file, code string }{{"prelude.js", fnPrelude}, {"handler.js", src}} {
		v, err := jsctx.Eval(step.file, qjs.Code(step.code))
		if err != nil {
			return resp, err
		}
		v.Free()
	}

	p, err := jsctx.Eval("invoke.js", qjs.Code(fnInvoke))
	if err != nil {
		return resp, err
	}
	if !p.IsPromise() {
		return parseFnResponse(p.String())
	}
	out, err := p.Await()
	if err != nil {
		return resp, err
	}
	return parseFnResponse(out.String())
}

func kvCall(this *qjs.This, kv kvBackend, app string) (*qjs.Value, error) {
	c := this.Context()
	args := this.Args()
	if kv == nil || len(args) == 0 {
		return c.NewNull(), nil
	}
	op := args[0].String()
	arg := func(i int) string {
		if i < len(args) {
			return args[i].String()
		}
		return ""
	}
	switch op {
	case "list":
		all, err := kv.KVAll(app)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(all)
		if err != nil {
			return nil, err
		}
		return c.NewString(string(b)), nil
	case "get":
		all, err := kv.KVAll(app)
		if err != nil {
			return nil, err
		}
		v, ok := all[arg(1)]
		if !ok {
			return c.NewNull(), nil
		}
		return c.NewString(string(v)), nil
	case "set":
		val := json.RawMessage(arg(2))
		if len(val) > maxKVBody || !json.Valid(val) {
			return nil, errors.New("kv.set: value must be JSON under 1 MB")
		}
		return c.NewNull(), kv.KVSet(app, arg(1), val)
	case "delete":
		return c.NewNull(), kv.KVSet(app, arg(1), nil)
	}
	return nil, fmt.Errorf("kv: unknown op %q", op)
}

var allowedFnHeaders = map[string]bool{"Content-Type": true, "Cache-Control": true, "Location": true}

// parseFnResponse maps handler output to an HTTP response. An object with a "status",
// "headers" or "body" key is a structured response; anything else is returned as JSON.
func parseFnResponse(out string) (FnResponse, error) {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal([]byte(out), &obj)
	_, hasStatus := obj["status"]
	_, hasHeaders := obj["headers"]
	body, hasBody := obj["body"]
	if !hasStatus && !hasHeaders && !hasBody {
		return FnResponse{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: []byte(out)}, nil
	}

	resp := FnResponse{Status: 200, Headers: map[string]string{}}
	if hasStatus {
		if err := json.Unmarshal(obj["status"], &resp.Status); err != nil || resp.Status < 200 || resp.Status > 599 {
			return resp, errors.New("handler returned an invalid status")
		}
	}
	if hasHeaders {
		var h map[string]string
		if err := json.Unmarshal(obj["headers"], &h); err != nil {
			return resp, errors.New("handler headers must be strings")
		}
		for k, v := range h {
			if allowedFnHeaders[k] {
				resp.Headers[k] = v
			}
		}
	}
	var s string
	switch {
	case !hasBody:
	case json.Unmarshal(body, &s) == nil:
		resp.Body = []byte(s)
		if resp.Headers["Content-Type"] == "" {
			resp.Headers["Content-Type"] = "text/plain; charset=utf-8"
		}
	default:
		resp.Body = body
		if resp.Headers["Content-Type"] == "" {
			resp.Headers["Content-Type"] = "application/json"
		}
	}
	return resp, nil
}
