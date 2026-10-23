package main

import (
	"bytes"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Originals are the identifying values found in a raw page, which its scrubbed form must
// not contain.
type Originals struct {
	JobIDs    []string
	Slugs     []string
	Companies []string
}

var currentJobRe = regexp.MustCompile(`currentJobId=(\d+)`)

// CollectOriginals reads the job IDs, company slugs and company names out of a raw page.
func CollectOriginals(raw []byte) Originals {
	ids := map[string]bool{}
	for _, re := range []*regexp.Regexp{jobURNRe, jobViewRe, currentJobRe} {
		for _, m := range re.FindAllSubmatch(raw, -1) {
			ids[string(m[1])] = true
		}
	}
	slugs := map[string]bool{}
	for _, m := range companyHrefRe.FindAllSubmatch(raw, -1) {
		slugs[companySlugKey(string(m[1]))] = true
	}
	names := map[string]bool{}
	if _, nodes, _, err := parse(raw); err == nil {
		for _, n := range nodes {
			walk(n, func(e *html.Node) {
				if hasClass(e, "topcard__org-name-link") || (e.Data == "a" && hasClass(e.Parent, "base-search-card__subtitle")) {
					if t := strings.Join(strings.Fields(textOf(e)), " "); t != "" {
						names[t] = true
					}
				}
			})
		}
	}
	return Originals{JobIDs: sorted(ids), Slugs: sorted(slugs), Companies: sorted(names)}
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Leaks returns the rules a scrubbed page breaks, each once. It names rules only, never
// the matched value.
func Leaks(out []byte, orig Originals) []string {
	var rules []string
	lo := bytes.ToLower(out)
	add := func(ok bool, rule string) {
		if ok {
			rules = append(rules, rule)
		}
	}
	add(bytes.Contains(lo, []byte(recruiterClass)), "recruiter-class")
	add(bytes.Contains(lo, []byte(profileMarker)), "profile-link")
	add(emailLikeRe.Match(out) && hasRealEmail(out), "email")
	add(hasPublicIPv4(out), "ipv4")
	add(containsAny(out, orig.JobIDs, false), "original-job-id")
	add(containsAny(out, orig.Slugs, true), "original-company-slug")
	add(containsAny(out, orig.Companies, true), "original-company-name")
	for _, k := range []string{"trk=", "refid=", "trackingid=", "session_redirect="} {
		if bytes.Contains(lo, []byte(k)) {
			rules = append(rules, "tracking-param")
			break
		}
	}
	return rules
}

func hasRealEmail(out []byte) bool {
	for _, m := range emailLikeRe.FindAll(out, -1) {
		if !bytes.HasSuffix(bytes.ToLower(m), []byte(".invalid")) && !bytes.HasSuffix(bytes.ToLower(m), []byte(".example")) {
			return true
		}
	}
	return false
}

func hasPublicIPv4(out []byte) bool {
	for _, m := range ipv4LikeRe.FindAll(out, -1) {
		if !bytes.HasPrefix(m, []byte("192.0.2.")) {
			return true
		}
	}
	return false
}

// containsAny looks for any value, case-insensitively; with words set, a hit must not be
// glued to a letter or digit on either side (so a short slug inside a longer word is not a
// leak).
func containsAny(out []byte, values []string, words bool) bool {
	lo := strings.ToLower(string(out))
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		for i := 0; ; {
			j := strings.Index(lo[i:], v)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(v)
			if !words || (!wordRuneBefore(lo, start) && !wordRuneAfter(lo, end)) {
				return true
			}
			i = start + 1
		}
	}
	return false
}

func wordRuneBefore(s string, i int) bool {
	if i == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

func wordRuneAfter(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
