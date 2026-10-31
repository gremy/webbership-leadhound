package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// themeCookie is the theme cookie as a browser would send it.
func themeCookie(dev bool, value string) *http.Cookie {
	name := "__Host-lh_theme"
	if dev {
		name = "lh_theme"
	}
	return &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode}
}

func TestThemeReadsCookie(t *testing.T) {
	s := &server{themeName: "lh_theme"}
	if got := s.theme(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Errorf("no cookie: %q, want no choice", got)
	}
	for _, tt := range []struct{ cookie, want string }{
		{"light", "light"}, {"dark", "dark"}, {"system", ""}, {"", ""}, {"purple", ""}, {"DARK", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(themeCookie(true, tt.cookie))
		if got := s.theme(r); got != tt.want {
			t.Errorf("cookie %q: %q, want %q", tt.cookie, got, tt.want)
		}
	}
}

func TestThemeSetsCookieAndGoesBack(t *testing.T) {
	for _, dev := range []bool{false, true} {
		h := newHarness(t, richBackend("x"), dev)
		name := themeCookie(dev, "").Name
		for _, tt := range []struct{ theme, want string }{
			{"light", "light"}, {"dark", "dark"}, {"system", ""},
			{"", ""}, {"purple", ""}, {"DARK", ""}, {"dark;x=1", ""},
		} {
			w := h.do(post("/theme", url.Values{"theme": {tt.theme}, "back": {"/jobs?x=1"}}, h.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/jobs?x=1" {
				t.Fatalf("dev=%v theme %q: %d Location %q, want 303 /jobs?x=1", dev, tt.theme, w.Code, w.Header().Get("Location"))
			}
			c := cookieNamed(w, name)
			if c == nil {
				t.Fatalf("dev=%v theme %q: no %s cookie", dev, tt.theme, name)
			}
			if c.Value != tt.want || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure == dev || c.Domain != "" {
				t.Errorf("dev=%v theme %q: cookie %+v, want %q, Path /, HttpOnly, Lax, Secure %v", dev, tt.theme, c, tt.want, !dev)
			}
			// A choice lasts a year; anything else clears the cookie, so the page follows the device.
			if tt.want != "" && c.MaxAge < 360*24*60*60 || tt.want == "" && c.MaxAge >= 0 {
				t.Errorf("dev=%v theme %q: MaxAge %d", dev, tt.theme, c.MaxAge)
			}
		}
	}
}

func TestThemeToggleFlips(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	s := h.session(t)
	for _, v := range []string{"dark", "light", "dark"} {
		c := cookieNamed(h.do(post("/theme", url.Values{"theme": {v}, "back": {"/"}}, s)), "__Host-lh_theme")
		if c == nil || c.Value != v || c.MaxAge <= 0 {
			t.Fatalf("theme %q: cookie %+v", v, c)
		}
	}
}

func TestThemeWorksSignedOut(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	w := h.do(post("/theme", url.Values{"theme": {"dark"}, "back": {"/login?next=%2Fjobs"}}))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next=%2Fjobs" {
		t.Fatalf("signed out: %d Location %q, want 303 back to the sign-in page", w.Code, w.Header().Get("Location"))
	}
	if c := cookieNamed(w, "__Host-lh_theme"); c == nil || c.Value != "dark" {
		t.Errorf("signed out: theme cookie %+v, want dark", c)
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("the backend was called %d times", n)
	}
}

func TestThemeBackIsSameSite(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	for _, back := range []string{
		"//evil.example", "///evil.example", `/\evil.example`, `\\evil.example`,
		"https://evil.example", "https://evil.example/jobs", "http:/jobs", "javascript:alert(1)",
		"evil.example", "jobs", "", "/jobs/../../x", "/%2F%2Fevil.example/../x",
		"/jobs\nSet-Cookie: x=1", "/jobs\t", " /jobs", "/\x7f",
	} {
		w := h.do(post("/theme", url.Values{"theme": {"light"}, "back": {back}}))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Errorf("back %q: %d Location %q, want 303 /", back, w.Code, w.Header().Get("Location"))
		}
	}
	for _, back := range []string{"/", "/leads/today", "/leads/5", "/tracked?status=offer", "/login"} {
		w := h.do(post("/theme", url.Values{"theme": {"light"}, "back": {back}}))
		if w.Header().Get("Location") != back {
			t.Errorf("back %q: Location %q", back, w.Header().Get("Location"))
		}
	}
}

func TestThemeCrossSiteRefused(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	r := post("/theme", url.Values{"theme": {"dark"}, "back": {"/"}})
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := h.do(r)
	if w.Code != http.StatusForbidden || cookieNamed(w, "__Host-lh_theme") != nil {
		t.Errorf("cross-site: %d, cookie set %v; want 403 and no cookie", w.Code, cookieNamed(w, "__Host-lh_theme") != nil)
	}
}

var htmlTag = regexp.MustCompile(`<html[^>]*>`)

// TestLayoutRendersTheme: no choice (no cookie, an old "system" one, junk) renders no
// data-theme, so the CSS follows the device; a choice renders it.
func TestLayoutRendersTheme(t *testing.T) {
	for _, dev := range []bool{false, true} {
		h := newHarness(t, richBackend("x"), dev)
		s := h.session(t)
		for _, tt := range []struct{ cookie, attr string }{
			{"", ""},
			{"system", ""},
			{"purple", ""},
			{"light", ` data-theme="light"`},
			{"dark", ` data-theme="dark"`},
		} {
			for _, p := range []string{"/login", "/", "/leads/today", "/jobs"} {
				cookies := []*http.Cookie{s}
				if p == "/login" {
					cookies = nil
				}
				if tt.cookie != "" {
					cookies = append(cookies, themeCookie(dev, tt.cookie))
				}
				body := h.do(get(p, cookies...)).Body.String()
				if got, want := htmlTag.FindString(body), `<html lang="en"`+tt.attr+`>`; got != want {
					t.Errorf("dev=%v %s cookie %q: %s, want %s", dev, p, tt.cookie, got, want)
				}
				if tt.attr == "" && strings.Contains(body, "data-theme=") {
					t.Errorf("dev=%v %s cookie %q: renders a data-theme with no choice", dev, p, tt.cookie)
				}
				if !strings.Contains(body, `action="/theme"`) {
					t.Fatalf("dev=%v %s: no theme form", dev, p)
				}
				back := `name="back" value="` + p + `"`
				if !strings.Contains(body, back) {
					t.Errorf("dev=%v %s: lacks %s", dev, p, back)
				}
			}
		}
	}
}

func TestThemeBackOnPostRender(t *testing.T) {
	// A page rendered as the answer to a POST goes back to its own page, never to the POST route.
	h := newHarness(t, richBackend("x"), false)
	body := h.do(post("/login", url.Values{"email": {testEmail}, "password": {"wrong password!"}})).Body.String()
	if !strings.Contains(body, `name="back" value="/login"`) {
		t.Errorf("a failed sign-in does not send the theme back to /login")
	}
}

func TestThemeKeepsCSP(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	for _, w := range []*http.Response{
		h.do(post("/theme", url.Values{"theme": {"dark"}, "back": {"/"}})).Result(),
		h.do(get("/login", themeCookie(false, "dark"))).Result(),
		h.do(get("/", h.session(t), themeCookie(false, "light"))).Result(),
	} {
		if got := w.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("CSP %q", got)
		}
	}
	const want = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
		"connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
	if contentSecurityPolicy != want {
		t.Errorf("CSP changed: %q", contentSecurityPolicy)
	}
}

// darkBlock returns the declarations of the first rule for selector in css.
func darkBlock(t *testing.T, css, selector string) string {
	t.Helper()
	i := strings.Index(css, selector+" {")
	if i < 0 {
		t.Fatalf("console.css has no %s rule", selector)
	}
	rest := css[i+len(selector)+2:]
	return strings.TrimSpace(rest[:strings.Index(rest, "}")])
}

func TestCSSHasBothDarkSelectors(t *testing.T) {
	b, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	media := strings.Index(css, "@media (prefers-color-scheme: dark) {\n  :root:not([data-theme=\"light\"]) {")
	if media < 0 {
		t.Fatal(`console.css lacks :root:not([data-theme="light"]) inside @media (prefers-color-scheme: dark)`)
	}
	if n := strings.Count(css, "prefers-color-scheme"); n != 1 {
		t.Errorf("console.css uses prefers-color-scheme %d times, want 1", n)
	}
	sys := darkBlock(t, css, `  :root:not([data-theme="light"])`)
	chosen := darkBlock(t, css, `:root[data-theme="dark"]`)
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ReplaceAll(s, "color-scheme: dark;", "")), " ")
	}
	if norm(sys) != norm(chosen) {
		t.Errorf("the two dark blocks differ:\n%s\n---\n%s", sys, chosen)
	}
	if !strings.Contains(sys, "--bg:") || !strings.Contains(sys, "color-scheme: dark;") || !strings.Contains(chosen, "color-scheme: dark;") {
		t.Error("a dark block lacks the tokens or color-scheme: dark")
	}
	if !strings.Contains(darkBlock(t, css, `:root[data-theme="light"]`), "color-scheme: light;") {
		t.Error(`:root[data-theme="light"] does not set color-scheme: light`)
	}
}

// TestThemeKeepsFlash: the page that showed a flash carries it in the theme switch, and the
// switch sets it again, so the line is still there on the page it goes back to. Anything that
// is not an encoded flash is dropped.
func TestThemeKeepsFlash(t *testing.T) {
	b := richBackend("x")
	b.outcomeMsg = "Saved: No answer. The \"tried to reach you\" email is due Thu 29 Oct 09:30."
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}}, c))
	fl := cookieNamed(w, "__Host-lh_flash")
	if fl == nil {
		t.Fatal("no flash")
	}
	page := h.do(get("/leads/calls/9", c, fl)).Body.String()
	m := regexp.MustCompile(`name="flash" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil || m[1] != fl.Value {
		t.Fatalf("the theme switch does not carry the flash: %v", m)
	}
	w = h.do(post("/theme", url.Values{"theme": {"dark"}, "back": {"/leads/calls/9"}, "flash": {m[1]}}, c))
	again := cookieNamed(w, "__Host-lh_flash")
	if again == nil || again.Value != fl.Value || again.MaxAge <= 0 {
		t.Fatalf("the theme switch did not set the flash again: %+v", again)
	}
	if body := h.do(get("/leads/calls/9", c, again)).Body.String(); !strings.Contains(body, "Saved: No answer.") {
		t.Error("the flash is gone after the theme switch")
	}
	// No flash on the page: none carried, none set.
	if body := h.do(get("/leads/calls/9", c)).Body.String(); strings.Contains(body, `name="flash"`) {
		t.Error("a page with no flash carries one")
	}
	for _, bad := range []string{"not base64!", strings.Repeat("A", maxFlashCarry+1)} {
		w = h.do(post("/theme", url.Values{"theme": {"dark"}, "back": {"/"}, "flash": {bad}}, c))
		if cookieNamed(w, "__Host-lh_flash") != nil {
			t.Errorf("flash %.12q set", bad)
		}
	}
}
