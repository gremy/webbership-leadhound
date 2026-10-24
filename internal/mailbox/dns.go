package mailbox

import (
	"context"
	"errors"
	"net"
	"strings"
)

// Resolver looks up TXT records; *net.Resolver is one.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// DNSResult says which of the sending domain's records are in place. Note explains each
// failure in a few words and never names the domain.
type DNSResult struct {
	SPF, DKIM, DMARC bool
	Note             string
}

// OK is true when all three records are in place.
func (r DNSResult) OK() bool { return r.SPF && r.DKIM && r.DMARC }

// CheckDNS looks up the sending domain's SPF (a TXT on the domain starting v=spf1), DKIM
// (a TXT on <selector>._domainkey.<domain> starting v=DKIM1; a key split over several
// strings is joined) and DMARC (a TXT on _dmarc.<domain> starting v=DMARC1) records.
func CheckDNS(ctx context.Context, r Resolver, domain, selector string) DNSResult {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	selector = strings.TrimSpace(selector)
	if domain == "" {
		return DNSResult{Note: "no sending domain"}
	}
	if selector == "" || strings.ContainsAny(selector, ". \t") {
		return DNSResult{Note: "the dkim selector is not a single label"}
	}
	if err := ctx.Err(); err != nil {
		return DNSResult{Note: err.Error()}
	}
	var res DNSResult
	var notes []string
	check := func(name, label, prefix string, joined bool) bool {
		txt, err := r.LookupTXT(ctx, name)
		if err != nil {
			notes = append(notes, label+": "+lookupProblem(ctx, err))
			return false
		}
		for _, t := range txt {
			if hasPrefixFold(t, prefix) {
				return true
			}
		}
		if joined && hasPrefixFold(strings.Join(txt, ""), prefix) {
			return true
		}
		notes = append(notes, label+": no TXT record starting "+prefix)
		return false
	}
	res.SPF = check(domain, "spf", "v=spf1", false)
	res.DKIM = check(selector+"._domainkey."+domain, "dkim", "v=DKIM1", true)
	res.DMARC = check("_dmarc."+domain, "dmarc", "v=DMARC1", false)
	res.Note = strings.Join(notes, "; ")
	return res
}

// hasPrefixFold: s, leading space trimmed, starts with prefix in any letter case and the
// version tag ends there.
func hasPrefixFold(s, prefix string) bool {
	s = strings.TrimLeft(s, " \t")
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return false
	}
	rest := s[len(prefix):]
	return rest == "" || rest[0] == ' ' || rest[0] == ';' || rest[0] == '\t'
}

// lookupProblem names a failed lookup in words that hold no host name.
func lookupProblem(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return ctx.Err().Error()
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		switch {
		case de.IsNotFound:
			return "not found"
		case de.IsTimeout:
			return "lookup failed (timeout)"
		}
	}
	return "lookup failed"
}
