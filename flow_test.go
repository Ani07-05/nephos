package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	return buf.Bytes()
}

// signIn walks the emailed-link flow for email and returns the session cookie.
func (g *rig) signIn(host, email string) *http.Cookie {
	g.t.Helper()
	g.do("POST", host, "/_nephos/login", url.Values{"email": {email}})
	tok := linkToken(g.t, g.mailer.next(g.t).Text)
	return sessionCookie(g.t, g.do("POST", host, "/_nephos/join", url.Values{"t": {tok}}))
}

func TestOrgAccessLetsAnyoneAtTheDomainIn(t *testing.T) {
	g := newRig(t, "http://localhost:8080", withOrg("acme.com"))

	if rec := g.admin("POST", "/apps/counter/access", url.Values{"access": {"org"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set access: %d %s", rec.Code, rec.Body)
	}
	carol := g.signIn(appHost, "Carol@Acme.com")
	if rec := g.do("GET", appHost, "/", nil, withCookie(carol)); rec.Code != http.StatusOK {
		t.Fatalf("org colleague who was never invited: %d", rec.Code)
	}

	g.do("POST", appHost, "/_nephos/login", url.Values{"email": {"eve@other.com"}})
	g.mailer.none(t)

	g.admin("POST", "/apps/counter/access", url.Values{"access": {"invited"}})
	if rec := g.do("GET", appHost, "/", nil, withCookie(carol)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("narrowing access must end org sessions: %d", rec.Code)
	}
}

func TestOrgAccessNeedsAConfiguredDomain(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	rec := g.do("POST", "localhost:8080", "/apps/counter/access", url.Values{"access": {"org"}}, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer admin-secret")
		r.Header.Set("Accept", "application/json")
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestOrgOnlyRefusesOutsideInvites(t *testing.T) {
	g := newRig(t, "http://localhost:8080", withOrg("acme.com"), withOrgOnly)
	if rec := g.admin("POST", "/apps/counter/share", url.Values{"email": {"eve@other.com"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("outside invite: %d", rec.Code)
	}
	g.mailer.none(t)
	if rec := g.admin("POST", "/apps/counter/share", url.Values{"email": {"dana@acme.com"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("org invite: %d", rec.Code)
	}
}

func TestSettingsAreWriteOnly(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	g.admin("POST", "/apps/counter/env", url.Values{"key": {"API_KEY"}, "value": {"s3cret-value"}})
	body := g.admin("GET", "/api/apps", nil).Body.String()
	if !strings.Contains(body, "API_KEY") || strings.Contains(body, "s3cret-value") {
		t.Fatalf("api leaked or lost settings: %s", body)
	}
	if rec := g.admin("POST", "/apps/counter/env", url.Values{"key": {"lower"}, "value": {"x"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad key: %d", rec.Code)
	}
}

func TestRollbackSwapsVersions(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	store := g.srv.store
	read := func() string {
		b, _ := os.ReadFile(store.SiteDir("counter") + "/index.html")
		return string(b)
	}
	if _, err := store.Rollback("counter"); err != ErrNoPrevious {
		t.Fatalf("fresh app: %v", err)
	}
	store.Deploy("counter", zipOf(t, map[string]string{"index.html": "v2"}))
	if read() != "v2" {
		t.Fatalf("deploy: %q", read())
	}
	store.Rollback("counter")
	if got := read(); !strings.Contains(got, "hello counter") {
		t.Fatalf("rollback: %q", got)
	}
	store.Rollback("counter")
	if read() != "v2" {
		t.Fatalf("roll forward: %q", read())
	}
}

// mcpCall runs one tool and decodes its structured result.
func mcpCall[T any](t *testing.T, cs *mcp.ClientSession, tool string, args any) T {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s failed: %+v", tool, res.Content)
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s result %s: %v", tool, b, err)
	}
	return out
}

// TestAgentBuildsAppAndColleagueUsesIt is the whole story end to end: an AI agent, through
// MCP only, builds an app with server code and a secret, deploys it, opens it to the
// organization and invites someone. Colleagues then sign in by email and use it.
func TestAgentBuildsAppAndColleagueUsesIt(t *testing.T) {
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	g := newRig(t, fmt.Sprintf("http://localhost:%d", port), withOrg("acme.com"), withRunner(t))
	ts.Config.Handler = g.srv
	ts.Start()
	defer ts.Close()
	host := fmt.Sprintf("standup.localhost:%d", port)

	ctx := context.Background()
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(&client{base: ts.URL, token: "admin-secret"}).Connect(ctx, t1, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); strings.Contains(got, "delete") || len(names) != 6 {
		t.Fatalf("agent tools: %s", got)
	}

	v1 := map[string]string{
		"index.html": `<h1>standup</h1><script>fetch('/api/').then(r=>r.json())</script>`,
		"handler.js": `async function handle(req) {
			if (req.method === 'POST') {
				const notes = kv.get('notes') || [];
				notes.push({ by: req.user, text: JSON.parse(req.body).text });
				kv.set('notes', notes);
			}
			return { team: env.TEAM, notes: kv.get('notes') || [] };
		}`,
	}
	dep := mcpCall[deployOut](t, cs, "deploy_app", deployIn{Name: "standup", Files: v1})
	if !strings.HasPrefix(dep.URL, "http://standup.localhost:") {
		t.Fatalf("deploy: %+v", dep)
	}
	mcpCall[noteOut](t, cs, "set_env", envIn{Name: "standup", Key: "TEAM", Value: "Platform"})
	mcpCall[noteOut](t, cs, "set_access", accessIn{Name: "standup", Access: "org"})
	shared := mcpCall[shareOut](t, cs, "share_app", shareIn{Name: "standup", Email: "dana@acme.com"})
	if !shared.Emailed || shared.Link != "" {
		t.Fatalf("share: %+v", shared)
	}

	if rec := g.do("GET", host, "/", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("stranger saw the app: %d", rec.Code)
	}

	dana := sessionCookie(t, g.do("POST", host, "/_nephos/join", url.Values{"t": {linkToken(t, g.mailer.next(t).Text)}}))
	post := g.do("POST", host, "/api/", nil, withCookie(dana), func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"text":"shipped the importer"}`))
		r.ContentLength = -1
	})
	if post.Code != http.StatusOK || !strings.Contains(post.Body.String(), `"team":"Platform"`) || !strings.Contains(post.Body.String(), `"by":"dana@acme.com"`) {
		t.Fatalf("dana's note: %d %s", post.Code, post.Body)
	}

	sam := g.signIn(host, "sam@acme.com")
	seen := g.do("GET", host, "/api/", nil, withCookie(sam))
	if !strings.Contains(seen.Body.String(), "shipped the importer") {
		t.Fatalf("sam should see dana's note: %s", seen.Body)
	}

	g.do("POST", host, "/_nephos/login", url.Values{"email": {"eve@other.com"}})
	g.mailer.none(t)

	bad := map[string]string{"index.html": "<h1>oops</h1>", "handler.js": "function handle(){ throw new Error('broken') }"}
	mcpCall[deployOut](t, cs, "deploy_app", deployIn{Name: "standup", Files: bad})
	if rec := g.do("GET", host, "/api/", nil, withCookie(sam)); rec.Code != http.StatusInternalServerError {
		t.Fatalf("broken deploy should fail: %d", rec.Code)
	}
	mcpCall[noteOut](t, cs, "rollback_app", nameIn{Name: "standup"})
	if rec := g.do("GET", host, "/api/", nil, withCookie(sam)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "shipped the importer") {
		t.Fatalf("after rollback: %d %s", rec.Code, rec.Body)
	}

	apps := mcpCall[listOut](t, cs, "list_apps", listIn{})
	if len(apps.Apps) < 1 {
		t.Fatalf("list: %+v", apps)
	}
}

// Pasted commands often carry a sentence's trailing period or a leading @.
func TestOrgDomainToleratesStrayPunctuation(t *testing.T) {
	g := newRig(t, "http://localhost:8080", withOrg("@Acme.com."))
	g.admin("POST", "/apps/counter/access", url.Values{"access": {"org"}})
	carol := g.signIn(appHost, "carol@acme.com")
	if rec := g.do("GET", appHost, "/", nil, withCookie(carol)); rec.Code != http.StatusOK {
		t.Fatalf("org member refused: %d", rec.Code)
	}
}
