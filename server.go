package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed pages.html
var pagesHTML string

//go:embed tokens.css
var tokensCSS string

var funcs = template.FuncMap{
	"cloud":  cloudSVG,
	"ago":    ago,
	"access": accessSummary,
}

var (
	dashboardTmpl = template.Must(template.New("dash").Funcs(funcs).Parse(dashboardHTML))
	pagesTmpl     = template.Must(template.New("pages").Funcs(funcs).Parse(pagesHTML))
)

const maxKVBody = 1 << 20

var notices = map[string]string{
	"emailed":  "Invite sent. They'll get a one-time link by email.",
	"nomail":   "Added. Email isn't set up on this server, so copy their sign-in link and send it yourself.",
	"mailfail": "Added, but the email didn't send. Copy their sign-in link and send it yourself.",
	"removed":  "Removed. They're signed out everywhere.",
	"rolled":   "Rolled back to the previous version.",
	"noprev":   "There is no earlier version to go back to.",
	"noorg":    "Set an organization domain on the server first (nephos serve -org-domain acme.com).",
}

type Config struct {
	PublicURL  string
	Domain     string
	AdminToken string
	// OrgDomains are email domains that count as "the organization", e.g. acme.com.
	OrgDomains []string
	// OrgOnly refuses invitations to addresses outside OrgDomains.
	OrgOnly bool
}

type Server struct {
	store      *Store
	auth       *AuthStore
	fn         *Runner
	mailer     Mailer
	limit      *limiter
	adminToken string
	publicURL  *url.URL
	domain     string
	secure     bool
	orgDomains []string
	orgOnly    bool
	mux        *http.ServeMux
}

func NewServer(store *Store, auth *AuthStore, fn *Runner, mailer Mailer, cfg Config) (*Server, error) {
	u, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return nil, err
	}
	s := &Server{
		store: store, auth: auth, fn: fn, mailer: mailer, limit: newLimiter(),
		adminToken: cfg.AdminToken, publicURL: u, domain: cfg.Domain, secure: u.Scheme == "https",
		orgOnly: cfg.OrgOnly && len(cfg.OrgDomains) > 0,
	}
	for _, d := range cfg.OrgDomains {
		if d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(d, "@"))); d != "" {
			s.orgDomains = append(s.orgDomains, d)
		}
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /{$}", s.admin(s.dashboard))
	s.mux.HandleFunc("GET /login", s.adminLogin)
	s.mux.HandleFunc("GET /api/apps", s.admin(s.listApps))
	s.mux.HandleFunc("POST /deploy", s.admin(s.deploy))
	s.mux.HandleFunc("GET /open/{name}", s.admin(s.open))
	s.mux.HandleFunc("POST /apps/{name}/share", s.admin(s.share))
	s.mux.HandleFunc("POST /apps/{name}/link", s.admin(s.link))
	s.mux.HandleFunc("POST /apps/{name}/revoke", s.admin(s.revoke))
	s.mux.HandleFunc("POST /apps/{name}/access", s.admin(s.setAccess))
	s.mux.HandleFunc("POST /apps/{name}/env", s.admin(s.setEnv))
	s.mux.HandleFunc("POST /apps/{name}/rollback", s.admin(s.rollback))
	s.mux.HandleFunc("GET /api/config", s.admin(s.config))
	s.mux.HandleFunc("POST /apps/{name}/delete", s.admin(s.deleteApp))
	s.mux.HandleFunc("GET /_internal/tls-ask", s.tlsAsk)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if name, ok := strings.CutSuffix(host, "."+s.domain); ok {
		s.serveApp(w, r, name)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) appURL(name, path string) string {
	return fmt.Sprintf("%s://%s.%s%s", s.publicURL.Scheme, name, s.publicURL.Host, path)
}

func (s *Server) joinURL(app, token string) string {
	return s.appURL(app, "/_nephos/join?t="+token)
}

// cookieName adds the __Host- prefix over https, which browsers only accept for
// Secure, Path=/, host-only cookies, so a sibling subdomain can't overwrite it.
func (s *Server) cookieName(base string) string {
	if s.secure {
		return "__Host-" + base
	}
	return base
}

func (s *Server) setCookie(w http.ResponseWriter, base, value string, maxAge int, site http.SameSite) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(base), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secure, SameSite: site,
	})
}

func (s *Server) isAdmin(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		if c, err := r.Cookie(s.cookieName("nephos_admin")); err == nil {
			tok = c.Value
		}
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) == 1
}

func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			http.Error(w, "unauthorized: open /login?token=<admin token>", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) != 1 {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	s.setCookie(w, "nephos_admin", tok, 0, http.SameSiteStrictMode)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type personView struct {
	Email    string    `json:"email"`
	Added    time.Time `json:"added"`
	LastSeen time.Time `json:"lastSeen"`
}

type appView struct {
	Name        string       `json:"name"`
	Access      Access       `json:"access"`
	URL         string       `json:"url"`
	HasHandler  bool         `json:"hasHandler"`
	Deployed    time.Time    `json:"deployed"`
	People      []personView `json:"people"`
	EnvKeys     []string     `json:"envKeys"`
	CanRollback bool         `json:"canRollback"`
}

func (s *Server) view(a *App) appView {
	v := appView{Name: a.Name, Access: a.Access, URL: s.appURL(a.Name, "/"), Deployed: a.Deployed, People: []personView{}, EnvKeys: []string{}, CanRollback: s.store.CanRollback(a.Name)}
	for k := range a.Env {
		v.EnvKeys = append(v.EnvKeys, k)
	}
	sort.Strings(v.EnvKeys)
	if _, err := os.Stat(filepath.Join(s.store.SiteDir(a.Name), "handler.js")); err == nil {
		v.HasHandler = true
	}
	for _, p := range a.People {
		v.People = append(v.People, personView{Email: p.Email, Added: p.Added, LastSeen: p.LastSeen})
	}
	return v
}

func (s *Server) views() ([]appView, error) {
	apps, err := s.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]appView, len(apps))
	for i, a := range apps {
		out[i] = s.view(a)
	}
	return out, nil
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	vs, err := s.views()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, vs)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	vs, err := s.views()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := struct {
		Tokens   template.CSS
		Apps     []appView
		Selected *appView
		EmailOn  bool
		Notice   string
		Org      string
	}{Tokens: template.CSS(tokensCSS), Apps: vs, EmailOn: s.mailer.Delivers(), Notice: notices[r.URL.Query().Get("notice")], Org: s.orgLabel()}
	want := r.URL.Query().Get("app")
	for i := range vs {
		if vs[i].Name == want || (data.Selected == nil && want == "") {
			data.Selected = &vs[i]
		}
	}
	if data.Selected == nil && len(vs) > 0 {
		data.Selected = &vs[0]
	}
	if err := dashboardTmpl.Execute(w, data); err != nil {
		log.Println("dashboard:", err)
	}
}

// done answers a mutation: 204 for API clients, back to the app's page for forms.
func done(w http.ResponseWriter, r *http.Request, app, notice string) {
	if r.Header.Get("Accept") == "application/json" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q := url.Values{}
	if app != "" {
		q.Set("app", app)
	}
	if notice != "" {
		q.Set("notice", notice)
	}
	target := "/"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSiteBytes))
	if err != nil {
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		return
	}
	a, err := s.store.Deploy(name, body)
	if err != nil {
		code := http.StatusUnprocessableEntity
		if errors.Is(err, ErrInvalidName) {
			code = http.StatusBadRequest
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, map[string]string{"name": a.Name, "url": s.appURL(a.Name, "/")})
}

// open signs the owner in to an app with a short-lived direct link.
func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.store.Get(name); err != nil {
		http.NotFound(w, r)
		return
	}
	tok, err := s.auth.NewMagic(name, OwnerEmail, directLinkTTL, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, s.joinURL(name, tok), http.StatusSeeOther)
}

type inviteResult struct {
	Email      string `json:"email"`
	Link       string `json:"link"`
	Emailed    bool   `json:"emailed"`
	EmailError string `json:"emailError,omitempty"`
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	email, err := NormalizeEmail(r.FormValue("email"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.orgOnly && !s.orgMatch(email) {
		http.Error(w, "this server only allows invitations to "+s.orgLabel()+" addresses", http.StatusForbidden)
		return
	}
	if _, err := s.store.Invite(name, email); errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tok, err := s.auth.NewMagic(name, email, inviteLinkTTL, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res := inviteResult{Email: email, Link: s.joinURL(name, tok)}
	sendErr := s.mailer.Send(r.Context(), inviteMail(email, name, s.appURL(name, "/"), res.Link))
	res.Emailed = s.mailer.Delivers() && sendErr == nil
	if sendErr != nil {
		res.EmailError = sendErr.Error()
		log.Printf("invite email to %s: %v", email, sendErr)
	}

	if r.Header.Get("Accept") == "application/json" {
		writeJSON(w, res)
		return
	}
	notice := "nomail"
	switch {
	case res.Emailed:
		notice = "emailed"
	case sendErr != nil:
		notice = "mailfail"
	}
	done(w, r, name, notice)
}

// link mints a fresh sign-in link for someone already invited, to send by hand.
func (s *Server) link(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	email, err := NormalizeEmail(r.FormValue("email"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, err := s.store.Get(name)
	if err != nil || !a.HasPerson(email) {
		http.NotFound(w, r)
		return
	}
	tok, err := s.auth.NewMagic(name, email, inviteLinkTTL, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"link": s.joinURL(name, tok)})
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	email, err := NormalizeEmail(r.FormValue("email"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.Revoke(name, email); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := s.auth.Drop(name, email); err != nil {
		log.Printf("drop sessions for %s on %s: %v", email, name, err)
	}
	done(w, r, name, "removed")
}

func (s *Server) setAccess(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	access, err := ParseAccess(r.FormValue("access"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if access == AccessOrg && len(s.orgDomains) == 0 {
		if r.Header.Get("Accept") == "application/json" {
			http.Error(w, "no organization domain is configured on this server", http.StatusBadRequest)
			return
		}
		done(w, r, name, "noorg")
		return
	}
	if _, err := s.store.Update(name, func(a *App) { a.Access = access }); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	done(w, r, name, "")
}

// setEnv stores or removes one setting. Values are write-only: nothing ever returns them.
func (s *Server) setEnv(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch err := s.store.SetEnv(name, r.FormValue("key"), r.FormValue("value")); {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	done(w, r, name, "")
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	switch _, err := s.store.Rollback(name); {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, ErrNoPrevious):
		if r.Header.Get("Accept") == "application/json" {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		done(w, r, name, "noprev")
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	done(w, r, name, "rolled")
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"orgDomains": s.orgDomains, "orgOnly": s.orgOnly, "emailDelivers": s.mailer.Delivers()})
}

func (s *Server) orgLabel() string { return strings.Join(s.orgDomains, " or ") }

// orgMatch reports whether email belongs to one of the organization's domains.
func (s *Server) orgMatch(email string) bool {
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return false
	}
	for _, d := range s.orgDomains {
		if domain == d {
			return true
		}
	}
	return false
}

// mayOpen is the one place that decides whether an identity can open an app.
func (s *Server) mayOpen(a *App, email string) bool {
	return email == OwnerEmail || a.HasPerson(email) || (a.Access == AccessOrg && s.orgMatch(email))
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.store.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := s.auth.Drop(name, ""); err != nil {
		log.Printf("drop sessions for %s: %v", name, err)
	}
	done(w, r, "", "")
}

// tlsAsk tells the reverse proxy which hostnames may get a certificate: the
// dashboard host and one subdomain per deployed app. It answers only the proxy
// itself (private address, no X-Forwarded-For), never traffic relayed from the internet.
func (s *Server) tlsAsk(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Forwarded-For") != "" || !fromPrivate(r) {
		http.NotFound(w, r)
		return
	}
	d := strings.ToLower(r.URL.Query().Get("domain"))
	if d == strings.ToLower(s.publicURL.Hostname()) {
		return
	}
	if name, ok := strings.CutSuffix(d, "."+s.domain); ok {
		if _, err := s.store.Get(name); err == nil {
			return
		}
	}
	http.NotFound(w, r)
}

func fromPrivate(r *http.Request) bool {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	return ip.IsLoopback() || ip.IsPrivate()
}

// clientIP trusts X-Forwarded-For only when the request came from our own proxy.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" && fromPrivate(r) {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// viewer resolves who is asking. email is "" for anonymous visitors of public apps.
// People removed from an app lose access at once, even with a live session.
func (s *Server) viewer(r *http.Request, a *App) (email string, ok bool) {
	if c, err := r.Cookie(s.cookieName("nephos")); err == nil {
		if sess, found := s.auth.Session(c.Value); found && sess.App == a.Name && s.mayOpen(a, sess.Email) {
			return sess.Email, true
		}
	}
	return "", a.Access == AccessPublic
}

type pageData struct {
	Tokens template.CSS
	Cloud  template.HTML
	App    string
	Email  string
	Token  string
	Notice string
	Mode   string
}

func (s *Server) page(w http.ResponseWriter, status int, app string, d pageData) {
	d.Tokens, d.Cloud, d.App = template.CSS(tokensCSS), cloudSVG(app), app
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := pagesTmpl.Execute(w, d); err != nil {
		log.Println("page:", err)
	}
}

// sameOrigin rejects cross-site form posts so other sites can't trigger sign-in emails.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	}
	return true
}

func (s *Server) serveApp(w http.ResponseWriter, r *http.Request, name string) {
	a, err := s.store.Get(name)
	if err != nil {
		http.Error(w, "no such app", http.StatusNotFound)
		return
	}

	switch r.URL.Path {
	case "/_nephos/join":
		s.join(w, r, a)
		return
	case "/_nephos/login":
		s.appLogin(w, r, a)
		return
	}

	email, ok := s.viewer(r, a)
	if !ok {
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			s.page(w, http.StatusUnauthorized, a.Name, pageData{Mode: "login"})
			return
		}
		http.Error(w, "sign in required", http.StatusUnauthorized)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/_nephos/") {
		s.appAPI(w, r, a, email)
		return
	}
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		s.runHandler(w, r, a, email)
		return
	}
	if r.URL.Path == "/handler.js" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.FileServer(http.Dir(s.store.SiteDir(a.Name))).ServeHTTP(w, r)
}

const linkUsedNotice = "That link has expired or was already used. Enter your email to get a new one."

// join handles the link from an email. GET only shows who is about to sign in,
// because mail scanners open links to check them and would burn a single-use link.
// POST uses the link up and starts the session. Direct links from the dashboard skip the page.
func (s *Server) join(w http.ResponseWriter, r *http.Request, a *App) {
	if r.Method == http.MethodPost {
		if !sameOrigin(r) {
			http.Error(w, "cross-site request", http.StatusForbidden)
			return
		}
		rec, ok := s.auth.ConsumeMagic(r.FormValue("t"))
		if !ok || rec.App != a.Name {
			s.page(w, http.StatusGone, a.Name, pageData{Mode: "login", Notice: linkUsedNotice})
			return
		}
		s.startSession(w, r, a, rec.Email)
		return
	}

	tok := r.URL.Query().Get("t")
	rec, ok := s.auth.PeekMagic(tok)
	if !ok || rec.App != a.Name {
		s.page(w, http.StatusGone, a.Name, pageData{Mode: "login", Notice: linkUsedNotice})
		return
	}
	if rec.Direct {
		if _, ok := s.auth.ConsumeMagic(tok); ok {
			s.startSession(w, r, a, rec.Email)
			return
		}
	}
	s.page(w, http.StatusOK, a.Name, pageData{Mode: "confirm", Email: displayEmail(rec.Email), Token: tok})
}

func displayEmail(email string) string {
	if email == OwnerEmail {
		return "the owner"
	}
	return email
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, a *App, email string) {
	if !s.mayOpen(a, email) {
		s.page(w, http.StatusForbidden, a.Name, pageData{Mode: "login", Notice: "That address no longer has access to this app."})
		return
	}
	tok, err := s.auth.NewSession(a.Name, email)
	if err != nil {
		http.Error(w, "could not sign in", http.StatusInternalServerError)
		return
	}
	if email != OwnerEmail {
		if err := s.store.Touch(a.Name, email); err != nil {
			log.Printf("touch %s on %s: %v", email, a.Name, err)
		}
	}
	s.setCookie(w, "nephos", tok, int(sessionTTL/time.Second), http.SameSiteLaxMode)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// appLogin sends a sign-in link to an invited address. The reply is identical
// whether or not the address has access, so it can't be used to list who does.
func (s *Server) appLogin(w http.ResponseWriter, r *http.Request, a *App) {
	if r.Method != http.MethodPost {
		s.page(w, http.StatusOK, a.Name, pageData{Mode: "login"})
		return
	}
	if !sameOrigin(r) {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	email, err := NormalizeEmail(r.FormValue("email"))
	if err != nil {
		s.page(w, http.StatusBadRequest, a.Name, pageData{Mode: "login", Notice: err.Error()})
		return
	}
	if !s.limit.Allow("ip:"+clientIP(r), maxLoginPerIP, time.Hour) || !s.limit.Allow("email:"+a.Name+":"+email, maxLoginPerKey, loginLinkTTL) {
		s.page(w, http.StatusTooManyRequests, a.Name, pageData{Mode: "login", Notice: "Too many tries. Wait a few minutes and try again."})
		return
	}
	if s.mayOpen(a, email) {
		go s.sendLoginLink(a.Name, email)
	}
	s.page(w, http.StatusOK, a.Name, pageData{Mode: "sent", Email: email})
}

func (s *Server) sendLoginLink(app, email string) {
	tok, err := s.auth.NewMagic(app, email, loginLinkTTL, false)
	if err != nil {
		log.Printf("login link for %s on %s: %v", email, app, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.mailer.Send(ctx, loginMail(email, app, s.joinURL(app, tok))); err != nil {
		log.Printf("login email to %s: %v", email, err)
	}
}

func (s *Server) appAPI(w http.ResponseWriter, r *http.Request, a *App, email string) {
	path := strings.TrimPrefix(r.URL.Path, "/_nephos/")
	switch {
	case path == "me" && r.Method == http.MethodGet:
		writeJSON(w, map[string]string{"email": email, "app": a.Name})

	case path == "kv" && r.Method == http.MethodGet:
		kv, err := s.store.KVAll(a.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, kv)

	case strings.HasPrefix(path, "kv/"):
		s.kvKey(w, r, a, email, strings.TrimPrefix(path, "kv/"))

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) kvKey(w http.ResponseWriter, r *http.Request, a *App, email, key string) {
	if key == "" || len(key) > 200 {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodGet && email == "" {
		http.Error(w, "writes need a signed-in person", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		kv, err := s.store.KVAll(a.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		v, ok := kv[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(v)
	case http.MethodPut:
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKVBody))
		if err != nil || !json.Valid(b) {
			http.Error(w, "body must be valid JSON under 1 MB", http.StatusBadRequest)
			return
		}
		if err := s.store.KVSet(a.Name, key, b); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if err := s.store.KVSet(a.Name, key, nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) runHandler(w http.ResponseWriter, r *http.Request, a *App, email string) {
	src, err := os.ReadFile(filepath.Join(s.store.SiteDir(a.Name), "handler.js"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxKVBody))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	req := FnRequest{
		Method:  r.Method,
		Path:    strings.TrimPrefix(r.URL.Path, "/api"),
		Query:   map[string]string{},
		Headers: map[string]string{},
		Body:    string(body),
		User:    email,
	}
	for k, v := range r.URL.Query() {
		req.Query[k] = v[0]
	}
	for _, h := range []string{"Content-Type", "Accept", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Headers[h] = v
		}
	}

	resp, err := s.fn.Run(r.Context(), string(src), req, s.store, a.Name, a.Env)
	switch {
	case errors.Is(err, ErrTimeout):
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
		return
	case errors.Is(err, ErrBusy):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	case err != nil:
		log.Printf("handler %s: %v", a.Name, err)
		http.Error(w, "handler error (see server log)", http.StatusInternalServerError)
		return
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(resp.Status)
	w.Write(resp.Body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
