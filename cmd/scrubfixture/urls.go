package main

import (
	"net/url"
	"strings"
)

// The synthetic hosts external links are rewritten to (RFC 2606 reserved names).
const (
	companyWebsite = "https://www.northwind.example"
	staticAsset    = "https://static.example/asset.svg"
)

// safeFirstSegments are LinkedIn paths that name no job, company or person; a link under
// one of them keeps that first segment and loses the rest.
var safeFirstSegments = map[string]bool{
	"login": true, "uas": true, "signup": true, "legal": true, "help": true,
	"accessibility": true, "psettings": true, "checkpoint": true, "authwall": true,
}

// rewriteURL returns the synthetic form of a URL. ok is false when the attribute should be
// dropped (a personal profile link).
func (s *Scrubber) rewriteURL(raw string) (out string, ok bool) {
	v := strings.TrimSpace(raw)
	if v == "" || strings.HasPrefix(v, "#") {
		return v, true
	}
	if strings.Contains(v, profileMarker) || strings.Contains(strings.ToLower(v), "/pub/") {
		return "", false
	}
	if strings.HasPrefix(v, "//") {
		v = "https:" + v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", false
	}
	if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	relative := host == ""
	switch {
	case relative || host == "linkedin.com" || strings.HasSuffix(host, ".linkedin.com"):
		p := s.linkedInPath(u.EscapedPath())
		if relative || strings.HasPrefix(p, "https://") {
			return p, true
		}
		return "https://www.linkedin.com" + p, true
	case host == "licdn.com" || strings.HasSuffix(host, ".licdn.com"):
		return staticAsset, true
	default:
		return companyWebsite, true
	}
}

// linkedInPath maps a LinkedIn path to its synthetic form; the query is always dropped.
func (s *Scrubber) linkedInPath(p string) string {
	if m := jobViewRe.FindStringSubmatch(p); m != nil {
		return "https://www.linkedin.com/jobs/view/senior-platform-engineer-" + s.fakeID(m[1])
	}
	segs := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case len(segs) >= 2 && segs[0] == "company":
		return "https://www.linkedin.com/company/" + slugify(s.companyName(companySlugKey(segs[1])))
	case len(segs) >= 1 && segs[0] == "redir":
		return companyWebsite
	case len(segs) >= 2 && segs[0] == "jobs" && segs[1] == "search":
		return "/jobs/search"
	case len(segs) >= 1 && safeFirstSegments[segs[0]]:
		return "/" + segs[0]
	case len(segs) >= 1 && segs[0] == "jobs":
		return "/jobs"
	}
	return "/"
}

// companySlugKey is the key a real company slug is stored under: unescaped, lower case.
func companySlugKey(slug string) string {
	if u, err := url.PathUnescape(slug); err == nil {
		slug = u
	}
	return strings.ToLower(slug)
}
