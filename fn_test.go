package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type memKV struct {
	mu sync.Mutex
	m  map[string]json.RawMessage
}

func (k *memKV) KVAll(string) (map[string]json.RawMessage, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]json.RawMessage{}
	for key, v := range k.m {
		out[key] = v
	}
	return out, nil
}

func (k *memKV) KVSet(_, key string, v json.RawMessage) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if v == nil {
		delete(k.m, key)
	} else {
		k.m[key] = v
	}
	return nil
}

func newTestRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := NewRunner(t.TempDir(), 4, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHandlerKVAndUser(t *testing.T) {
	r := newTestRunner(t)
	kv := &memKV{m: map[string]json.RawMessage{}}
	src := `async function handle(req) {
		const n = (kv.get('hits:' + req.user) || 0) + 1;
		kv.set('hits:' + req.user, n);
		return { status: 201, body: { user: req.user, n, path: req.path } };
	}`
	for want := 1; want <= 2; want++ {
		resp, err := r.Run(context.Background(), src, FnRequest{Path: "/x", User: "a@b.c"}, kv, "app", nil)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			User string
			N    int
			Path string
		}
		if err := json.Unmarshal(resp.Body, &got); err != nil {
			t.Fatal(err)
		}
		if resp.Status != 201 || got.N != want || got.User != "a@b.c" || got.Path != "/x" {
			t.Fatalf("resp=%+v body=%s", resp, resp.Body)
		}
	}
}

func TestHandlerPlainValueIsJSON(t *testing.T) {
	r := newTestRunner(t)
	resp, err := r.Run(context.Background(), `function handle(){ return [1,2] }`, FnRequest{}, nil, "", nil)
	if err != nil || resp.Status != 200 || string(resp.Body) != "[1,2]" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestHandlerInfiniteLoopTimesOut(t *testing.T) {
	r := newTestRunner(t)
	start := time.Now()
	_, err := r.Run(context.Background(), `function handle(){ while(true){} }`, FnRequest{}, nil, "", nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	if _, err := r.Run(context.Background(), `function handle(){ return 1 }`, FnRequest{}, nil, "", nil); err != nil {
		t.Fatalf("runner unusable after timeout: %v", err)
	}
}

func TestHandlerCannotSeeHostFilesystem(t *testing.T) {
	r := newTestRunner(t)
	for _, src := range []string{
		`function handle(){ return typeof os + typeof std }`,
		`function handle(){ return typeof fetch + typeof process + typeof require + typeof XMLHttpRequest }`,
	} {
		resp, err := r.Run(context.Background(), src, FnRequest{}, nil, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(resp.Body), "object") || strings.Contains(string(resp.Body), "function") {
			t.Fatalf("%s leaked: %s", src, resp.Body)
		}
	}
	resp, err := r.Run(context.Background(), `async function handle(){ const m = await import('os'); return m.readdir('.')[0] }`, FnRequest{}, nil, "", nil)
	if err == nil && strings.Contains(string(resp.Body), "go.mod") {
		t.Fatalf("host cwd visible: %s", resp.Body)
	}
}

func TestHandlerThrowIsError(t *testing.T) {
	r := newTestRunner(t)
	_, err := r.Run(context.Background(), `function handle(){ throw new Error('boom') }`, FnRequest{}, nil, "", nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
}

func TestHandlerHeaderAllowlist(t *testing.T) {
	r := newTestRunner(t)
	resp, err := r.Run(context.Background(), `function handle(){ return {headers:{'Set-Cookie':'x=1','Content-Type':'text/html'}, body:'hi'} }`, FnRequest{}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := resp.Headers["Set-Cookie"]; bad || resp.Headers["Content-Type"] != "text/html" || string(resp.Body) != "hi" {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestHandlerReadsSettingsAsFrozenEnv(t *testing.T) {
	r := newTestRunner(t)
	src := `function handle(){ try { env.TEAM = 'x' } catch (e) {} return { team: env.TEAM, missing: env.NOPE === undefined } }`
	resp, err := r.Run(context.Background(), src, FnRequest{}, nil, "", map[string]string{"TEAM": "Platform"})
	if err != nil || string(resp.Body) != `{"team":"Platform","missing":true}` {
		t.Fatalf("resp=%s err=%v", resp.Body, err)
	}
}
