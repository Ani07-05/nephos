package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpInstructions = `Nephos hosts small software for a handful of people.

An app is a set of files. It must include index.html. Static files are served as they are.
Optionally add handler.js to run code on the server for requests to /api/*:

  async function handle(req) {            // may also be a plain function
    // req: { method, path, query, headers, body, user }   user is the signed-in person's email
    const n = (kv.get('visits') || 0) + 1; // kv.get/set/delete/list: JSON values, shared by everyone using the app
    kv.set('visits', n);
    return { visits: n, team: env.TEAM };  // env: settings the owner set with set_env, read-only
  }

Return an object for JSON, or { status, headers, body } for control. Handlers have a 2 second limit,
no network and no filesystem. From the browser, call fetch('/api/...'); the page and the API share one origin.
Pages can also use /_nephos/kv/<key> (GET/PUT/DELETE) and /_nephos/me directly.

Apps start private. Use share_app to invite people by email; they get a one-time sign-in link.
Do not put secrets in files: use set_env and read them as env.NAME in handler.js.
Deleting apps and changing server policy is not available to you on purpose.`

type appSummary struct {
	Name       string   `json:"name"`
	URL        string   `json:"url"`
	Access     string   `json:"access" jsonschema:"invited, org or public"`
	People     []string `json:"people" jsonschema:"emails invited to this app"`
	HasHandler bool     `json:"hasHandler"`
	EnvKeys    []string `json:"envKeys" jsonschema:"names of settings, never their values"`
}

type (
	listIn  struct{}
	listOut struct {
		Apps []appSummary `json:"apps"`
	}

	deployIn struct {
		Name  string            `json:"name" jsonschema:"lowercase letters, digits and dashes, up to 32 characters"`
		Files map[string]string `json:"files" jsonschema:"file path to file contents, e.g. {\"index.html\": \"...\", \"handler.js\": \"...\"}; must include index.html"`
	}
	deployOut struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		Note string `json:"note"`
	}

	shareIn struct {
		Name  string `json:"name"`
		Email string `json:"email" jsonschema:"the person to invite"`
	}
	shareOut struct {
		Email   string `json:"email"`
		Emailed bool   `json:"emailed" jsonschema:"true when the server emailed them a one-time sign-in link"`
		Link    string `json:"link,omitempty" jsonschema:"only set when no email was sent: pass this link to the person yourself"`
		Note    string `json:"note"`
	}

	accessIn struct {
		Name   string `json:"name"`
		Access string `json:"access" jsonschema:"invited (only invited people), org (anyone at the organization's email domain) or public (anyone with the address)"`
	}
	envIn struct {
		Name  string `json:"name"`
		Key   string `json:"key" jsonschema:"capitals, digits and underscores, e.g. TEAM_NAME"`
		Value string `json:"value" jsonschema:"empty string removes the setting"`
	}
	nameIn struct {
		Name string `json:"name"`
	}
	noteOut struct {
		Note string `json:"note"`
	}
)

func newMCPServer(c *client) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "nephos", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: mcpInstructions})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_apps",
		Description: "List the apps on this nephos server with their address, who can open them and which settings exist.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listIn) (*mcp.CallToolResult, listOut, error) {
		var apps []appView
		if err := c.request(http.MethodGet, "/api/apps", "", nil, &apps); err != nil {
			return nil, listOut{}, err
		}
		out := listOut{Apps: []appSummary{}}
		for _, a := range apps {
			s := appSummary{Name: a.Name, URL: a.URL, Access: string(a.Access), People: []string{}, HasHandler: a.HasHandler, EnvKeys: a.EnvKeys}
			for _, p := range a.People {
				s.People = append(s.People, p.Email)
			}
			out.Apps = append(out.Apps, s)
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "deploy_app",
		Description: "Publish or update an app from a map of files. Replaces the live files; the previous version is kept for rollback_app. New apps start private.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deployIn) (*mcp.CallToolResult, deployOut, error) {
		zipped, err := zipFiles(in.Files)
		if err != nil {
			return nil, deployOut{}, err
		}
		var out deployOut
		if err := c.post("/deploy?name="+url.QueryEscape(in.Name), "application/zip", zipped, &out); err != nil {
			return nil, deployOut{}, err
		}
		out.Note = "Live at " + out.URL + ". Only you can open it until you call share_app or set_access."
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "share_app",
		Description: "Invite someone to an app by email. They get a one-time link, sign in, and stay signed in for 30 days.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in shareIn) (*mcp.CallToolResult, shareOut, error) {
		var res inviteResult
		if err := formPost(c, "/apps/"+url.PathEscape(in.Name)+"/share", url.Values{"email": {in.Email}}, &res); err != nil {
			return nil, shareOut{}, err
		}
		out := shareOut{Email: res.Email, Emailed: res.Emailed}
		if res.Emailed {
			out.Note = res.Email + " was emailed a one-time sign-in link."
			return nil, out, nil
		}
		out.Link = res.Link
		out.Note = "The server can't send email, so give " + res.Email + " this link yourself. It works once and expires in 7 days."
		if res.EmailError != "" {
			out.Note = "Sending failed (" + res.EmailError + "). Give " + res.Email + " the link yourself. It works once and expires in 7 days."
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_access",
		Description: "Choose who can open an app: invited, org or public. Prefer invited or org for anything internal.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in accessIn) (*mcp.CallToolResult, noteOut, error) {
		if err := formPost(c, "/apps/"+url.PathEscape(in.Name)+"/access", url.Values{"access": {in.Access}}, nil); err != nil {
			return nil, noteOut{}, err
		}
		return nil, noteOut{Note: in.Name + " is now " + in.Access}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_env",
		Description: "Set or remove a setting or secret for an app. Handlers read it as env.KEY. Values can't be read back.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in envIn) (*mcp.CallToolResult, noteOut, error) {
		if err := formPost(c, "/apps/"+url.PathEscape(in.Name)+"/env", url.Values{"key": {in.Key}, "value": {in.Value}}, nil); err != nil {
			return nil, noteOut{}, err
		}
		verb := "Set"
		if in.Value == "" {
			verb = "Removed"
		}
		return nil, noteOut{Note: fmt.Sprintf("%s %s on %s.", verb, in.Key, in.Name)}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "rollback_app",
		Description: "Undo the last deploy by switching back to the previous version. Calling it again switches forward.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nameIn) (*mcp.CallToolResult, noteOut, error) {
		if err := formPost(c, "/apps/"+url.PathEscape(in.Name)+"/rollback", url.Values{}, nil); err != nil {
			return nil, noteOut{}, err
		}
		return nil, noteOut{Note: in.Name + " is back on its previous version."}, nil
	})

	return srv
}

func zipFiles(files map[string]string) (*bytes.Reader, error) {
	if _, ok := files["index.html"]; !ok {
		return nil, errors.New("files must include index.html")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		if !filepath.IsLocal(n) {
			return nil, fmt.Errorf("unsafe file path %q", n)
		}
		names = append(names, n)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(filepath.ToSlash(n))
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

func mcpCmd() error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return newMCPServer(c).Run(context.Background(), &mcp.StdioTransport{})
}
