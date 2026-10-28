// Package cafe holds the cafe@1 detector and the café campaign's kind and chain rules.
//
// Detect reads a café's own homepage (bytes already fetched by the probe) and reports
// whether it shows digital loyalty, online ordering and Instagram, and its language. Every
// signal carries the evidence that made it (which script, link or phrase matched), so the
// lead page can say why. The signal list is data (signals.yaml, embedded), not code.
//
// The package is pure: no store, no fetch. The probe and the build (cmd/leadhound) wire it.
package cafe

import (
	_ "embed"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"
	"golang.org/x/net/html"

	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/postingguard"
)

// DerivedBy names this rule set in the probe rows it produces.
const DerivedBy = "cafe@1"

// MaxBody is how much of a body Detect reads; anything past it is cut.
const MaxBody = 2 << 20

// MaxSignalsPerKind caps the signals kept per kind, so the evidence stays short.
const MaxSignalsPerKind = 5

// Signal kinds.
const (
	SigLoyalty   = "loyalty"
	SigOrdering  = "ordering"
	SigInstagram = "instagram"
)

// Via: where on the page a signal was seen.
const (
	ViaScript       = "script"        // <script src>
	ViaLink         = "link"          // <a href>, <link href>, <area href>
	ViaIframe       = "iframe"        // <iframe src>
	ViaForm         = "form"          // <form action>
	ViaInlineScript = "inline-script" // a vendor host inside an inline script
	ViaWord         = "word"          // a phrase in the visible text
)

// Language sources.
const (
	LangViaHTML = "html-lang" // <html lang>
	LangViaMeta = "meta"      // <meta http-equiv="content-language"> or og:locale
	LangViaText = "text"      // detected from the visible text
)

// minLangConfidence is the text-detection confidence below which the language stays unknown.
const minLangConfidence = 0.6

//go:embed signals.yaml
var defaultSignals []byte

// Vendor is one known vendor: a loyalty platform, an ordering platform or Instagram.
type Vendor struct {
	ID    string   `yaml:"id"`
	Name  string   `yaml:"name"`
	Kind  string   `yaml:"kind"`
	Own   bool     `yaml:"own"`   // Webbership itself: the café is already a customer
	Match []string `yaml:"match"` // "host" or "host/path-prefix"
}

// Words are phrases of one kind, matched in the visible text.
type Words struct {
	Kind    string   `yaml:"kind"`
	ID      string   `yaml:"id"`
	Phrases []string `yaml:"phrases"`
}

// Config is the signal list.
type Config struct {
	Vendors []Vendor `yaml:"vendors"`
	Words   []Words  `yaml:"words"`

	matchers []matcher
	phrases  []phrase
}

type matcher struct {
	vendor     *Vendor
	host, path string // path "" or "/prefix"
	needle     string // host+path, lower case, for inline scripts
}

type phrase struct {
	kind, id string
	re       *regexp.Regexp
}

// ParseConfig reads a signal list in the signals.yaml shape and checks it.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("cafe signals: %w", err)
	}
	if err := c.compile(); err != nil {
		return nil, err
	}
	return &c, nil
}

// DefaultConfig is the embedded signals.yaml, parsed once. Callers must not change it.
var DefaultConfig = sync.OnceValue(func() *Config {
	c, err := ParseConfig(defaultSignals)
	if err != nil {
		panic(err) // the embedded file is covered by a test
	}
	return c
})

func validKind(k string) bool { return k == SigLoyalty || k == SigOrdering || k == SigInstagram }

func (c *Config) compile() error {
	seen := map[string]bool{}
	for i := range c.Vendors {
		v := &c.Vendors[i]
		if v.ID == "" || seen[v.ID] {
			return fmt.Errorf("cafe signals: vendor %d: id %q is empty or repeated", i, v.ID)
		}
		seen[v.ID] = true
		if !validKind(v.Kind) {
			return fmt.Errorf("cafe signals: vendor %q: kind %q is not loyalty, ordering or instagram", v.ID, v.Kind)
		}
		if len(v.Match) == 0 {
			return fmt.Errorf("cafe signals: vendor %q: match is empty", v.ID)
		}
		for _, m := range v.Match {
			m = strings.ToLower(strings.TrimSpace(m))
			host, path, _ := strings.Cut(m, "/")
			if host == "" || !strings.Contains(host, ".") || strings.ContainsAny(host, ":@ ") {
				return fmt.Errorf("cafe signals: vendor %q: match %q is not host[/path]", v.ID, m)
			}
			if path != "" {
				path = "/" + path
			}
			c.matchers = append(c.matchers, matcher{vendor: v, host: host, path: path, needle: host + path})
		}
	}
	for _, w := range c.Words {
		if !validKind(w.Kind) || w.ID == "" || len(w.Phrases) == 0 {
			return fmt.Errorf("cafe signals: words %q: needs a kind, an id and phrases", w.ID)
		}
		for _, p := range w.Phrases {
			fields := strings.Fields(strings.ToLower(p))
			if len(fields) == 0 {
				return fmt.Errorf("cafe signals: words %q: blank phrase", w.ID)
			}
			for i, f := range fields {
				fields[i] = regexp.QuoteMeta(f)
			}
			// Whole words: no letter or digit on either side (RE2's \b is ASCII-only).
			re := regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(` + strings.Join(fields, `\s+`) + `)(?:$|[^\p{L}\p{N}])`)
			c.phrases = append(c.phrases, phrase{kind: w.Kind, id: w.ID, re: re})
		}
	}
	return nil
}

// OwnVendorID is the id WithOwnVendor gives the own vendor.
const OwnVendorID = "webbership"

// WithOwnVendor returns a copy of c with the own vendor (Webbership itself) added, matching
// hosts ("host" or "host/path-prefix"). A café showing it is already a customer: Classify
// skips it. The hosts come from the operator's private config, never from this repository.
func (c *Config) WithOwnVendor(name string, hosts []string) (*Config, error) {
	out := &Config{Vendors: append(slices.Clone(c.Vendors), Vendor{ID: OwnVendorID, Name: name, Kind: SigLoyalty, Own: true, Match: hosts}), Words: c.Words}
	if err := out.compile(); err != nil {
		return nil, err
	}
	return out, nil
}

// Vendor returns the vendor with id, or nil.
func (c *Config) Vendor(id string) *Vendor {
	for i := range c.Vendors {
		if c.Vendors[i].ID == id {
			return &c.Vendors[i]
		}
	}
	return nil
}

// Page is a fetched homepage.
type Page struct {
	URL  string // the final URL after redirects; links resolve against it
	Body []byte // as fetched; decoded as UTF-8, else by the page's own charset declaration
}

// Signal is one piece of evidence. Vendor is the vendor id, or the words id for a phrase.
// Match is what matched: the link or script URL, or the phrase as it reads on the page.
type Signal struct {
	Kind   string
	Vendor string
	Via    string
	Match  string
}

// String renders the signal as "kind:vendor:via:match".
func (s Signal) String() string { return s.Kind + ":" + s.Vendor + ":" + s.Via + ":" + s.Match }

// Result is what Detect found.
type Result struct {
	HasLoyalty   bool
	HasOrdering  bool
	HasInstagram bool
	OwnCustomer  bool     // a loyalty signal from the own vendor (Webbership)
	Vendors      []string // loyalty vendor ids seen, in page order (phrase ids excluded)
	Language     string   // ISO 639-1, lower case; "" when unknown
	LanguageVia  string   // LangViaHTML, LangViaMeta or LangViaText
	LanguageNote string   // the attribute value, or the detection confidence
	Signals      []Signal // in page order, at most MaxSignalsPerKind per kind
}

// Evidence is every signal and the language source joined with "|", for the probe row.
func (r Result) Evidence() string {
	parts := make([]string, 0, len(r.Signals)+1)
	for _, s := range r.Signals {
		parts = append(parts, s.String())
	}
	if r.Language != "" {
		parts = append(parts, "lang:"+r.Language+":"+r.LanguageVia+":"+r.LanguageNote)
	}
	return strings.Join(parts, "|")
}

// SignalsOf returns the signals of one kind.
func (r Result) SignalsOf(kind string) []Signal {
	var out []Signal
	for _, s := range r.Signals {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// Detect reads one homepage with cfg (nil: DefaultConfig). It is deterministic: the same
// bytes give the same result.
func Detect(cfg *Config, pg Page) Result {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	body := pg.Body
	if len(body) > MaxBody {
		body = body[:MaxBody]
	}
	markup := postingguard.DecodePage(body)
	base, _ := url.Parse(pg.URL)
	d := detection{cfg: cfg, seen: map[string]bool{}, count: map[string]int{}}

	var text strings.Builder
	z := html.NewTokenizer(strings.NewReader(markup))
	inScript, inStyle := false, false
	for done := false; !done; {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			done = true
		case html.StartTagToken, html.SelfClosingTagToken:
			tn, hasAttr := z.TagName()
			name := string(tn)
			attrs := readAttrs(z, hasAttr)
			switch name {
			case "html":
				d.setLang(attrs["lang"], LangViaHTML)
			case "meta":
				if strings.EqualFold(attrs["http-equiv"], "content-language") {
					d.setLang(attrs["content"], LangViaMeta)
				}
				if strings.EqualFold(attrs["property"], "og:locale") {
					d.setLang(attrs["content"], LangViaMeta)
				}
			case "script":
				d.link(base, attrs["src"], ViaScript)
				inScript = tt == html.StartTagToken
			case "a", "link", "area":
				d.link(base, attrs["href"], ViaLink)
			case "iframe":
				d.link(base, attrs["src"], ViaIframe)
			case "form":
				d.link(base, attrs["action"], ViaForm)
			case "style":
				inStyle = tt == html.StartTagToken
			}
		case html.EndTagToken:
			inScript, inStyle = false, false
		case html.TextToken:
			switch {
			case inScript:
				d.inline(string(z.Text()))
			case inStyle:
			default:
				text.WriteString(string(z.Text()))
				text.WriteByte(' ')
			}
		}
	}
	visible := strings.Join(strings.Fields(text.String()), " ")
	d.words(visible)
	if d.res.Language == "" {
		if iso, conf := gates.DetectLanguage(visible); iso != "" && conf >= minLangConfidence {
			d.res.Language, d.res.LanguageVia = iso, LangViaText
			d.res.LanguageNote = fmt.Sprintf("%.0f%%", conf*100)
		}
	}
	return d.res
}

type detection struct {
	cfg   *Config
	res   Result
	seen  map[string]bool
	count map[string]int
}

func (d *detection) add(s Signal, own bool) {
	key := s.String()
	if d.seen[key] {
		return
	}
	d.seen[key] = true
	switch s.Kind {
	case SigLoyalty:
		d.res.HasLoyalty = true
		if s.Via != ViaWord && !slices.Contains(d.res.Vendors, s.Vendor) {
			d.res.Vendors = append(d.res.Vendors, s.Vendor)
		}
		if own {
			d.res.OwnCustomer = true
		}
	case SigOrdering:
		d.res.HasOrdering = true
	case SigInstagram:
		d.res.HasInstagram = true
	}
	if d.count[s.Kind] < MaxSignalsPerKind {
		d.count[s.Kind]++
		d.res.Signals = append(d.res.Signals, s)
	}
}

// link checks one URL attribute against the vendors.
func (d *detection) link(base *url.URL, ref, via string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	u, err := url.Parse(ref)
	if err != nil {
		return
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	path := strings.ToLower(u.EscapedPath())
	for _, m := range d.cfg.matchers {
		if !places.HostIs(host, m.host) {
			continue
		}
		if m.path != "" && path != m.path && !strings.HasPrefix(path, m.path+"/") {
			continue
		}
		d.add(Signal{Kind: m.vendor.Kind, Vendor: m.vendor.ID, Via: via, Match: cleanURL(u)}, m.vendor.Own)
	}
}

// inline finds vendor hosts named inside an inline script (a widget loader).
func (d *detection) inline(js string) {
	low := strings.ToLower(js)
	for _, m := range d.cfg.matchers {
		for from := 0; ; {
			i := strings.Index(low[from:], m.needle)
			if i < 0 {
				break
			}
			at := from + i
			if hostBoundary(low, at) {
				d.add(Signal{Kind: m.vendor.Kind, Vendor: m.vendor.ID, Via: ViaInlineScript, Match: m.needle}, m.vendor.Own)
				break
			}
			from = at + len(m.needle)
		}
	}
}

// hostBoundary: the host at i is not the tail of a longer name ("notwolt.com").
func hostBoundary(s string, i int) bool {
	if i == 0 {
		return true
	}
	c := s[i-1]
	return c == '.' || c == '/' || c == '"' || c == '\'' || c == ' ' || c == '=' || c == '(' || c == '`'
}

// words finds the configured phrases in the visible text.
func (d *detection) words(text string) {
	for _, p := range d.cfg.phrases {
		if m := p.re.FindStringSubmatch(text); m != nil {
			d.add(Signal{Kind: p.kind, Vendor: p.id, Via: ViaWord, Match: m[1]}, false)
		}
	}
}

// setLang takes the first declared language ("ro-RO", "ro_RO" → "ro").
func (d *detection) setLang(v, via string) {
	if d.res.Language != "" {
		return
	}
	v = strings.TrimSpace(v)
	code, _, _ := strings.Cut(strings.ReplaceAll(v, "_", "-"), "-")
	code = strings.ToLower(code)
	if len(code) != 2 || code[0] < 'a' || code[0] > 'z' || code[1] < 'a' || code[1] > 'z' {
		return
	}
	d.res.Language, d.res.LanguageVia, d.res.LanguageNote = code, via, v
}

func readAttrs(z *html.Tokenizer, more bool) map[string]string {
	attrs := map[string]string{}
	for more {
		var k, v []byte
		k, v, more = z.TagAttr()
		if len(k) == 0 {
			continue
		}
		if _, dup := attrs[string(k)]; !dup {
			attrs[string(k)] = string(v)
		}
	}
	return attrs
}

// cleanURL is scheme://host/path without the query or fragment (a query may carry ids).
func cleanURL(u *url.URL) string {
	return u.Scheme + "://" + strings.ToLower(u.Host) + u.EscapedPath()
}
