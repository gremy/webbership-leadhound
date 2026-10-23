package main

import (
	"net/netip"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Rule names. They are all a finding ever prints: the matched text never leaves the process.
const (
	ruleDenylist       = "denylist"
	ruleEmail          = "email"
	ruleProfile        = "linkedin-profile"
	ruleIPv4           = "ipv4"
	ruleRecruiter      = "recruiter-class"
	ruleNotAllowlisted = "not-allowlisted"
	ruleBinaryDocument = "binary-document"
)

// checker holds the private lists. deny entries are normalised (see normalize). allow and
// allowNames are used only for identity headers (author, committer, tagger) in history
// mode, never for content.
type checker struct {
	deny       []string
	allow      map[string]bool // identity emails, lower-cased
	allowNames map[string]bool // identity names, normalised
}

// allowNamePrefix marks an allow.txt line as an identity name rather than an email.
const allowNamePrefix = "name:"

func newChecker(deny []string, allow map[string]bool) *checker {
	c := &checker{allow: map[string]bool{}, allowNames: map[string]bool{}}
	for _, d := range deny {
		if d = normalize(d); d != "" {
			c.deny = append(c.deny, d)
		}
	}
	for a, ok := range allow {
		if !ok {
			continue
		}
		a = strings.TrimSpace(a)
		if len(a) >= len(allowNamePrefix) && strings.EqualFold(a[:len(allowNamePrefix)], allowNamePrefix) {
			if n := normalize(a[len(allowNamePrefix):]); n != "" {
				c.allowNames[n] = true
			}
			continue
		}
		if a != "" {
			c.allow[strings.ToLower(a)] = true
		}
	}
	return c
}

// foldExtra covers Latin letters whose diacritic is not a combining mark under NFD.
var foldExtra = map[rune]rune{'ł': 'l', 'Ł': 'l', 'đ': 'd', 'Đ': 'd', 'ø': 'o', 'Ø': 'o', 'ħ': 'h', 'Ħ': 'h'}

// normalize makes denylist matching blind to case, diacritics and separators: it
// decomposes (NFD), drops combining marks (so ș, ş, ț, ţ, ă, â, î fold to s, t, a, i),
// lower-cases, and collapses every run of whitespace, dots, underscores and hyphens to one
// space, trimmed at both ends. "First.Last", "first-last" and "FIRST  LAST" all become
// "first last".
func normalize(s string) string {
	var b strings.Builder
	sep := false
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsSpace(r) || r == '.' || r == '_' || r == '-' {
			sep = true
			continue
		}
		if sep && b.Len() > 0 {
			b.WriteByte(' ')
		}
		sep = false
		if f, ok := foldExtra[r]; ok {
			r = f
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

var (
	emailRe     = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}`)
	dottedNumRe = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)+`)
	retinaRe    = regexp.MustCompile(`^[0-9]+x\.[a-z]+$`)
)

// reservedTLDs and reservedDomains are the RFC 2606 names test data may use freely.
var (
	reservedTLDs    = []string{"example", "invalid", "test", "localhost"}
	reservedDomains = []string{"example.com", "example.org", "example.net"}
)

// documentation and loopback ranges (RFC 5737, RFC 1122) that may appear in the repo.
var allowedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("127.0.0.0/8"),
}

// profilePath is split so this file does not trip its own rule.
const profilePath = "linkedin.com/" + "in/"

// scanLine returns the rules a line of content trips, each at most once.
func (c *checker) scanLine(path, line string) []string {
	var rules []string
	if c.denyHit(line) {
		rules = append(rules, ruleDenylist)
	}
	if hasBlockedEmail(line) {
		rules = append(rules, ruleEmail)
	}
	if strings.Contains(strings.ToLower(line), profilePath) {
		rules = append(rules, ruleProfile)
	}
	if hasPublicIPv4(line) {
		rules = append(rules, ruleIPv4)
	}
	if inTestdata(path) && strings.Contains(strings.ToLower(line), "message-the-recruiter") {
		rules = append(rules, ruleRecruiter)
	}
	return rules
}

// scanName checks a file path itself (an added or tracked file name).
func (c *checker) scanName(path string) []string {
	return c.scanLine("", path)
}

func (c *checker) denyHit(s string) bool {
	if len(c.deny) == 0 {
		return false
	}
	n := normalize(s)
	for _, d := range c.deny {
		if strings.Contains(n, d) {
			return true
		}
	}
	return false
}

func hasBlockedEmail(s string) bool {
	for _, m := range emailRe.FindAllString(s, -1) {
		if !emailExempt(m) {
			return true
		}
	}
	return false
}

// emailExempt: reserved names, noreply addresses, the ssh remote form git@github.com,
// and retina asset names such as logo@2x.png, which are not addresses at all.
func emailExempt(addr string) bool {
	addr = strings.ToLower(addr)
	_, domain, _ := strings.Cut(addr, "@")
	if isNoreply(addr) || addr == "git@github.com" || retinaRe.MatchString(domain) {
		return true
	}
	tld := domain[strings.LastIndexByte(domain, '.')+1:]
	for _, r := range reservedTLDs {
		if tld == r {
			return true
		}
	}
	for _, r := range reservedDomains {
		if domain == r || strings.HasSuffix(domain, "."+r) {
			return true
		}
	}
	return false
}

func isNoreply(addr string) bool {
	local, domain, _ := strings.Cut(strings.ToLower(addr), "@")
	return local == "noreply" || domain == "users.noreply.github.com"
}

// hasPublicIPv4 finds word-bounded dotted quads (exactly four parts, each 0-255, no
// leading zeros) outside the allowed ranges. A run with a letter, digit, dot or
// underscore glued to its front (v1.2.3.4) or a letter after it is not an address;
// five-part runs (1.2.3.4.5) are version numbers. 0.0.0.0 (the bind-all address)
// names no machine, so it passes. A product version such as Chrome/124.0.0.0 (letters
// and a slash right before it) or one after "version " is not an address either; a URL
// host (http:// then an address) still is, because a slash precedes the slash there.
func hasPublicIPv4(s string) bool {
	for _, loc := range dottedNumRe.FindAllStringIndex(s, -1) {
		start, end := loc[0], loc[1]
		if start > 0 && isWordish(s[start-1]) {
			continue
		}
		if isVersionContext(s[:start]) {
			continue
		}
		if end < len(s) && isWordish(s[end]) && s[end] != '.' {
			continue
		}
		cand := s[start:end]
		if strings.Count(cand, ".") != 3 {
			continue
		}
		addr, err := netip.ParseAddr(cand)
		if err != nil || !addr.Is4() || addr.IsUnspecified() {
			continue
		}
		allowed := false
		for _, p := range allowedPrefixes {
			if p.Contains(addr) {
				allowed = true
			}
		}
		if !allowed {
			return true
		}
	}
	return false
}

// isVersionContext reports whether the text before a dotted number marks it as a version:
// it ends in "<letters>/" or in "version " (any case).
func isVersionContext(before string) bool {
	if strings.HasSuffix(strings.ToLower(before), "version ") {
		return true
	}
	if !strings.HasSuffix(before, "/") {
		return false
	}
	before = before[:len(before)-1]
	i := len(before)
	for i > 0 && isLetter(before[i-1]) {
		i--
	}
	return i < len(before)
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

func isWordish(b byte) bool {
	return b == '.' || b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func inTestdata(path string) bool {
	for _, part := range strings.Split(strings.ReplaceAll(path, `\`, "/"), "/") {
		if part == "testdata" {
			return true
		}
	}
	return false
}

// blockedExtensions are document, archive, dump, mail and contact formats that never belong
// in this repo whatever they hold, so they are blocked by name alone.
var blockedExtensions = []string{
	".pdf", ".doc", ".docx", ".xls", ".xlsx", ".odt", ".ods", ".rtf",
	".zip", ".gz", ".tgz", ".7z", ".rar", ".dump", ".sql.gz",
	".eml", ".msg", ".vcf", ".heic",
}

func hasBlockedExtension(path string) bool {
	lo := strings.ToLower(path)
	for _, ext := range blockedExtensions {
		if strings.HasSuffix(lo, ext) {
			return true
		}
	}
	return false
}

// isBinary is git's own test: a NUL in the first 8000 bytes.
func isBinary(data []byte) bool {
	return strings.IndexByte(string(data[:min(len(data), 8000)]), 0) >= 0
}

// minRun is the shortest text run taken from binary content (the strings(1) default).
const minRun = 4

// textRuns pulls readable text out of binary content: runs of printable UTF-8 (which
// covers ASCII, and EXIF or metadata text in images) and runs of UTF-16 in both byte orders
// and both alignments (Office and Windows metadata).
func textRuns(data []byte) []string {
	var runs []string
	var cur []rune
	flush := func() {
		if len(cur) >= minRun {
			runs = append(runs, string(cur))
		}
		cur = cur[:0]
	}
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError && (r == '\t' || unicode.IsPrint(r)) {
			cur = append(cur, r)
		} else {
			flush()
		}
		i += size
	}
	flush()
	for _, bigEndian := range []bool{false, true} {
		for off := 0; off < 2; off++ {
			for i := off; i+1 < len(data); i += 2 {
				u := rune(data[i]) | rune(data[i+1])<<8
				if bigEndian {
					u = rune(data[i])<<8 | rune(data[i+1])
				}
				if isUTF16Text(u) {
					cur = append(cur, u)
				} else {
					flush()
				}
			}
			flush()
		}
	}
	return runs
}

// isUTF16Text accepts the Latin script range (with combining marks) only, so random binary
// bytes read as UTF-16 do not turn into long runs of unrelated characters.
func isUTF16Text(r rune) bool {
	if r == '\t' {
		return true
	}
	return (r >= 0x20 && r < 0x250 || r >= 0x300 && r < 0x370) && unicode.IsPrint(r)
}
