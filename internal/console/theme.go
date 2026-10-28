package console

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// themeTTL is how long the theme cookie lasts.
const themeTTL = 365 * 24 * time.Hour

// themeOption is one button of the header's theme switch.
type themeOption struct{ Value, Label string }

// themeOptions are the header's theme switch, in order. "system" follows the
// device's own setting and is the default.
var themeOptions = []themeOption{{"system", "System"}, {"light", "Light"}, {"dark", "Dark"}}

// themeOf returns v when it is one of the themes, else "system".
func themeOf(v string) string {
	switch v {
	case "light", "dark":
		return v
	}
	return "system"
}

// theme returns the theme the request's cookie chose, "system" without one.
func (s *server) theme(r *http.Request) string {
	c, err := r.Cookie(s.themeName)
	if err != nil {
		return "system"
	}
	return themeOf(c.Value)
}

// themeBack is where the theme switch on this page sends the browser back to: the page
// itself for a GET, and for a page rendered as the answer to a POST, the page it stands
// for, as a POST route cannot be fetched.
func themeBack(r *http.Request, p page) string {
	switch {
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		return r.URL.RequestURI()
	case p.Title == "Sign in":
		return "/login"
	case p.Active != "":
		return p.Active
	}
	return "/"
}

// safeBack returns back when it is a path on this site, with its query; anything
// else, another host or a scheme included, is "/". Unlike safeNext it allows any
// path, the sign-in page too: a wrong path is a 404 on this site, never a way off it.
func safeBack(back string) string {
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.HasPrefix(back, `/\`) {
		return "/"
	}
	for _, c := range back {
		if c < 0x21 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	u, err := url.Parse(back)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return "/"
	}
	if path.Clean(u.Path) != u.Path || strings.HasPrefix(u.EscapedPath(), "//") {
		return "/"
	}
	return u.RequestURI()
}

// setTheme is POST /theme: it stores the chosen theme in a cookie and goes back to
// the page the switch was on. It needs no session, so the sign-in page has it too;
// cross-site posts are refused before it runs, as for every POST.
func (s *server) setTheme(w http.ResponseWriter, r *http.Request) {
	if ok, tooBig := parseForm(w, r); tooBig {
		return
	} else if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is off only in dev mode, over plain http on loopback
		Name:     s.themeName,
		Value:    themeOf(r.PostForm.Get("theme")),
		Path:     "/",
		HttpOnly: true,
		Secure:   !s.opt.Dev,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(themeTTL / time.Second),
	})
	// The page the switch was on read its flash as it rendered; set it again so the page it
	// goes back to still shows it. Only an encoded flash is taken, and cross-site posts never
	// reach here, so it is the page's own line; the template escapes it when shown.
	if f := r.PostForm.Get("flash"); validFlash(f) {
		b, _ := base64.RawURLEncoding.DecodeString(f)
		s.setFlash(w, string(b))
	}
	http.Redirect(w, r, safeBack(r.PostForm.Get("back")), http.StatusSeeOther) //nolint:gosec // G710: safeBack keeps it a path on this site
}
