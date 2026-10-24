// Package drupal tells from one homepage response whether a site runs Drupal, and which
// major version. Detect is pure: it reads the status, headers and body it is given and
// fetches nothing.
package drupal

import (
	"bytes"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"github.com/gremy/webbership-leadhound/internal/postingguard"
)

// Signal is one piece of evidence; Value is empty for a presence signal.
type Signal struct {
	Name, Value string
}

// String is the signal as it appears in the evidence string: Name, or Name:Value.
func (s Signal) String() string {
	if s.Value == "" {
		return s.Name
	}
	return s.Name + ":" + s.Value
}

// Finding is what Detect concluded. Major, Exact and Ambiguous are set only when IsDrupal;
// a bot check carries its own signal and nothing else.
type Finding struct {
	IsDrupal  bool
	Major     int
	Exact     string
	Ambiguous string
	Signals   []Signal
	BotCheck  bool
	SiteName  string
}

// Evidence joins the signals with "|" (the probe's evidence format).
func (f Finding) Evidence() string {
	parts := make([]string, len(f.Signals))
	for i, s := range f.Signals {
		parts[i] = s.String()
	}
	return strings.Join(parts, "|")
}

// page is what one pass over the markup collected.
type page struct {
	metaGenerators []string
	siteName       string
	hasSiteName    bool
	settingsJSON   bool
	d7Settings     bool
	ajaxPageState  bool
	systemBase     bool
	challenge      bool
	urls           []*url.URL
}

// Detect applies the drupal@1 rules to one response. finalURL is the address the response
// came from, used to resolve relative asset links. Only the first MaxBody bytes are read.
func Detect(status int, header http.Header, body []byte, finalURL string) Finding {
	if (status == http.StatusForbidden || status == http.StatusServiceUnavailable) &&
		strings.EqualFold(strings.TrimSpace(header.Get("Cf-Mitigated")), "challenge") {
		return Finding{BotCheck: true, Signals: []Signal{{Name: SigCfMitigated, Value: "challenge"}}}
	}
	if len(body) > MaxBody {
		body = body[:MaxBody]
	}
	p := scan(decode(body, header.Get("Content-Type")), finalURL)
	if p.challenge {
		return Finding{BotCheck: true, Signals: []Signal{{Name: SigChallengePlatform}}}
	}

	sig := map[string]string{}
	set := func(name, value string) {
		if _, ok := sig[name]; !ok {
			sig[name] = value
		}
	}

	headerMajor := generatorMajor(header.Get("X-Generator"))
	if headerMajor > 0 {
		set(SigXGenerator, strconv.Itoa(headerMajor))
	}
	metaMajor := 0
	for _, g := range p.metaGenerators {
		if m := generatorMajor(g); m > 0 {
			metaMajor = m
			set(SigMetaGenerator, strconv.Itoa(m))
			break
		}
	}
	if header.Get("X-Drupal-Cache") != "" {
		set(SigXDrupalCache, "")
	}
	dynamic := header.Get("X-Drupal-Dynamic-Cache") != ""
	if dynamic {
		set(SigXDrupalDynamic, "")
	}
	if strings.Contains(header.Get("Expires"), "1978") {
		set(SigExpires1978, "")
	}
	if p.settingsJSON {
		set(SigDrupalSettings, "")
	}
	if p.d7Settings {
		set(SigD7Settings, "")
		if p.ajaxPageState {
			set(SigAjaxPageState, "")
		}
	}
	if p.systemBase {
		set(SigSystemBaseCSS, "")
	}

	miscJS, agg := false, false
	own := hostKey(finalURL)
	var seenV, ownV []string
	for _, u := range p.urls {
		path := u.EscapedPath()
		if v, ok := coreMiscVersion(u); ok {
			seenV = append(seenV, v)
			if sameHost(u, own) {
				ownV = append(ownV, v)
			}
		}
		for _, pre := range corePathPrefixes {
			if strings.Contains(path, pre) {
				set(SigCorePaths, "")
				break
			}
		}
		if aggFileRe.MatchString(path) && u.Query().Has("include") {
			agg = true
			set(SigAggInclude, "")
		}
		nonCoreMisc := strings.Contains(path, "/misc/") && !strings.Contains(path, "/core/misc/")
		if strings.Contains(path, "/sites/all/") || nonCoreMisc {
			set(SigD7Paths, "")
		}
		if nonCoreMisc && strings.HasSuffix(path, "/misc/drupal.js") {
			miscJS = true
			set(SigMiscDrupalJS, "")
		}
		if strings.HasSuffix(path, "/"+systemBaseMarker) {
			set(SigSystemBaseCSS, "")
		}
	}

	knownMajor := headerMajor
	if knownMajor == 0 {
		knownMajor = metaMajor
	}
	exact := pickExact(ownV, knownMajor)
	switch {
	case exact != "":
		sig[SigCoreAssetV] = exact
	case len(seenV) > 0:
		sig[SigCoreAssetV] = seenV[0]
	}

	f := Finding{Signals: ordered(sig), SiteName: p.siteName}
	f.IsDrupal = headerMajor > 0 || metaMajor > 0 || p.settingsJSON || p.d7Settings
	if !f.IsDrupal {
		return f
	}
	f.Exact = exact
	_, systemBase := sig[SigSystemBaseCSS]
	d8Plus := dynamic || p.settingsJSON
	switch {
	case headerMajor > 0:
		f.Major = headerMajor
	case metaMajor > 0:
		f.Major = metaMajor
	case exact != "":
		f.Major = versionMajor(exact)
	case d8Plus && agg:
		f.Ambiguous = Ambiguous10Or11
	case d8Plus:
		f.Ambiguous = Ambiguous8Plus
	case p.d7Settings && (p.ajaxPageState || systemBase):
		f.Major = 7
	case p.d7Settings && miscJS:
		f.Major = 6
	case p.d7Settings:
		f.Ambiguous = Ambiguous6Or7
	}
	return f
}

// generatorMajor is the major in a "Drupal N …" Generator value, or 0.
func generatorMajor(v string) int {
	m := generatorRe.FindStringSubmatch(v)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// hostKey is a URL's host, lower-cased and without a leading "www.", or "" when it has none.
func hostKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// sameHost reports whether u (already resolved against the page) is on the page's own
// host; a link with no host at all counts as the page's own.
func sameHost(u *url.URL, own string) bool {
	return u.Host == "" || strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") == own
}

// versionMajor is the first number of an X.Y.Z version.
func versionMajor(v string) int {
	n, _ := strconv.Atoi(v[:strings.IndexByte(v, '.')])
	return n
}

// pickExact is the first /core/misc/ version that is Drupal core's: major 8 to 11, and
// equal to knownMajor when another signal gave one. A bundled library's ?v= (3.11.7,
// 4.3.0) fails both and is evidence only.
func pickExact(versions []string, knownMajor int) string {
	for _, v := range versions {
		m := versionMajor(v)
		if m < minExactMajor || m > maxExactMajor {
			continue
		}
		if knownMajor == 0 || m == knownMajor {
			return v
		}
	}
	return ""
}

// coreMiscVersion is the X.Y.Z of a /core/misc/*.js?v=X.Y.Z asset.
func coreMiscVersion(u *url.URL) (string, bool) {
	path := u.EscapedPath()
	if !strings.Contains(path, "/core/misc/") || !strings.HasSuffix(path, ".js") {
		return "", false
	}
	v := u.Query().Get("v")
	if !exactRe.MatchString(v) {
		return "", false
	}
	return v, true
}

// ordered lists the collected signals in signalOrder.
func ordered(sig map[string]string) []Signal {
	if len(sig) == 0 {
		return nil
	}
	out := make([]Signal, 0, len(sig))
	for _, name := range signalOrder {
		if v, ok := sig[name]; ok {
			out = append(out, Signal{Name: name, Value: v})
		}
	}
	return out
}

// scan walks the markup once, collecting meta tags, settings blocks, script and style
// text markers and every src/href link.
func scan(markup, finalURL string) page {
	var p page
	if markup == "" {
		return p
	}
	baseURL, _ := url.Parse(finalURL)
	z := html.NewTokenizer(strings.NewReader(markup))
	inScript, inStyle := false, false
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return p // a strings.Reader only ends with io.EOF
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				key := string(k)
				if _, dup := attrs[key]; !dup {
					attrs[key] = string(v)
				}
				if key == "src" || key == "href" {
					if u := resolve(baseURL, string(v)); u != nil {
						p.urls = append(p.urls, u)
					}
				}
			}
			switch string(name) {
			case "meta":
				p.readMeta(attrs)
			case "script":
				inScript = tt == html.StartTagToken
				if strings.EqualFold(attrs["type"], "application/json") &&
					attrs["data-drupal-selector"] == "drupal-settings-json" {
					p.settingsJSON = true
				}
				if isChallenge(attrs["src"]) {
					p.challenge = true
				}
			case "style":
				inStyle = tt == html.StartTagToken
			case "form":
				if attrs["id"] == challengeFormID {
					p.challenge = true
				}
			}
		case html.EndTagToken:
			inScript, inStyle = false, false
		case html.TextToken:
			if !inScript && !inStyle {
				continue
			}
			text := string(z.Text())
			if strings.Contains(text, systemBaseMarker) {
				p.systemBase = true
			}
			if inScript {
				if strings.Contains(text, d7SettingsMarker) {
					p.d7Settings = true
					if strings.Contains(text, ajaxMarker) {
						p.ajaxPageState = true
					}
				}
				if isChallenge(text) {
					p.challenge = true
				}
			}
		}
	}
}

func (p *page) readMeta(attrs map[string]string) {
	content := attrs["content"]
	if strings.EqualFold(attrs["name"], "generator") {
		p.metaGenerators = append(p.metaGenerators, content)
	}
	if !p.hasSiteName && (attrs["property"] == "og:site_name" || attrs["name"] == "og:site_name") {
		p.hasSiteName = true
		p.siteName = cleanSiteName(content)
	}
}

// isChallenge reports an active Cloudflare challenge in a script's src or text: the
// challenge options object, or a challenge-platform URL to an orchestrate script. A
// challenge-platform URL under /scripts/jsd/ is the passive detection script and never
// counts on its own.
func isChallenge(s string) bool {
	if strings.Contains(s, chlOptMarker) {
		return true
	}
	for {
		i := strings.Index(s, challengeMarker)
		if i < 0 {
			return false
		}
		s = s[i+len(challengeMarker):]
		ref := s
		if j := strings.IndexAny(ref, "'\"` \t\r\n)"); j >= 0 {
			ref = ref[:j]
		}
		if !strings.Contains(ref, jsdPath) && strings.Contains(ref, orchestrateMarker) {
			return true
		}
	}
}

// decode is the markup as UTF-8: valid UTF-8 as is; else by the page's own declaration
// (BOM or meta charset, via postingguard.DecodePage); else by the Content-Type charset;
// else UTF-8 with invalid bytes replaced.
func decode(body []byte, contentType string) string {
	if len(body) == 0 {
		return ""
	}
	if utf8.Valid(body) || declaresCharset(body) {
		return postingguard.DecodePage(body)
	}
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		if enc, _ := charset.Lookup(params["charset"]); enc != nil {
			if out, err := enc.NewDecoder().Bytes(body); err == nil {
				return strings.ToValidUTF8(string(out), "\uFFFD")
			}
		}
	}
	return postingguard.DecodePage(body)
}

// declaresCharset reports a BOM or a meta charset in the first 1024 bytes.
func declaresCharset(body []byte) bool {
	if bytes.HasPrefix(body, []byte{0xFF, 0xFE}) || bytes.HasPrefix(body, []byte{0xFE, 0xFF}) {
		return true
	}
	return metaCharsetRe.Match(body[:min(len(body), 1024)])
}

// resolve parses a link against the page's address; a link that does not parse is skipped.
func resolve(baseURL *url.URL, ref string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return nil
	}
	if baseURL != nil {
		return baseURL.ResolveReference(u)
	}
	return u
}

// cleanSiteName folds whitespace, drops control and invisible runes and cuts to
// SiteNameMaxRunes.
func cleanSiteName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	s = postingguard.CleanField(s)
	if utf8.RuneCountInString(s) > SiteNameMaxRunes {
		s = strings.TrimSpace(string([]rune(s)[:SiteNameMaxRunes]))
	}
	return s
}
