package outreach

import (
	"strings"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// SuppressionSet holds the suppression rows in the shape Suppressed matches against:
// addresses in lower case, domains as their registrable domain. The zero value is an
// empty set that suppresses nothing.
type SuppressionSet struct {
	addresses map[string]bool
	domains   map[string]bool
}

// NewSuppressionSet builds a set from the address and domain suppression values.
func NewSuppressionSet(addresses, domains []string) SuppressionSet {
	s := SuppressionSet{addresses: map[string]bool{}, domains: map[string]bool{}}
	for _, a := range addresses {
		s.AddAddress(a)
	}
	for _, d := range domains {
		s.AddDomain(d)
	}
	return s
}

// AddAddress adds one address suppression.
func (s *SuppressionSet) AddAddress(addr string) {
	if a := suppNorm(addr); a != "" {
		if s.addresses == nil {
			s.addresses = map[string]bool{}
		}
		s.addresses[a] = true
	}
}

// AddDomain adds one domain suppression. It is stored as its registrable domain, so a
// suppression on a subdomain covers the whole organisation: suppression errs wide.
func (s *SuppressionSet) AddDomain(domain string) {
	if d := suppDomain(domain); d != "" {
		if s.domains == nil {
			s.domains = map[string]bool{}
		}
		s.domains[d] = true
	}
}

// Suppressed reports whether a message to addr, for an org on domain, is suppressed: the
// address itself is suppressed, or the registrable domain of either the address's domain
// or domain is. Either argument may be empty. Matching ignores case.
func Suppressed(addr, domain string, set SuppressionSet) bool {
	a := suppNorm(addr)
	if a != "" && set.addresses[a] {
		return true
	}
	if at := strings.LastIndexByte(a, '@'); at >= 0 {
		if d := suppDomain(a[at+1:]); d != "" && set.domains[d] {
			return true
		}
	}
	d := suppDomain(domain)
	return d != "" && set.domains[d]
}

func suppNorm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// suppDomain is domain's registrable domain, or its lower-case form when it has none.
func suppDomain(domain string) string {
	d := strings.TrimSuffix(suppNorm(domain), ".")
	if d == "" {
		return ""
	}
	if r, err := sitelist.Registrable(d); err == nil {
		return r
	}
	return d
}
