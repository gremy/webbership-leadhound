// Package contacts finds an organisation's published contact routes on its own site: role
// email addresses on its own registrable domain, and contact forms. ContactLinks and
// Extract are pure; Find fetches at most two pages beyond the homepage through an injected
// Fetcher, and MX checks a domain through an injected Resolver.
//
// An address whose local part is not a role is counted as person-like and dropped: it is
// never returned and never logged. A Cloudflare-protected address is counted and never
// decoded.
package contacts

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"

	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// DerivedBy names this rule set on the routes it produces.
const DerivedBy = "contacts@1"

// MaxBody is how much of a page body ContactLinks and Extract read.
const MaxBody = 2 << 20

// maxLinks is how many contact links ContactLinks returns.
const maxLinks = 2

// contactWords are the words a contact link's text or path holds, in priority order.
var contactWords = []string{
	"contact", "contact-us", "kontakt", "impressum", "contacteer", "contacto", "contactati", "despre", "about",
}

// protectedPath is the path of a Cloudflare-protected email link.
const protectedPath = "/cdn-cgi/l/email-protection"

var emailRe = regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9\-]+(?:\.[a-z0-9\-]+)*\.[a-z]{2,}`)

// Result is what Extract found on one page.
type Result struct {
	Emails     []string // role addresses on the org's own domain, lower-case, first-seen order
	PersonLike int      // distinct own-domain addresses whose local part is not a role (dropped)
	Foreign    int      // distinct addresses on another registrable domain (dropped)
	Protected  int      // distinct Cloudflare-protected addresses (never decoded)
	Forms      []string // the page URL, once, when it holds a contact form
}

// ContactLinks returns at most two absolute http(s) URLs of links on the page at base that
// stay on orgDomain's registrable domain and whose text or path holds a contact word. They
// are ordered by the first word of the contact list each matches, then by page order. A link
// holding an address ("@" or "%40" anywhere) or pointing at a protected-email path is
// skipped; the query and fragment are dropped, and a link to the page itself and a repeat
// are skipped.
func ContactLinks(base *url.URL, body []byte, orgDomain string) []string {
	if base == nil {
		return nil
	}
	own, err := sitelist.Registrable(orgDomain)
	if err != nil {
		return nil
	}
	type cand struct {
		u    string
		rank int
	}
	var cands []cand
	seen := map[string]bool{stripFragment(base): true}

	var href string
	var text strings.Builder
	inA := false
	flush := func() {
		if !inA {
			return
		}
		inA = false
		u, ok := resolveHTTP(base, href)
		if !ok || holdsAddress(href) || holdsAddress(u.String()) || strings.Contains(u.EscapedPath(), protectedPath) {
			return
		}
		u.RawQuery, u.ForceQuery = "", false
		key := stripFragment(u)
		if seen[key] {
			return
		}
		if d, err := sitelist.Registrable(u.Hostname()); err != nil || d != own {
			return
		}
		rank := contactRank(strings.ToLower(text.String()), strings.ToLower(pathOf(u)))
		if rank < 0 {
			return
		}
		seen[key] = true
		cands = append(cands, cand{u: key, rank: rank})
	}

	z := html.NewTokenizer(strings.NewReader(decodeBody(body)))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "a" {
				continue
			}
			flush()
			href = ""
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if string(k) == "href" {
					href = string(v)
				}
			}
			text.Reset()
			inA = true // a self-closing <a/> is a start tag in HTML too
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "a" {
				flush()
			}
		case html.TextToken:
			if inA {
				text.Write(z.Text())
				text.WriteByte(' ')
			}
		}
	}
	flush()

	// A stable insertion sort by rank keeps page order within a rank.
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && cands[j].rank < cands[j-1].rank; j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
	out := make([]string, 0, maxLinks)
	for _, c := range cands {
		if len(out) == maxLinks {
			break
		}
		out = append(out, c.u)
	}
	return out
}

// contactRank is the index of the first contact word that text or path holds, or -1.
func contactRank(text, path string) int {
	for i, w := range contactWords {
		if strings.Contains(text, w) || strings.Contains(path, w) {
			return i
		}
	}
	return -1
}

// Extract reads one page's contact routes. pageURL is the address the body came from;
// orgDomain is the organisation's registrable domain; roles are the role local parts.
func Extract(pageURL string, body []byte, orgDomain string, roles []string) Result {
	s := newSeen()
	s.extract(pageURL, body, orgDomain, roles)
	return s.result()
}

// seen collects addresses across pages, so a repeat is counted once.
type seen struct {
	emails    []string
	kept      map[string]bool
	person    map[string]bool
	foreign   map[string]bool
	protected map[string]bool
	forms     []string
	formSeen  map[string]bool
}

func newSeen() *seen {
	return &seen{
		kept: map[string]bool{}, person: map[string]bool{}, foreign: map[string]bool{},
		protected: map[string]bool{}, formSeen: map[string]bool{},
	}
}

func (s *seen) result() Result {
	return Result{
		Emails:     append([]string(nil), s.emails...),
		PersonLike: len(s.person),
		Foreign:    len(s.foreign),
		Protected:  len(s.protected),
		Forms:      append([]string(nil), s.forms...),
	}
}

// extract adds one page's findings; it reports whether the page yielded a new route.
func (s *seen) extract(pageURL string, body []byte, orgDomain string, roles []string) bool {
	own, err := sitelist.Registrable(orgDomain)
	if err != nil {
		own = strings.ToLower(strings.TrimSuffix(orgDomain, "."))
	}
	roleSet := map[string]bool{}
	for _, r := range roles {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" {
			roleSet[r] = true
		}
	}
	yielded := false
	add := func(addr string) {
		addr = strings.ToLower(strings.Trim(addr, ".-"))
		local, domain, ok := strings.Cut(addr, "@")
		if !ok || local == "" || domain == "" {
			return
		}
		d, err := sitelist.Registrable(domain)
		switch {
		case err != nil || d != own:
			s.foreign[addr] = true
		case !isRole(local, roleSet):
			s.person[addr] = true
		case !s.kept[addr]:
			s.kept[addr] = true
			s.emails = append(s.emails, addr)
			yielded = true
		}
	}

	var (
		skipText  int // depth inside script, style, template, noscript
		formDepth int
		textarea  bool
		emailIn   bool
		hasForm   bool
	)
	z := html.NewTokenizer(strings.NewReader(decodeBody(body)))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				attrs[string(k)] = string(v)
			}
			if v, ok := attrs["data-cfemail"]; ok {
				s.protected[strings.ToLower(v)] = true
			}
			switch tag {
			case "script", "style", "template", "noscript":
				if tt == html.StartTagToken {
					skipText++
				}
			case "a":
				s.href(attrs["href"], add)
			case "form":
				if tt == html.StartTagToken {
					if formDepth == 0 {
						textarea, emailIn = false, false
					}
					formDepth++
				}
			case "textarea":
				if formDepth > 0 {
					textarea = true
				}
			case "input":
				if formDepth > 0 && emailField(attrs) {
					emailIn = true
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style", "template", "noscript":
				if skipText > 0 {
					skipText--
				}
			case "form":
				if formDepth > 0 {
					formDepth--
					if formDepth == 0 && textarea && emailIn {
						hasForm = true
					}
				}
			}
		case html.TextToken:
			if skipText == 0 {
				for _, m := range emailRe.FindAllString(string(z.Text()), -1) {
					add(m)
				}
			}
		}
	}
	if formDepth > 0 && textarea && emailIn {
		hasForm = true // an unclosed form at the end of the page
	}
	if hasForm && pageURL != "" && !s.formSeen[pageURL] {
		s.formSeen[pageURL] = true
		s.forms = append(s.forms, pageURL)
		yielded = true
	}
	return yielded
}

// href reads one link: a protected link is counted by its encoded token, a mailto link
// gives its addresses (the query dropped).
func (s *seen) href(href string, add func(string)) {
	h := strings.TrimSpace(href)
	if i := strings.Index(h, protectedPath); i >= 0 {
		_, token, _ := strings.Cut(h[i:], "#")
		if token == "" {
			token = h
		}
		s.protected[strings.ToLower(token)] = true
		return
	}
	if len(h) < len("mailto:") || !strings.EqualFold(h[:len("mailto:")], "mailto:") {
		return
	}
	rest := h[len("mailto:"):]
	rest, _, _ = strings.Cut(rest, "?")
	if dec, err := url.PathUnescape(rest); err == nil {
		rest = dec
	}
	for _, part := range strings.Split(rest, ",") {
		if m := emailRe.FindString(strings.TrimSpace(part)); m != "" {
			add(m)
		}
	}
}

// isRole reports whether local is a role, or a role followed by "-" or "." and a suffix.
func isRole(local string, roles map[string]bool) bool {
	if roles[local] {
		return true
	}
	for i := 0; i < len(local); i++ {
		if (local[i] == '-' || local[i] == '.') && i+1 < len(local) && roles[local[:i]] {
			return true
		}
	}
	return false
}

// emailField reports an input that takes an email address: type email, or a name or id
// holding "mail".
func emailField(attrs map[string]string) bool {
	if strings.EqualFold(strings.TrimSpace(attrs["type"]), "email") {
		return true
	}
	return strings.Contains(strings.ToLower(attrs["name"]), "mail") ||
		strings.Contains(strings.ToLower(attrs["id"]), "mail")
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

// resolveHTTP resolves href against base and keeps it only when it is http(s) with a host.
func resolveHTTP(base *url.URL, href string) (*url.URL, bool) {
	href = strings.TrimSpace(href)
	if href == "" {
		return nil, false
	}
	ref, err := url.Parse(href)
	if err != nil {
		return nil, false
	}
	u := base.ResolveReference(ref)
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, false
	}
	return u, true
}

// holdsAddress reports an "@" or its percent-encoding anywhere in s.
func holdsAddress(s string) bool {
	return strings.Contains(s, "@") || strings.Contains(strings.ToLower(s), "%40")
}

func stripFragment(u *url.URL) string {
	c := *u
	c.Fragment, c.RawFragment = "", ""
	if c.Path == "" {
		c.Path = "/"
	}
	return c.String()
}

func pathOf(u *url.URL) string {
	if p, err := url.PathUnescape(u.EscapedPath()); err == nil {
		return p
	}
	return u.EscapedPath()
}
