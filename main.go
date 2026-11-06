package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `nephos - a cloud for small software

  nephos serve  [-lan] [-addr :8080] [-data ./data] [-url http://localhost:8080] [-domain localhost]
  nephos deploy <dir> [name]
  nephos share  <name> <email>
  nephos access <name> invited|org|public
  nephos env    <name> [KEY=value ...]   set settings handlers read as env.KEY; KEY= removes one
  nephos rollback <name>
  nephos mailtest <you@example.com>      send a real test email with your SMTP settings
  nephos tui
  nephos mcp                             MCP server on stdin/stdout, so an AI agent can deploy and share
client env: NEPHOS_URL (default http://localhost:8080), NEPHOS_TOKEN (default ./data/admin_token)`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "deploy":
		err = deployCmd(os.Args[2:])
	case "share":
		err = shareCmd(os.Args[2:])
	case "tui":
		err = tuiCmd()
	case "access":
		err = accessCmd(os.Args[2:])
	case "env":
		err = envCmd(os.Args[2:])
	case "rollback":
		err = rollbackCmd(os.Args[2:])
	case "mcp":
		err = mcpCmd()
	case "mailtest":
		err = mailtestCmd(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	data := fs.String("data", "./data", "data directory")
	pub := fs.String("url", "http://localhost:8080", "public base URL; apps live at <name>.<host>")
	domain := fs.String("domain", "localhost", "base domain for app subdomains")
	smtpAddr := fs.String("smtp", os.Getenv("NEPHOS_SMTP"), "SMTP server host:port (587 STARTTLS or 465 TLS); empty prints emails to the log")
	mailFrom := fs.String("mail-from", os.Getenv("NEPHOS_MAIL_FROM"), `sender address, e.g. "nephos <hello@example.com>"`)
	orgDomains := fs.String("org-domain", os.Getenv("NEPHOS_ORG_DOMAINS"), "email domain(s) that count as your organization, comma separated, e.g. acme.com")
	lan := fs.Bool("lan", false, "share on your local network or tailnet: apps get <name>.<ip>.sslip.io addresses, no domain or server needed")
	lanIP := fs.String("ip", "", "with -lan: address to share on instead of the detected one, e.g. your Tailscale IP")
	orgOnly := fs.Bool("org-only", false, "only allow invitations to -org-domain addresses")
	fs.Parse(args)

	if *lan {
		ip := *lanIP
		if ip == "" {
			var err error
			if ip, err = outboundIP(); err != nil {
				return fmt.Errorf("can't detect this machine's address, pass -ip: %w", err)
			}
		}
		_, port, err := net.SplitHostPort(*addr)
		if err != nil {
			return err
		}
		*domain = strings.ReplaceAll(ip, ".", "-") + ".sslip.io"
		*pub = "http://" + *domain + ":" + port
		log.Printf("sharing on %s: apps are at http://<name>.%s:%s", ip, *domain, port)
	}

	store, err := NewStore(*data)
	if err != nil {
		return err
	}
	tok, err := adminToken(*data)
	if err != nil {
		return err
	}
	runner, err := NewRunner(*data, 4, 2*time.Second)
	if err != nil {
		return err
	}
	auth, err := NewAuthStore(filepath.Join(*data, "auth.json"))
	if err != nil {
		return err
	}
	mailer, err := NewMailer(*smtpAddr, *mailFrom)
	if err != nil {
		return err
	}
	if !mailer.Delivers() {
		log.Print("no SMTP configured: sign-in emails are printed here instead of sent")
	}
	srv, err := NewServer(store, auth, runner, mailer, Config{PublicURL: *pub, Domain: *domain, AdminToken: tok, OrgDomains: splitList(*orgDomains), OrgOnly: *orgOnly})
	if err != nil {
		return err
	}
	log.Printf("dashboard: %s/login?token=%s", strings.TrimRight(*pub, "/"), tok)
	return http.ListenAndServe(*addr, srv)
}

func adminToken(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "admin_token")
	b, err := os.ReadFile(p)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	tok := newToken()
	return tok, os.WriteFile(p, []byte(tok+"\n"), 0o600)
}

type client struct{ base, token string }

func newClient() (*client, error) {
	c := &client{base: strings.TrimRight(envOr("NEPHOS_URL", "http://localhost:8080"), "/"), token: os.Getenv("NEPHOS_TOKEN")}
	if c.token == "" {
		b, err := os.ReadFile("data/admin_token")
		if err != nil {
			return nil, fmt.Errorf("set NEPHOS_TOKEN or run from the server dir: %w", err)
		}
		c.token = strings.TrimSpace(string(b))
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (c *client) post(path, contentType string, body io.Reader, out any) error {
	return c.request(http.MethodPost, path, contentType, body, out)
}

func (c *client) request(method, path, contentType string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func deployCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: nephos deploy <dir> [name]")
	}
	dir := args[0]
	name := filepath.Base(filepath.Clean(dir))
	if len(args) > 1 {
		name = args[1]
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return fmt.Errorf("%s has no index.html", dir)
	}
	zipped, err := zipDir(dir)
	if err != nil {
		return err
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out struct{ Name, URL string }
	if err := c.post("/deploy?name="+url.QueryEscape(name), "application/zip", zipped, &out); err != nil {
		return err
	}
	fmt.Println("deployed", out.Name, "->", out.URL)
	return nil
}

func shareCmd(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: nephos share <name> <email>")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	var out inviteResult
	form := url.Values{"email": {args[1]}}.Encode()
	if err := c.post("/apps/"+url.PathEscape(args[0])+"/share", "application/x-www-form-urlencoded", strings.NewReader(form), &out); err != nil {
		return err
	}
	if out.Emailed {
		fmt.Println("emailed", out.Email, "a one-time sign-in link")
		return nil
	}
	if out.EmailError != "" {
		fmt.Println("invited", out.Email, "but the email failed:", out.EmailError)
	} else {
		fmt.Println("invited", out.Email, "(no SMTP configured, nothing was emailed)")
	}
	fmt.Println("send them this link yourself:\n ", out.Link)
	return nil
}

func zipDir(dir string) (*bytes.Reader, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// outboundIP is the address this machine uses to reach the internet. Dialing UDP sends nothing.
func outboundIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func formPost(c *client, path string, form url.Values, out any) error {
	return c.post(path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), out)
}

func accessCmd(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: nephos access <name> invited|org|public")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := formPost(c, "/apps/"+url.PathEscape(args[0])+"/access", url.Values{"access": {args[1]}}, nil); err != nil {
		return err
	}
	fmt.Println(args[0], "is now", args[1])
	return nil
}

func envCmd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: nephos env <name> [KEY=value ...]")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if len(args) == 1 {
		var apps []appView
		if err := c.request(http.MethodGet, "/api/apps", "", nil, &apps); err != nil {
			return err
		}
		for _, a := range apps {
			if a.Name == args[0] {
				fmt.Println(strings.Join(a.EnvKeys, "\n"))
				return nil
			}
		}
		return fmt.Errorf("no app named %s", args[0])
	}
	for _, kv := range args[1:] {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%q: use KEY=value", kv)
		}
		if err := formPost(c, "/apps/"+url.PathEscape(args[0])+"/env", url.Values{"key": {key}, "value": {value}}, nil); err != nil {
			return err
		}
		fmt.Println("set", key)
	}
	return nil
}

func rollbackCmd(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: nephos rollback <name>")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := formPost(c, "/apps/"+url.PathEscape(args[0])+"/rollback", url.Values{}, nil); err != nil {
		return err
	}
	fmt.Println("rolled", args[0], "back to the previous version")
	return nil
}

// mailtestCmd sends one message through the configured SMTP server, so delivery can be
// checked with real credentials before anyone is invited.
func mailtestCmd(args []string) error {
	fs := flag.NewFlagSet("mailtest", flag.ExitOnError)
	smtpAddr := fs.String("smtp", os.Getenv("NEPHOS_SMTP"), "SMTP server host:port")
	from := fs.String("mail-from", os.Getenv("NEPHOS_MAIL_FROM"), "sender address")
	fs.Parse(args)
	if fs.NArg() != 1 || *smtpAddr == "" {
		return fmt.Errorf("usage: NEPHOS_SMTP=host:587 NEPHOS_SMTP_USER=... NEPHOS_SMTP_PASS=... NEPHOS_MAIL_FROM=... nephos mailtest you@example.com")
	}
	to, err := NormalizeEmail(fs.Arg(0))
	if err != nil {
		return err
	}
	mailer, err := NewMailer(*smtpAddr, *from)
	if err != nil {
		return err
	}
	m := inviteMail(to, "mailtest", "https://example.com/", "https://mailtest.example.com/_nephos/join?t=test")
	m.Subject = "nephos test email"
	if err := mailer.Send(context.Background(), m); err != nil {
		return fmt.Errorf("send failed: %w", err)
	}
	fmt.Println("sent to", to, "- check the inbox (and spam)")
	return nil
}
