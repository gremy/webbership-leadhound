package console

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// shellProduct is the product each signed-in GET page belongs to, by its sample path. It is
// written out rather than worked out with productOf, so a wrong productOf fails here; a new
// GET route fails TestEveryPageRendersTheShell until it is added.
var shellProduct = map[string]string{
	"/":                 "",
	"/leads/home":       "leads",
	"/jobs/home":        "jobs",
	"/account":          "",
	"/controls":         "",
	"/status":           "",
	"/jobs":             "jobs",
	"/tracked":          "jobs",
	"/runs":             "jobs",
	"/jobs/42":          "jobs",
	"/rules":            "jobs",
	"/results":          "jobs",
	"/leads":            "leads",
	"/leads/outreach":   "leads",
	"/leads/settings":   "",
	"/leads/campaigns":  "leads",
	"/leads/today":      "leads",
	"/leads/drafts":     "leads",
	"/leads/calls/9":    "leads",
	"/leads/batches/3":  "leads",
	"/leads/5":          "leads",
	"/mailboxes":        "",
	"/mailboxes/new":    "",
	"/mailboxes/1":      "",
	"/mailboxes/1/edit": "",
}

// notPages are the signed-in GETs that answer with something other than a console page: a
// redirect, an htmx fragment, or a report download.
var notPages = []string{"/leads/next", "/leads/priorities", "/run/status", "/mailboxes/1/check", "/leads/reports/5"}

// TestEveryPageRendersTheShell: every signed-in GET route's sample path answers 200 inside
// the shell (switcher, theme button, gear menu with Sign out), with the switcher marking its
// product and the sub-nav showing that product's links only.
func TestEveryPageRendersTheShell(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	seen := map[string]bool{}
	for _, rt := range routes {
		if rt.public || !strings.HasPrefix(rt.pattern, "GET ") {
			continue
		}
		seen[rt.sample] = true
		if slices.Contains(notPages, rt.sample) {
			continue
		}
		want, ok := shellProduct[rt.sample]
		if !ok {
			t.Errorf("%s: add it to shellProduct", rt.sample)
			continue
		}
		t.Run(rt.sample, func(t *testing.T) {
			w := h.do(get(rt.sample, c))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			body := w.Body.String()
			for _, part := range []string{`<ul class="switcher">`, `<details class="gear">`, `<form class="theme-form" method="post" action="/theme">`,
				`<form class="signout" method="post" action="/logout">`, `href="/account"`} {
				if !strings.Contains(body, part) {
					t.Errorf("lacks %q", part)
				}
			}
			leadsOn := strings.Contains(body, `<a href="/leads/home" aria-current=`)
			jobsOn := strings.Contains(body, `<a href="/jobs/home" aria-current=`)
			if leadsOn != (want == "leads") || jobsOn != (want == "jobs") {
				t.Errorf("switcher marks leads=%v jobs=%v, want product %q", leadsOn, jobsOn, want)
			}
			subnav := strings.Contains(body, `<nav class="subnav"`)
			if subnav != (want != "") {
				t.Errorf("sub-nav shown=%v for product %q", subnav, want)
			}
			if want == "leads" && strings.Contains(body, `<a href="/tracked"`) || want == "jobs" && strings.Contains(body, `<a href="/leads/drafts"`) {
				t.Error("the sub-nav shows the other product's links")
			}
		})
	}
	for p := range shellProduct {
		if !seen[p] {
			t.Errorf("shellProduct names %s, which no GET route samples", p)
		}
	}
}

// TestShellCurrentLink: the sub-nav and gear mark the page itself "page" and a page inside a
// link (a lead under List, a batch under Outreach, a call under Today) "true".
func TestShellCurrentLink(t *testing.T) {
	for _, tt := range []struct{ path, label, want string }{
		{"/leads/home", "Home", "page"},
		{"/leads", "List", "page"},
		{"/leads/5", "List", "true"},
		{"/leads/batches/3", "Outreach", "true"},
		{"/leads/calls/9", "Today", "true"},
		{"/leads/reports/5", "List", "true"},
		{"/jobs/42", "Jobs", "true"},
		{"/jobs/home", "Home", "page"},
	} {
		links := leadsNav
		if productOf(tt.path) == "jobs" {
			links = jobsNav
		}
		var marked []string
		for _, l := range current(links, tt.path) {
			if l.Current != "" {
				marked = append(marked, l.Label+"="+l.Current)
			}
		}
		if want := []string{tt.label + "=" + tt.want}; !slices.Equal(marked, want) {
			t.Errorf("%s: marked %v, want %v", tt.path, marked, want)
		}
	}
	for _, tt := range []struct{ path, label, want string }{
		{"/mailboxes", "Mailboxes", "page"}, {"/mailboxes/1", "Mailboxes", "true"},
		{"/controls", "Controls", "page"}, {"/account", "Account", "page"}, {"/leads/settings", "Settings", "page"},
	} {
		for _, l := range current(gearNav, tt.path) {
			if (l.Label == tt.label) != (l.Current == tt.want) {
				t.Errorf("%s: %s is %q", tt.path, l.Label, l.Current)
			}
		}
	}
}

// TestThemeButtons: the theme control is one form with two submit buttons, Dark and Light,
// on the sign-in page too, and the stylesheet shows only the one that flips the theme on
// screen.
func TestThemeButtons(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	for _, p := range []string{"/login", "/"} {
		var cookies []*http.Cookie
		if p != "/login" {
			cookies = append(cookies, h.session(t))
		}
		body := h.do(get(p, cookies...)).Body.String()
		if n := strings.Count(body, `action="/theme"`); n != 1 {
			t.Errorf("%s: %d theme forms", p, n)
		}
		for _, want := range []string{
			`<button class="icon-btn theme-to-dark" type="submit" name="theme" value="dark" aria-label="Switch to dark theme">`,
			`<button class="icon-btn theme-to-light" type="submit" name="theme" value="light" aria-label="Switch to light theme">`,
			`<input type="hidden" name="back" value="` + p + `">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", p, want)
			}
		}
	}
	css, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	if o, c := strings.Count(string(css), "{"), strings.Count(string(css), "}"); o != c {
		t.Errorf("console.css has %d { and %d }: a rule is cut off", o, c)
	}
	for _, rule := range []string{
		".theme-form .theme-to-light {\n  display: none;",
		":root[data-theme=\"dark\"] .theme-form .theme-to-light {\n  display: inline-flex;",
		":root[data-theme=\"dark\"] .theme-form .theme-to-dark {\n  display: none;",
		"  :root:not([data-theme=\"light\"]) .theme-form .theme-to-light {\n    display: inline-flex;",
		"  :root:not([data-theme=\"light\"]) .theme-form .theme-to-dark {\n    display: none;",
	} {
		if !strings.Contains(string(css), rule) {
			t.Errorf("console.css lacks %q", rule)
		}
	}
}

// TestConsoleJS: every page loads console.js with its version pin, and the file is served
// as JavaScript.
func TestConsoleJS(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	body := h.do(get("/", h.session(t))).Body.String()
	if len(jsVersion) != 8 || !strings.Contains(body, `<script src="/static/console.js?v=`+jsVersion+`" defer></script>`) {
		t.Errorf("the page does not load console.js with its version %q", jsVersion)
	}
	w := h.do(get("/static/console.js?v=" + jsVersion))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "javascript") ||
		!strings.Contains(w.Body.String(), `details.gear[open]`) {
		t.Errorf("console.js: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

// TestAccountShowsOperator: the gear menu and the Account page name the operator, and
// Account offers Sign out.
func TestAccountShowsOperator(t *testing.T) {
	a, _, _ := testAuth(t)
	hh := New(richBackend("x"), a, Options{Operator: "operator@mail.example"})
	tok, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{h: hh, name: "__Host-lh_session"}
	w := h.do(get("/account", h.cookie(tok)))
	body := w.Body.String()
	if w.Code != http.StatusOK || strings.Count(body, "operator@mail.example") != 2 || strings.Count(body, `action="/logout"`) != 2 {
		t.Errorf("account: %d, operator named %d times, sign-out forms %d", w.Code,
			strings.Count(body, "operator@mail.example"), strings.Count(body, `action="/logout"`))
	}
}
