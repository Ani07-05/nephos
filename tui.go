package main

import (
	"fmt"
	"image/color"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// banner is the wordmark shown above the lists when the terminal is tall enough.
const banner = `                       _
 _ __    ___   _ __   | |__     ___    ___
| '_ \  / _ \ | '_ \  | '_ \   / _ \  / __|
| | | ||  __/ | |_) | | | | | | (_) | \__ \
|_| |_| \___| | .__/  |_| |_|  \___/  |___/
              |_|`

const bannerMinHeight = 30

const (
	railWidth   = 28
	refreshEach = 5 * time.Second
)

type palette struct {
	sun, ink, muted, link, danger color.Color
}

func newPalette(dark bool) palette {
	ld := lipgloss.LightDark(dark)
	return palette{
		sun:    ld(lipgloss.Color("#9A6A00"), lipgloss.Color("#EDB02E")),
		ink:    ld(lipgloss.Color("#14222E"), lipgloss.Color("#E6EEF4")),
		muted:  ld(lipgloss.Color("#566B7C"), lipgloss.Color("#8FA4B4")),
		link:   ld(lipgloss.Color("#1F5A8C"), lipgloss.Color("#8CC0EA")),
		danger: ld(lipgloss.Color("#B3392E"), lipgloss.Color("#EF8478")),
	}
}

type (
	appsMsg struct {
		apps []appView
		err  error
	}
	doneMsg struct {
		status string
		err    error
	}
	configMsg struct{ orgDomains []string }
	tickMsg   time.Time
)

type pane int

const (
	paneApps pane = iota
	panePeople
)

type mode int

const (
	modeBrowse mode = iota
	modeInvite
	modeDelete
)

type tuiModel struct {
	c      *client
	apps   []appView
	sel    int
	person int
	pane   pane
	mode   mode
	input  textinput.Model
	org    []string
	status string
	failed bool
	w, h   int
	pal    palette
}

func tuiCmd() error {
	c, err := newClient()
	if err != nil {
		return err
	}
	in := textinput.New()
	in.Prompt = "Invite: "
	in.Placeholder = "name@example.com"
	in.CharLimit = 120
	_, err = tea.NewProgram(tuiModel{c: c, input: in, pal: newPalette(true)}).Run()
	return err
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.fetchConfig(), m.fetch(), m.tick())
}

func (m tuiModel) tick() tea.Cmd {
	return tea.Tick(refreshEach, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m tuiModel) fetchConfig() tea.Cmd {
	c := m.c
	return func() tea.Msg {
		var cfg struct {
			OrgDomains []string `json:"orgDomains"`
		}
		_ = c.request(http.MethodGet, "/api/config", "", nil, &cfg)
		return configMsg{cfg.OrgDomains}
	}
}

func (m tuiModel) orgLabel() string { return strings.Join(m.org, " or ") }

func (m tuiModel) fetch() tea.Cmd {
	c := m.c
	return func() tea.Msg {
		var apps []appView
		err := c.request(http.MethodGet, "/api/apps", "", nil, &apps)
		return appsMsg{apps, err}
	}
}

func (m tuiModel) current() *appView {
	if m.sel < 0 || m.sel >= len(m.apps) {
		return nil
	}
	return &m.apps[m.sel]
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.BackgroundColorMsg:
		m.pal = newPalette(msg.IsDark())
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(m.fetch(), m.tick())
	case configMsg:
		m.org = msg.orgDomains
	case appsMsg:
		return m.applyApps(msg), nil
	case doneMsg:
		m.status, m.failed = msg.status, msg.err != nil
		if msg.err != nil {
			m.status = msg.err.Error()
		}
		return m, m.fetch()
	case tea.KeyPressMsg:
		switch m.mode {
		case modeInvite:
			return m.updateInvite(msg)
		case modeDelete:
			return m.updateDelete(msg)
		}
		return m.updateBrowse(msg)
	}
	return m, nil
}

func (m tuiModel) applyApps(msg appsMsg) tuiModel {
	if msg.err != nil {
		m.status, m.failed = "Can't reach the server: "+msg.err.Error(), true
		return m
	}
	name := ""
	if cur := m.current(); cur != nil {
		name = cur.Name
	}
	m.apps = msg.apps
	m.sel = 0
	for i, a := range m.apps {
		if a.Name == name {
			m.sel = i
		}
	}
	if cur := m.current(); cur != nil && m.person >= len(cur.People) {
		m.person = max(len(cur.People)-1, 0)
	}
	if m.failed && strings.HasPrefix(m.status, "Can't reach") {
		m.status, m.failed = "", false
	}
	return m
}

func (m tuiModel) updateBrowse(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	cur := m.current()
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "r":
		return m, m.fetch()
	case "tab", "left", "right", "h", "l":
		if m.pane == paneApps && cur != nil && len(cur.People) > 0 {
			m.pane = panePeople
		} else {
			m.pane = paneApps
		}
	case "up", "k":
		m = m.move(-1)
	case "down", "j":
		m = m.move(1)
	}
	if cur == nil {
		return m, nil
	}
	switch msg.String() {
	case "enter", "o":
		return m, m.openApp(cur.Name)
	case "i":
		m.mode = modeInvite
		m.input.Reset()
		return m, m.input.Focus()
	case "p":
		next := m.nextAccess(cur.Access)
		return m, m.act(cur.Name, "access", url.Values{"access": {string(next)}}, "Now: "+accessSummary(appView{Access: next}, m.orgLabel()))
	case "b":
		return m, m.act(cur.Name, "rollback", nil, "Rolled back "+cur.Name+" to the previous version")
	case "d":
		m.mode = modeDelete
	case "c":
		if m.pane == panePeople && m.person < len(cur.People) {
			return m, m.copyLink(cur.Name, cur.People[m.person].Email)
		}
		return m, tea.Batch(tea.SetClipboard(cur.URL), func() tea.Msg { return doneMsg{status: "Copied the address"} })
	case "x":
		if m.pane == panePeople && m.person < len(cur.People) {
			p := cur.People[m.person]
			return m, m.act(cur.Name, "revoke", url.Values{"email": {p.Email}}, "Removed "+p.Email)
		}
		m.status, m.failed = "Switch to the people list (tab) to remove someone", true
	}
	return m, nil
}

// nextAccess steps invited -> org -> public, skipping org when the server has none.
func (m tuiModel) nextAccess(a Access) Access {
	switch {
	case a == AccessInvited && len(m.org) > 0:
		return AccessOrg
	case a == AccessInvited || a == AccessOrg:
		return AccessPublic
	}
	return AccessInvited
}

func (m tuiModel) move(delta int) tuiModel {
	cur := m.current()
	if cur == nil {
		return m
	}
	if m.pane == panePeople {
		m.person = clamp(m.person+delta, 0, len(cur.People)-1)
		return m
	}
	m.sel = clamp(m.sel+delta, 0, len(m.apps)-1)
	m.person = 0
	return m
}

func (m tuiModel) updateInvite(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = modeBrowse
		m.input.Blur()
		return m, nil
	case "enter":
		email := strings.TrimSpace(m.input.Value())
		cur := m.current()
		m.mode = modeBrowse
		m.input.Blur()
		if cur == nil || !strings.Contains(email, "@") {
			m.status, m.failed = "Enter a full email address", true
			return m, nil
		}
		return m, m.invite(cur.Name, email)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m tuiModel) updateDelete(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	m.mode = modeBrowse
	if msg.String() != "y" {
		return m, nil
	}
	cur := m.current()
	if cur == nil {
		return m, nil
	}
	m.sel, m.person, m.pane = 0, 0, paneApps
	return m, m.act(cur.Name, "delete", nil, "Deleted "+cur.Name)
}

func (m tuiModel) act(app, verb string, form url.Values, ok string) tea.Cmd {
	c := m.c
	return func() tea.Msg {
		err := c.post("/apps/"+url.PathEscape(app)+"/"+verb, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
		return doneMsg{status: ok, err: err}
	}
}

func (m tuiModel) invite(app, email string) tea.Cmd {
	c := m.c
	return func() tea.Msg {
		var out inviteResult
		form := url.Values{"email": {email}}.Encode()
		if err := c.post("/apps/"+url.PathEscape(app)+"/share", "application/x-www-form-urlencoded", strings.NewReader(form), &out); err != nil {
			return doneMsg{err: err}
		}
		if out.Emailed {
			return doneMsg{status: "Invited " + out.Email + ". They were emailed a one-time link."}
		}
		return tea.BatchMsg{
			tea.SetClipboard(out.Link),
			func() tea.Msg {
				return doneMsg{status: "Invited " + out.Email + ". Email isn't set up, so their sign-in link is copied: send it to them."}
			},
		}
	}
}

func (m tuiModel) copyLink(app, email string) tea.Cmd {
	c := m.c
	return func() tea.Msg {
		var out struct{ Link string }
		form := url.Values{"email": {email}}.Encode()
		if err := c.post("/apps/"+url.PathEscape(app)+"/link", "application/x-www-form-urlencoded", strings.NewReader(form), &out); err != nil {
			return doneMsg{err: err}
		}
		return tea.BatchMsg{
			tea.SetClipboard(out.Link),
			func() tea.Msg { return doneMsg{status: "Copied a sign-in link for " + email} },
		}
	}
}

// openApp asks the server for an owner sign-in link without following the redirect,
// then hands that link to the system browser.
func (m tuiModel) openApp(name string) tea.Cmd {
	c := m.c
	return func() tea.Msg {
		req, err := http.NewRequest(http.MethodGet, c.base+"/open/"+url.PathEscape(name), nil)
		if err != nil {
			return doneMsg{err: err}
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return doneMsg{err: err}
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if loc == "" {
			return doneMsg{err: fmt.Errorf("server did not return a link: %s", resp.Status)}
		}
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		if err := exec.Command(opener, loc).Start(); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{status: "Opened " + name + " in your browser"}
	}
}

func (m tuiModel) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "nephos"
	return v
}

func (m tuiModel) render() string {
	p := m.pal
	sun := lipgloss.NewStyle().Bold(true).Foreground(p.sun)
	where := lipgloss.NewStyle().Foreground(p.muted).Render(m.c.base)
	header, chrome := " "+sun.Render("nephos")+"  "+where, 7
	if m.h >= bannerMinHeight {
		header, chrome = sun.Render(banner)+"\n "+where, 12
	}

	rightW := max(m.w-railWidth-3, 30)
	rule := lipgloss.NewStyle().Foreground(p.muted).Render(strings.TrimSuffix(strings.Repeat("│\n", max(m.h-chrome, 4)), "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(railWidth).Render(m.renderRail()),
		rule,
		lipgloss.NewStyle().Width(rightW).PaddingLeft(2).Render(m.renderDetail()),
	)

	status := ""
	if m.status != "" {
		c := p.ink
		if m.failed {
			c = p.danger
		}
		status = lipgloss.NewStyle().Foreground(c).Render(m.status)
	}
	return lipgloss.JoinVertical(lipgloss.Left, "", header, "", body, "", " "+status, " "+m.help())
}

func (m tuiModel) renderRail() string {
	p := m.pal
	if len(m.apps) == 0 {
		return lipgloss.NewStyle().Foreground(p.muted).Render(" No apps yet")
	}
	var b strings.Builder
	for i, a := range m.apps {
		name := lipgloss.NewStyle().Foreground(p.ink)
		marker := "  "
		if i == m.sel {
			name = name.Bold(true)
			marker = lipgloss.NewStyle().Foreground(p.sun).Render("▌ ")
		}
		fmt.Fprintf(&b, "%s%s\n", marker, name.Render(a.Name))
		fmt.Fprintf(&b, "  %s\n", lipgloss.NewStyle().Foreground(p.muted).Render(accessSummary(a, m.orgLabel())))
	}
	return b.String()
}

func (m tuiModel) renderDetail() string {
	p := m.pal
	cur := m.current()
	muted := lipgloss.NewStyle().Foreground(p.muted)
	if cur == nil {
		return "Nothing deployed yet.\n\n" + muted.Render("Deploy a folder that has an index.html:") + "\n  nephos deploy ./my-app"
	}

	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(p.sun).Render(cur.Name) + "\n")
	b.WriteString(lipgloss.NewStyle().Foreground(p.link).Underline(true).Render(cur.URL) + "\n")
	facts := "Deployed " + ago(cur.Deployed) + "."
	if cur.HasHandler {
		facts += " Runs server code."
	}
	if len(cur.EnvKeys) > 0 {
		facts += " Settings: " + strings.Join(cur.EnvKeys, ", ") + "."
	}
	b.WriteString(muted.Render(facts) + "\n\n")

	switch cur.Access {
	case AccessPublic:
		b.WriteString("Anyone with the address can open it.\n")
	case AccessOrg:
		b.WriteString("Anyone at " + m.orgLabel() + " can open it, plus the people below.\n")
	default:
		b.WriteString("Only you and the people below can open it.\n")
	}
	b.WriteString(muted.Render("p changes this") + "\n\n")

	if len(cur.People) == 0 {
		b.WriteString(muted.Render("Nobody invited yet. Press i to add an email.") + "\n")
	}
	for i, person := range cur.People {
		line := "  " + person.Email
		if m.pane == panePeople && i == m.person {
			line = lipgloss.NewStyle().Foreground(p.sun).Render("▌ ") + lipgloss.NewStyle().Bold(true).Render(person.Email)
		}
		b.WriteString(line + "  " + muted.Render(personStatus(person)) + "\n")
	}

	switch m.mode {
	case modeInvite:
		b.WriteString("\n" + m.input.View() + "\n")
	case modeDelete:
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(p.danger).Render("Delete "+cur.Name+" and everything it saved? y to confirm, any other key to cancel") + "\n")
	}
	return b.String()
}

func (m tuiModel) help() string {
	muted := lipgloss.NewStyle().Foreground(m.pal.muted).MaxWidth(max(m.w-2, 20))
	switch m.mode {
	case modeInvite:
		return muted.Render("enter invite   esc cancel")
	case modeDelete:
		return muted.Render("y delete   any other key cancel")
	}
	return muted.Render("j/k move   tab switch list   o open   i invite   c copy link   x remove   p access   b roll back   d delete   q quit")
}

func personStatus(p personView) string {
	if p.LastSeen.IsZero() {
		return "invited " + ago(p.Added)
	}
	return "opened " + ago(p.LastSeen)
}

func accessSummary(a appView, org string) string {
	switch {
	case a.Access == AccessPublic:
		return "Anyone with the address"
	case a.Access == AccessOrg:
		return "Anyone at " + org
	case len(a.People) == 0:
		return "Only you"
	case len(a.People) == 1:
		return "You and 1 person"
	}
	return fmt.Sprintf("You and %d people", len(a.People))
}

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}
