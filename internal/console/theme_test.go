package console

import (
	"net/http"
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

func TestThemeSetsCookieAndGoesBack(t *testing.T) {
	for _, dev := range []bool{false, true} {
		h := newHarness(t, richBackend("x"), dev)
		name := themeCookie(dev, "").Name
		for _, tt := range []struct{ theme, want string }{
			{"light", "light"}, {"dark", "dark"}, {"system", "system"},
			{"", "system"}, {"purple", "system"}, {"DARK", "system"}, {"dark;x=1", "system"},
		} {
			w := h.do(post("/theme", url.Values{"theme": {tt.theme}, "back": {"/jobs?x=1"}}, h.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/jobs?x=1" {
				t.Fatalf("dev=%v theme %q: %d Location %q, want 303 /jobs?x=1", dev, tt.theme, w.Code, w.Header().Get("Location"))
			}
			c := cookieNamed(w, name)
			if c == nil {
				t.Fatalf("dev=%v theme %q: no %s cookie", dev, tt.theme, name)
			}
			if c.Value != tt.want || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode ||
				c.Secure == dev || c.MaxAge < 360*24*60*60 || c.Domain != "" {
				t.Errorf("dev=%v theme %q: cookie %+v, want %s, Path /, HttpOnly, Lax, Secure %v, a year", dev, tt.theme, c, tt.want, !dev)
			}
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

func TestLayoutRendersTheme(t *testing.T) {
	for _, dev := range []bool{false, true} {
		h := newHarness(t, richBackend("x"), dev)
		s := h.session(t)
		for _, tt := range []struct{ cookie, attr, scheme, pressed string }{
			{"", "", "light dark", "system"},
			{"system", "", "light dark", "system"},
			{"light", ` data-theme="light"`, "light", "light"},
			{"dark", ` data-theme="dark"`, "dark", "dark"},
			{"purple", "", "light dark", "system"},
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
				if !strings.Contains(body, `<meta name="color-scheme" content="`+tt.scheme+`">`) {
					t.Errorf("dev=%v %s cookie %q: color-scheme meta is not %q", dev, p, tt.cookie, tt.scheme)
				}
				if !strings.Contains(body, `<form class="theme" method="post" action="/theme">`) {
					t.Fatalf("dev=%v %s: no theme form", dev, p)
				}
				for _, v := range []string{"system", "light", "dark"} {
					want := `name="theme" value="` + v + `" data-label="` + map[string]string{"system": "System", "light": "Light", "dark": "Dark"}[v] + `" aria-pressed="` + map[bool]string{true: "true", false: "false"}[v == tt.pressed] + `"`
					if !strings.Contains(body, want) {
						t.Errorf("dev=%v %s cookie %q: lacks %s", dev, p, tt.cookie, want)
					}
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
