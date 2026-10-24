// Package robots parses robots.txt files and answers whether a path may be fetched.
//
// Allowed uses the group named for the crawler's product token when one exists, else
// the "*" group, and applies the longest matching Allow or Disallow rule, with "*" as a
// wildcard and a trailing "$" anchoring the end of the path. A tie goes to Allow.
package robots

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
)

// MaxBytes is how much of a robots.txt body Parse reads; the rest is ignored.
const MaxBytes = 512 << 10

// Verdict is what a robots.txt response means for the site it came from.
type Verdict string

// The verdicts Decide returns.
const (
	VerdictAllowedAll Verdict = "allowed_all" // no robots.txt: every path is allowed
	VerdictRules      Verdict = "rules"       // the body holds the rules to apply
	VerdictSkip       Verdict = "skip"        // any other answer: fetch nothing from the site
)

// Decide maps a robots.txt fetch to a Verdict: a transport error skips the site, 404 and
// 410 allow every path, a 2xx means the body holds the rules, and any other status skips.
func Decide(status int, _ []byte, err error) Verdict {
	switch {
	case err != nil:
		return VerdictSkip
	case status == http.StatusNotFound || status == http.StatusGone:
		return VerdictAllowedAll
	case status >= 200 && status < 300:
		return VerdictRules
	default:
		return VerdictSkip
	}
}

// Rules is a parsed robots.txt. A nil *Rules allows every path.
type Rules struct {
	groups []group
}

type group struct {
	agents []string // lower-case product tokens; "*" for every crawler
	rules  []rule
}

type rule struct {
	allow   bool
	pattern string
}

// Parse reads the groups and their Allow and Disallow rules from a robots.txt body. It
// reads at most MaxBytes, drops a line the cap cuts, and ignores lines it does not know.
func Parse(body []byte) *Rules {
	if len(body) > MaxBytes {
		body = body[:MaxBytes]
		if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
			body = body[:i]
		} else {
			body = nil
		}
	}
	body = bytes.TrimPrefix(body, []byte("\ufeff"))

	r := &Rules{}
	var cur *group
	lastWasAgent := false
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if cur == nil || !lastWasAgent {
				r.groups = append(r.groups, group{})
				cur = &r.groups[len(r.groups)-1]
			}
			cur.agents = append(cur.agents, productToken(value))
			lastWasAgent = true
		case "allow", "disallow":
			lastWasAgent = false
			if cur == nil || value == "" {
				continue // a rule outside a group, or an empty rule, allows everything
			}
			cur.rules = append(cur.rules, rule{allow: key == "allow", pattern: canonical(value)})
		default:
			// Other lines (Sitemap, Crawl-delay and the like) neither start nor end a group.
		}
	}
	return r
}

// Allowed reports whether the crawler named by userAgent may fetch path, which is the
// URL path with its query. The crawler's groups are the ones naming its product token
// (the part before the first "/" or space, compared without case), else the "*" groups.
func (r *Rules) Allowed(userAgent, path string) bool {
	if r == nil {
		return true
	}
	if path == "" {
		path = "/"
	}
	path = canonical(path)
	rules := r.rulesFor(productToken(userAgent))
	best, allowed := -1, true
	for _, ru := range rules {
		n := len(ru.pattern)
		if n < best || !match(ru.pattern, path) {
			continue
		}
		if n > best {
			best, allowed = n, ru.allow
		} else {
			allowed = allowed || ru.allow
		}
	}
	return allowed
}

// rulesFor returns the rules of every group naming token, else of every "*" group. A
// crawler named by a group with no rules gets no rules, so every path is allowed.
func (r *Rules) rulesFor(token string) []rule {
	var named, star []rule
	hasNamed := false
	for _, g := range r.groups {
		switch {
		case token != "" && slices.Contains(g.agents, token):
			hasNamed = true
			named = append(named, g.rules...)
		case slices.Contains(g.agents, "*"):
			star = append(star, g.rules...)
		}
	}
	if hasNamed {
		return named
	}
	return star
}

// canonical percent-encodes every byte outside US-ASCII and upper-cases the hex digits
// of existing %XX escapes, so patterns and paths compare in one form. Reserved
// characters and escapes of them (such as %2F) are left as they are.
func canonical(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte('%')
			b.WriteByte(upper(s[i+1]))
			b.WriteByte(upper(s[i+2]))
			i += 2
		case c >= 0x80:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func upper(c byte) byte {
	if 'a' <= c && c <= 'f' {
		return c - 'a' + 'A'
	}
	return c
}

// productToken returns the lower-case name before the first "/" or space.
func productToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "/ \t"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}

// match reports whether pattern matches path from its start. "*" matches any run of
// bytes, and a trailing "$" means the pattern must reach the end of path.
func match(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	if len(parts) == 1 {
		return !anchored || len(path) == len(parts[0])
	}
	pos := len(parts[0])
	last := len(parts) - 1
	for _, p := range parts[1:last] {
		i := strings.Index(path[pos:], p)
		if i < 0 {
			return false
		}
		pos += i + len(p)
	}
	tail := parts[last]
	if anchored {
		return len(path)-len(tail) >= pos && strings.HasSuffix(path, tail)
	}
	return strings.Contains(path[pos:], tail)
}
