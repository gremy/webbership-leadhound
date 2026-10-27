// Package enrich reads public facts about a business from its own pages: its phone numbers,
// its description and the links to its social profiles. Extract is pure: it reads one page
// body and fetches nothing.
//
// A phone comes from a tel: link, a JSON-LD telephone, a microdata telephone, or, on a
// contact page only, a labelled text line. A node or scope typed as a person is never read.
// A LinkedIn link is kept only for a company page, so a person's profile is never returned,
// and no output ever holds an email address.
package enrich

import (
	"encoding/json"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// DerivedBy names this rule set on the facts it produces.
const DerivedBy = "enrich@1"

// The kinds of fact Extract returns; they are org_fact.kind's values.
const (
	KindPhone       = "phone"
	KindDescription = "description"
	KindSocial      = "social"
)

const (
	// MaxBody is how much of a page body Extract reads.
	MaxBody = 2 << 20
	// MaxRunes is the longest value Extract returns.
	MaxRunes = 300
	// MaxSocial is the most social links Extract returns for one page.
	MaxSocial = 8
	// MaxScript is the largest JSON-LD script Extract parses; a longer one is skipped.
	MaxScript = 64 << 10
	// A phone's normalized value holds 8 to 15 digits.
	minDigits = 8
	maxDigits = 15
)

// Fact is one fact read from a page.
type Fact struct {
	Kind       string // KindPhone, KindDescription or KindSocial
	Value      string // as shown on the page, cleaned
	Normalized string // the comparison key: + and digits, the description itself, or the profile URL
}

// Extract reads the facts on one page. pageURL is the address the body came from; a page off
// orgDomain's registrable domain yields nothing. contactPage allows labelled phone lines.
// Facts come phones first, then the description, then social links, each in page order and
// each once.
func Extract(body []byte, pageURL, orgDomain string, contactPage bool) []Fact {
	base, err := url.Parse(pageURL)
	if err != nil || !onDomain(base, orgDomain) {
		return nil
	}
	p := &page{base: base, contact: contactPage, seen: map[string]bool{}}
	p.read(decodeBody(body))

	var out []Fact
	out = append(out, p.phones...)
	desc := p.description
	if desc == "" {
		desc = p.ogDescription
	}
	if d := cleanDescription(desc); d != "" {
		out = append(out, Fact{Kind: KindDescription, Value: d, Normalized: d})
	}
	out = append(out, p.social...)
	kept := out[:0]
	for _, f := range out {
		if safe(f) {
			kept = append(kept, f)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// page collects what one body yields.
type page struct {
	base    *url.URL
	contact bool

	phones        []Fact
	social        []Fact
	seen          map[string]bool // kind + "\x00" + normalized
	description   string
	ogDescription string
}

// read walks the body once with the tokenizer.
func (p *page) read(body string) {
	var (
		skipText  int    // depth inside script, style, template, noscript
		ldJSON    bool   // inside an application/ld+json script
		script    []byte // that script's text
		tooLong   bool
		itemTag   string // the element whose text is a microdata telephone
		itemDepth int
		itemText  strings.Builder
		personTag string // the element that opened a person's microdata scope
		personDep int
	)
	z := html.NewTokenizer(strings.NewReader(body))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[strings.ToLower(string(k))] = string(v)
			}
			opens := tt == html.StartTagToken && !voidTag(tag)
			if personTag != "" && opens && tag == personTag {
				personDep++
			}
			if personTag == "" && opens && hasAttrKey(attrs, "itemscope") && isPersonType(attrs["itemtype"]) {
				personTag, personDep = tag, 1
			}
			if itemTag != "" && opens && tag == itemTag {
				itemDepth++
			}
			switch tag {
			case "script", "style", "template", "noscript":
				if tt == html.StartTagToken {
					skipText++
					if tag == "script" && strings.EqualFold(strings.TrimSpace(attrs["type"]), "application/ld+json") {
						ldJSON, script, tooLong = true, script[:0], false
					}
				}
			case "meta":
				p.meta(attrs)
			case "a":
				p.link(attrs["href"])
			}
			if personTag == "" && hasItemprop(attrs["itemprop"], "telephone") {
				switch {
				case strings.TrimSpace(attrs["content"]) != "":
					p.phone(attrs["content"])
				case isTel(attrs["href"]):
					// the tel: link itself was read above
				case opens && itemTag == "":
					itemTag, itemDepth = tag, 1
					itemText.Reset()
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch tag {
			case "script", "style", "template", "noscript":
				if skipText > 0 {
					skipText--
				}
				if tag == "script" && ldJSON {
					if !tooLong {
						p.jsonLD(script)
					}
					ldJSON = false
				}
			}
			if itemTag != "" && tag == itemTag {
				if itemDepth--; itemDepth == 0 {
					p.phone(itemText.String())
					itemTag = ""
				}
			}
			if personTag != "" && tag == personTag {
				if personDep--; personDep == 0 {
					personTag = ""
				}
			}
		case html.TextToken:
			text := z.Text()
			switch {
			case ldJSON:
				if len(script)+len(text) > MaxScript {
					tooLong = true
				} else if !tooLong {
					script = append(script, text...)
				}
			case skipText > 0:
			default:
				if itemTag != "" {
					itemText.Write(text)
				}
				if p.contact && personTag == "" {
					p.labelled(string(text))
				}
			}
		}
	}
}

// meta reads the description and og:description.
func (p *page) meta(attrs map[string]string) {
	content := attrs["content"]
	if strings.TrimSpace(content) == "" {
		return
	}
	name := strings.ToLower(strings.TrimSpace(attrs["name"]))
	prop := strings.ToLower(strings.TrimSpace(attrs["property"]))
	switch {
	case name == "description" && p.description == "":
		p.description = content
	case (prop == "og:description" || name == "og:description") && p.ogDescription == "":
		p.ogDescription = content
	}
}

// link reads one href: a tel: link gives a phone, a social profile link a social fact.
func (p *page) link(href string) {
	h := strings.TrimSpace(href)
	if isTel(h) {
		p.phone(telNumber(h))
		return
	}
	if v, norm, ok := socialProfile(p.base, h); ok && len(p.social) < MaxSocial {
		p.add(&p.social, Fact{Kind: KindSocial, Value: v, Normalized: norm})
	}
}

// phone keeps raw when it normalises to a phone number.
func (p *page) phone(raw string) {
	v, norm, ok := NormalizePhone(raw)
	if ok {
		p.add(&p.phones, Fact{Kind: KindPhone, Value: v, Normalized: norm})
	}
}

func (p *page) add(list *[]Fact, f Fact) {
	key := f.Kind + "\x00" + f.Normalized
	if p.seen[key] {
		return
	}
	p.seen[key] = true
	*list = append(*list, f)
}

// labelRe finds a labelled phone line: the label (Tel, Telefon, Phone, T:, Tél and their
// longer spellings), an optional colon, then the number.
var labelRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:(?:t[eé]l[eé]phone|telefon|tel|t[eé]l|phone)\.?\s*:?|t\s*:)[ \t\x{a0}]*(\+?[0-9(][0-9 \t\x{a0}().\-/]{5,24}[0-9])`)

// labelled reads the labelled phone lines of one text node.
func (p *page) labelled(text string) {
	for line := range strings.SplitSeq(text, "\n") {
		for _, m := range labelRe.FindAllStringSubmatch(line, -1) {
			p.phone(m[1])
		}
	}
}

// jsonLD reads every telephone in one JSON-LD script: an object, an array or an @graph,
// on any node but a person's.
func (p *page) jsonLD(script []byte) {
	var v any
	if err := json.Unmarshal(script, &v); err != nil {
		return
	}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			if isPersonNode(x["@type"]) {
				return
			}
			switch t := x["telephone"].(type) { // the node's own number first
			case string:
				p.phone(t)
			case []any:
				for _, s := range t {
					if s, ok := s.(string); ok {
						p.phone(s)
					}
				}
			}
			for _, k := range slices.Sorted(maps.Keys(x)) { // then its children, in a stable order
				if k != "telephone" {
					walk(x[k])
				}
			}
		}
	}
	walk(v)
}

// isPersonNode reports a JSON-LD @type of Person (a string or a list holding it).
func isPersonNode(t any) bool {
	switch x := t.(type) {
	case string:
		return isPersonType(x)
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && isPersonType(s) {
				return true
			}
		}
	}
	return false
}

// isPersonType reports a schema type naming a person: "Person", or a URL ending in /Person.
func isPersonType(t string) bool {
	for f := range strings.FieldsSeq(t) {
		f = strings.TrimSuffix(strings.ToLower(f), "/")
		if f == "person" || strings.HasSuffix(f, "/person") || strings.HasSuffix(f, ":person") {
			return true
		}
	}
	return false
}

func hasItemprop(v, want string) bool {
	for f := range strings.FieldsSeq(v) {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

func hasAttrKey(attrs map[string]string, k string) bool {
	_, ok := attrs[k]
	return ok
}

func isTel(href string) bool {
	h := strings.TrimSpace(href)
	return len(h) > 4 && strings.EqualFold(h[:4], "tel:")
}

// telNumber is the number of a tel: link: unescaped, without its ;ext= or other parameters.
func telNumber(href string) string {
	n := strings.TrimSpace(href)[4:]
	if dec, err := url.PathUnescape(n); err == nil {
		n = dec
	}
	n, _, _ = strings.Cut(n, ";")
	n, _, _ = strings.Cut(n, "?")
	return n
}

// NormalizePhone cleans raw into the number as shown and its normalized value: a leading +
// (from "+" or the international "00"), then 8 to 15 digits. A national trunk "(0)" after a
// country code is dropped. Anything else, letters included, is not a phone.
func NormalizePhone(raw string) (value, normalized string, ok bool) {
	v := strings.Join(strings.FieldsFunc(raw, func(r rune) bool { return unicode.IsSpace(r) }), " ")
	v = strings.Trim(v, " .-/")
	if v == "" {
		return "", "", false
	}
	plus := strings.HasPrefix(v, "+")
	s := v
	if plus {
		s = strings.Replace(s, "(0)", "", 1)
	}
	var digits strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' || r == ' ' || r == '(' || r == ')' || r == '.' || r == '-' || r == '/':
		default:
			return "", "", false
		}
	}
	d := digits.String()
	if !plus && strings.HasPrefix(d, "00") {
		plus, d = true, d[2:]
	}
	if len(d) < minDigits || len(d) > maxDigits {
		return "", "", false
	}
	if plus {
		d = "+" + d
	}
	return v, d, true
}

// cleanDescription collapses whitespace, drops control characters and cuts to MaxRunes.
func cleanDescription(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > MaxRunes {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:MaxRunes]))
	}
	return s
}

// socialHosts are the networks whose profile links are kept, by registrable domain.
var socialHosts = map[string]string{
	"facebook.com":  "facebook.com",
	"instagram.com": "instagram.com",
	"linkedin.com":  "linkedin.com",
	"x.com":         "x.com",
	"twitter.com":   "x.com", // one account under either name
	"youtube.com":   "youtube.com",
	"tiktok.com":    "tiktok.com",
}

// shareWords are path segments of share buttons and embeds, never a profile.
var shareWords = map[string]bool{
	"sharer": true, "share": true, "sharing": true, "sharearticle": true, "intent": true,
	"dialog": true, "plugins": true,
}

// notProfile are first path segments that are a network's own pages, a post or a person.
var notProfile = map[string]map[string]bool{
	"facebook.com":  {"profile": true, "people": true, "groups": true, "events": true, "watch": true, "photo": true, "story": true, "permalink": true, "login": true, "tr": true, "home": true},
	"instagram.com": {"p": true, "reel": true, "reels": true, "explore": true, "stories": true, "tv": true, "accounts": true},
	"x.com":         {"i": true, "home": true, "hashtag": true, "search": true, "login": true},
}

// socialProfile resolves href against base and returns it as a social profile link: the
// value is the link without its query and fragment, the normalized value the https URL on
// the network's own host name, in lower case, without a trailing slash.
func socialProfile(base *url.URL, href string) (value, normalized string, ok bool) {
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil || href == "" {
		return "", "", false
	}
	u := base.ResolveReference(ref)
	if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Port() != "" {
		return "", "", false
	}
	reg, err := sitelist.Registrable(u.Hostname())
	if err != nil {
		return "", "", false
	}
	network, known := socialHosts[reg]
	if !known {
		return "", "", false
	}
	var segs []string // the path's segments, as written
	for s := range strings.SplitSeq(u.EscapedPath(), "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return "", "", false
	}
	for _, s := range segs {
		w := strings.TrimSuffix(strings.ToLower(s), ".php")
		if shareWords[w] || strings.HasPrefix(w, "share") {
			return "", "", false
		}
	}
	first := strings.TrimSuffix(strings.ToLower(segs[0]), ".php")
	switch network {
	case "linkedin.com":
		if len(segs) < 2 || first != "company" {
			return "", "", false
		}
		segs = segs[:2] // the company page, not its sub-pages
	case "youtube.com":
		if !strings.HasPrefix(first, "@") && ((first != "channel" && first != "c" && first != "user") || len(segs) < 2) {
			return "", "", false
		}
	case "tiktok.com":
		if !strings.HasPrefix(first, "@") || len(first) < 2 {
			return "", "", false
		}
	default:
		if notProfile[network][first] {
			return "", "", false
		}
	}
	path := "/" + strings.Join(segs, "/")
	clean := url.URL{Scheme: u.Scheme, Host: u.Host}
	value = clean.String() + path
	normalized = "https://" + network + strings.ToLower(path)
	if utf8.RuneCountInString(value) > MaxRunes || utf8.RuneCountInString(normalized) > MaxRunes {
		return "", "", false
	}
	return value, normalized, true
}

// emailRe is an email-shaped string anywhere in a value.
var emailRe = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9\-]+(?:\.[a-z0-9\-]+)*\.[a-z]{2,}`)

// safe is the last check on every fact: no email-shaped string, no person's profile path,
// nothing empty, nothing over MaxRunes.
func safe(f Fact) bool {
	for _, s := range []string{f.Value, f.Normalized} {
		if strings.TrimSpace(s) == "" || utf8.RuneCountInString(s) > MaxRunes || emailRe.MatchString(s) {
			return false
		}
		if strings.Contains(strings.ToLower(s), "/in/") {
			return false
		}
	}
	return true
}

// onDomain reports whether u is http(s) on orgDomain's registrable domain.
func onDomain(u *url.URL, orgDomain string) bool {
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	own, err := sitelist.Registrable(orgDomain)
	if err != nil {
		return false
	}
	d, err := sitelist.Registrable(u.Hostname())
	return err == nil && d == own
}

// voidTag reports an element that never has an end tag.
func voidTag(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "source", "track", "wbr":
		return true
	}
	return false
}

func decodeBody(body []byte) string {
	if len(body) > MaxBody {
		body = body[:MaxBody]
	}
	if len(body) == 0 {
		return ""
	}
	return postingguard.DecodePage(body)
}
