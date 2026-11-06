package main

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type captureMailer struct {
	mu   sync.Mutex
	sent chan Mail
}

func newCaptureMailer() *captureMailer { return &captureMailer{sent: make(chan Mail, 16)} }

func (c *captureMailer) Delivers() bool { return true }
func (c *captureMailer) Send(_ context.Context, m Mail) error {
	c.sent <- m
	return nil
}

func (c *captureMailer) next(t *testing.T) Mail {
	t.Helper()
	select {
	case m := <-c.sent:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no email was sent")
		return Mail{}
	}
}

func (c *captureMailer) none(t *testing.T) {
	t.Helper()
	select {
	case m := <-c.sent:
		t.Fatalf("unexpected email to %s", m.To)
	case <-time.After(150 * time.Millisecond):
	}
}

type rig struct {
	t      *testing.T
	srv    *Server
	mailer *captureMailer
}

type rigSetup struct {
	cfg    Config
	runner *Runner
}

func withOrg(domains ...string) func(*rigSetup) {
	return func(r *rigSetup) { r.cfg.OrgDomains = domains }
}

func withOrgOnly(r *rigSetup) { r.cfg.OrgOnly = true }

func withRunner(t *testing.T) func(*rigSetup) {
	return func(r *rigSetup) { r.runner = newTestRunner(t) }
}

func newRig(t *testing.T, publicURL string, opts ...func(*rigSetup)) *rig {
	t.Helper()
	setup := rigSetup{cfg: Config{PublicURL: publicURL, Domain: "localhost", AdminToken: "admin-secret"}}
	for _, o := range opts {
		o(&setup)
	}
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := NewAuthStore(dir + "/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	m := newCaptureMailer()
	srv, err := NewServer(store, auth, setup.runner, m, setup.cfg)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("index.html")
	w.Write([]byte("<h1>hello counter</h1>"))
	zw.Close()
	if _, err := store.Deploy("counter", buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, srv: srv, mailer: m}
}

func (g *rig) do(method, host, path string, form url.Values, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	g.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = host
	req.RemoteAddr = "203.0.113.9:4000"
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "text/html")
	for _, f := range mutate {
		f(req)
	}
	rec := httptest.NewRecorder()
	g.srv.ServeHTTP(rec, req)
	return rec
}

func (g *rig) admin(method, path string, form url.Values) *httptest.ResponseRecorder {
	return g.do(method, "localhost:8080", path, form, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer admin-secret")
	})
}

func withCookie(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) { r.AddCookie(c) }
}

const appHost = "counter.localhost:8080"

var tokenRe = regexp.MustCompile(`t=([0-9a-f]+)`)

func linkToken(t *testing.T, text string) string {
	t.Helper()
	m := tokenRe.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no sign-in link in %q", text)
	}
	return m[1]
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, "nephos") {
			return c
		}
	}
	t.Fatalf("no session cookie, status %d", rec.Code)
	return nil
}

func TestInviteSignInFlow(t *testing.T) {
	g := newRig(t, "http://localhost:8080")

	if rec := g.do("GET", appHost, "/", nil); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "is private") {
		t.Fatalf("anonymous: %d %s", rec.Code, rec.Body)
	}

	if rec := g.admin("POST", "/apps/counter/share", url.Values{"email": {"  Bob@Example.com "}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	mail := g.mailer.next(t)
	if mail.To != "bob@example.com" {
		t.Fatalf("emailed %q", mail.To)
	}
	tok := linkToken(t, mail.Text)
	joinPath := "/_nephos/join?t=" + tok

	for i := 0; i < 2; i++ {
		rec := g.do("GET", appHost, joinPath, nil)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "bob@example.com") || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("scanner GET #%d must only show the confirm page: %d", i, rec.Code)
		}
	}

	post := g.do("POST", appHost, "/_nephos/join", url.Values{"t": {tok}})
	if post.Code != http.StatusSeeOther {
		t.Fatalf("confirm: %d %s", post.Code, post.Body)
	}
	cookie := sessionCookie(t, post)

	rec := g.do("GET", appHost, "/", nil, withCookie(cookie))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello counter") {
		t.Fatalf("signed in: %d %s", rec.Code, rec.Body)
	}
	if rec := g.do("GET", appHost, "/_nephos/me", nil, withCookie(cookie)); !strings.Contains(rec.Body.String(), "bob@example.com") {
		t.Fatalf("me: %s", rec.Body)
	}

	if rec := g.do("POST", appHost, "/_nephos/join", url.Values{"t": {tok}}); rec.Code != http.StatusGone {
		t.Fatalf("link reuse: %d", rec.Code)
	}

	g.admin("POST", "/apps/counter/revoke", url.Values{"email": {"bob@example.com"}})
	if rec := g.do("GET", appHost, "/", nil, withCookie(cookie)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("removed person kept access: %d", rec.Code)
	}
}

func TestLoginLinkRequest(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	g.admin("POST", "/apps/counter/share", url.Values{"email": {"bob@example.com"}})
	g.mailer.next(t)

	known := g.do("POST", appHost, "/_nephos/login", url.Values{"email": {"bob@example.com"}})
	loginMail := g.mailer.next(t)
	unknown := g.do("POST", appHost, "/_nephos/login", url.Values{"email": {"mallory@example.com"}})
	g.mailer.none(t)

	if known.Code != http.StatusOK || unknown.Code != http.StatusOK {
		t.Fatalf("codes %d %d", known.Code, unknown.Code)
	}
	strip := func(s, email string) string { return strings.ReplaceAll(s, email, "EMAIL") }
	if strip(known.Body.String(), "bob@example.com") != strip(unknown.Body.String(), "mallory@example.com") {
		t.Fatal("reply differs for known and unknown addresses")
	}
	if !strings.Contains(loginMail.Subject, "counter") {
		t.Fatalf("subject %q", loginMail.Subject)
	}
}

func TestLoginRateLimit(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	var last int
	for i := 0; i <= maxLoginPerKey; i++ {
		last = g.do("POST", appHost, "/_nephos/login", url.Values{"email": {"bob@example.com"}}).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("attempt %d got %d", maxLoginPerKey+1, last)
	}
}

func TestCrossSiteSignInPostRejected(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	rec := g.do("POST", appHost, "/_nephos/login", url.Values{"email": {"bob@example.com"}}, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
	g.mailer.none(t)
}

func TestOwnerOpenLink(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	rec := g.admin("GET", "/open/counter", nil)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "http://counter.localhost:8080/_nephos/join?t=") {
		t.Fatalf("open: %d %s", rec.Code, loc)
	}
	u, _ := url.Parse(loc)
	join := g.do("GET", appHost, u.RequestURI(), nil)
	if join.Code != http.StatusSeeOther {
		t.Fatalf("direct link should sign in at once: %d", join.Code)
	}
	if rec := g.do("GET", appHost, "/", nil, withCookie(sessionCookie(t, join))); rec.Code != http.StatusOK {
		t.Fatalf("owner session: %d", rec.Code)
	}
	if rec := g.do("GET", appHost, u.RequestURI(), nil); rec.Code != http.StatusGone {
		t.Fatalf("direct link reuse: %d", rec.Code)
	}
}

func TestHTTPSCookiesAreHostPrefixedAndSecure(t *testing.T) {
	g := newRig(t, "https://localhost")
	loc := g.admin("GET", "/open/counter", nil).Header().Get("Location")
	u, _ := url.Parse(loc)
	c := sessionCookie(t, g.do("GET", "counter.localhost", u.RequestURI(), nil))
	if c.Name != "__Host-nephos" || !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
		t.Fatalf("cookie %+v", c)
	}
}

func TestTLSAsk(t *testing.T) {
	g := newRig(t, "https://localhost")
	ask := func(domain string, mutate ...func(*http.Request)) int {
		m := append([]func(*http.Request){func(r *http.Request) { r.RemoteAddr = "172.18.0.2:5555" }}, mutate...)
		return g.do("GET", "localhost", "/_internal/tls-ask?domain="+domain, nil, m...).Code
	}
	if c := ask("counter.localhost"); c != http.StatusOK {
		t.Fatalf("deployed app: %d", c)
	}
	if c := ask("localhost"); c != http.StatusOK {
		t.Fatalf("dashboard host: %d", c)
	}
	if c := ask("ghost.localhost"); c != http.StatusNotFound {
		t.Fatalf("unknown app: %d", c)
	}
	if c := ask("counter.localhost", func(r *http.Request) { r.Header.Set("X-Forwarded-For", "198.51.100.1") }); c != http.StatusNotFound {
		t.Fatalf("relayed from internet: %d", c)
	}
	if c := ask("counter.localhost", func(r *http.Request) { r.RemoteAddr = "198.51.100.7:1" }); c != http.StatusNotFound {
		t.Fatalf("public source: %d", c)
	}
}

func TestMagicLinkExpiryAndSingleUse(t *testing.T) {
	a, err := NewAuthStore(t.TempDir() + "/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	a.now = func() time.Time { return clock }

	tok, _ := a.NewMagic("counter", "bob@example.com", time.Minute, false)
	if _, ok := a.ConsumeMagic(tok); !ok {
		t.Fatal("fresh link rejected")
	}
	if _, ok := a.ConsumeMagic(tok); ok {
		t.Fatal("link worked twice")
	}

	tok, _ = a.NewMagic("counter", "bob@example.com", time.Minute, false)
	clock = clock.Add(2 * time.Minute)
	if _, ok := a.ConsumeMagic(tok); ok {
		t.Fatal("expired link accepted")
	}
}

func TestAuthStoreKeepsOnlyHashes(t *testing.T) {
	dir := t.TempDir()
	a, _ := NewAuthStore(dir + "/auth.json")
	magic, _ := a.NewMagic("counter", "bob@example.com", time.Hour, false)
	sess, _ := a.NewSession("counter", "bob@example.com")

	raw, err := readFile(dir + "/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, magic) || strings.Contains(raw, sess) {
		t.Fatal("plaintext token on disk")
	}
	b, _ := NewAuthStore(dir + "/auth.json")
	if _, ok := b.Session(sess); !ok {
		t.Fatal("session lost across restart")
	}
}

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{"Bob@Example.COM": "bob@example.com", "  a@b.co ": "a@b.co"} {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "bob", "bob@", "Bob <bob@example.com>", "a@b.co, c@d.co"} {
		if _, err := NormalizeEmail(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// Browsers send "Origin: null" on form posts when the page's referrer policy is no-referrer,
// so the sign-in pages must use same-origin. Origin null itself stays refused.
func TestSignInPagesKeepBrowserOriginOnFormPosts(t *testing.T) {
	g := newRig(t, "http://localhost:8080")
	page := g.do("GET", appHost, "/", nil)
	if got := page.Header().Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy %q would make browsers send Origin: null", got)
	}
	if !strings.Contains(page.Body.String(), `<meta name="referrer" content="same-origin">`) {
		t.Fatal("page meta referrer policy must be same-origin")
	}

	realBrowser := func(r *http.Request) {
		r.Header.Set("Origin", "http://"+appHost)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if rec := g.do("POST", appHost, "/_nephos/login", url.Values{"email": {"bob@example.com"}}, realBrowser); rec.Code != http.StatusOK {
		t.Fatalf("same-origin form post refused: %d", rec.Code)
	}
	null := func(r *http.Request) { r.Header.Set("Origin", "null") }
	if rec := g.do("POST", appHost, "/_nephos/login", url.Values{"email": {"bob@example.com"}}, null); rec.Code != http.StatusForbidden {
		t.Fatalf("Origin: null must stay refused, got %d", rec.Code)
	}
}

func TestParseDotEnv(t *testing.T) {
	got := parseDotEnv("# mail\nNEPHOS_SMTP=smtp.gmail.com:587\n\nexport NEPHOS_SMTP_USER = me@gmail.com \nNEPHOS_MAIL_FROM=\"nephos <me@gmail.com>\"\nNEPHOS_SMTP_PASS='abcd efgh'\nnot a setting\n")
	want := map[string]string{
		"NEPHOS_SMTP":      "smtp.gmail.com:587",
		"NEPHOS_SMTP_USER": "me@gmail.com",
		"NEPHOS_MAIL_FROM": "nephos <me@gmail.com>",
		"NEPHOS_SMTP_PASS": "abcd efgh",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
