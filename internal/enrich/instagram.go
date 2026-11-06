package enrich

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// instagramHandleRe is what an Instagram handle is once lower-cased: the shape the
// ig_* tables' handle CHECKs hold (migration 00031).
var instagramHandleRe = regexp.MustCompile(`^[a-z0-9._]{1,30}$`)

// instagramDomains are Instagram's registrable domains, the short one included.
var instagramDomains = map[string]bool{"instagram.com": true, "instagr.am": true}

// InstagramHandle returns the handle raw names, lower case and without its @: raw is a
// profile URL (any scheme-less, www or m. form, with or without a trailing slash, query or
// fragment; only the first path segment counts), "@handle", or a bare handle. A URL on
// another host, Instagram's own pages (/p/, /reel/, /explore/, /stories/ and the like), a
// port or a login in the URL, or a segment not shaped like a handle is not ok, and neither is
// Instagram's own host where a handle would be ("instagram.com", "www.instagram.com.", the
// probe's inline-script needle): a site, not a handle, and Instagram allows no handle naming it. It is the one rule every reader of a handle
// uses (the route backfill in 00031 mirrors it in SQL).
func InstagramHandle(raw string) (handle string, ok bool) {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "/") {
		return instagramSegment(strings.TrimPrefix(s, "@"))
	}
	switch {
	case strings.HasPrefix(s, "//"):
		s = "https:" + s
	case !strings.Contains(s, "://"):
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Port() != "" {
		return "", false
	}
	reg, err := sitelist.Registrable(u.Hostname())
	if err != nil || !instagramDomains[reg] {
		return "", false
	}
	for seg := range strings.SplitSeq(u.EscapedPath(), "/") {
		if seg != "" {
			return instagramSegment(seg)
		}
	}
	return "", false
}

// instagramSegment is InstagramHandle's test of one candidate handle.
func instagramSegment(s string) (string, bool) {
	h := strings.ToLower(s)
	if !instagramHandleRe.MatchString(h) || strings.Trim(h, ".") == "" || notProfile["instagram.com"][h] || instagramHost(h) {
		return "", false
	}
	return h, true
}

// instagramHost reports whether h, read as a host name, is on one of Instagram's own domains.
func instagramHost(h string) bool {
	if !strings.Contains(h, ".") {
		return false
	}
	reg, err := sitelist.Registrable(h)
	return err == nil && instagramDomains[reg]
}

// InstagramURL is the one canonical address of a handle's profile,
// https://www.instagram.com/<handle>/: the value of every instagram contact route. handle may
// be in any form InstagramHandle reads; one it does not read gives "".
func InstagramURL(handle string) string {
	h, ok := InstagramHandle(handle)
	if !ok {
		return ""
	}
	return "https://www.instagram.com/" + h + "/"
}

// InstagramRoute is the instagram route value a fact gives, beside LinkedInRoute: a social fact
// holding an Instagram profile gives the handle's canonical address (InstagramURL), and any other
// fact none. A social fact's value is always a URL, so a fact holding a bare word is no route.
func InstagramRoute(f Fact) (value string, ok bool) {
	if f.Kind != KindSocial || !strings.Contains(f.Normalized, "/") {
		return "", false
	}
	h, ok := InstagramHandle(f.Normalized)
	if !ok {
		return "", false
	}
	return InstagramURL(h), true
}
