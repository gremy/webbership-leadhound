package campaign

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Signals are a lead's stored signals that a pain's needs are judged on.
type Signals struct {
	Kind  string // the lead's kind (unsupported, d10, agency_partner …)
	Major int    // the finding's Drupal major; 0: unknown
}

// Need is one parsed condition of a pain's needs. The language is closed on purpose: a pain
// applies only on a signal the store holds, so Claude can never be handed a pain the lead's
// facts do not support.
//
//	finding.major in A..B   the major is between A and B, both included
//	finding.major == N      the major is N
//	kind == X               the lead's kind is X
//
// An unknown major (0) meets no finding.major need.
type Need struct {
	field  string // needMajor | needKind
	lo, hi int
	kind   string
}

const (
	needMajor = "finding.major"
	needKind  = "kind"
)

var (
	needMajorIn = regexp.MustCompile(`^finding\.major\s+in\s+(\d{1,2})\s*\.\.\s*(\d{1,2})$`)
	needMajorEq = regexp.MustCompile(`^finding\.major\s*==\s*(\d{1,2})$`)
	needKindEq  = regexp.MustCompile(`^kind\s*==\s*([a-z0-9_]{1,40})$`)
)

// ParseNeed reads one need; anything outside the three forms is an error.
func ParseNeed(s string) (Need, error) {
	t := strings.TrimSpace(s)
	if m := needMajorIn.FindStringSubmatch(t); m != nil {
		lo, _ := strconv.Atoi(m[1])
		hi, _ := strconv.Atoi(m[2])
		if lo < 1 || lo > hi {
			return Need{}, fmt.Errorf("need %q: the range %d..%d must run upwards from 1", s, lo, hi)
		}
		return Need{field: needMajor, lo: lo, hi: hi}, nil
	}
	if m := needMajorEq.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1])
		if n < 1 {
			return Need{}, fmt.Errorf("need %q: a major is 1 or more", s)
		}
		return Need{field: needMajor, lo: n, hi: n}, nil
	}
	if m := needKindEq.FindStringSubmatch(t); m != nil {
		return Need{field: needKind, kind: m[1]}, nil
	}
	return Need{}, fmt.Errorf("need %q is not one of: finding.major in A..B, finding.major == N, kind == X", s)
}

// Holds reports whether sig meets the need.
func (n Need) Holds(sig Signals) bool {
	switch n.field {
	case needMajor:
		return sig.Major != 0 && sig.Major >= n.lo && sig.Major <= n.hi
	case needKind:
		return sig.Kind == n.kind
	}
	return false
}

// PainsFor returns the offer's pains whose every need holds for sig, in spec order. A need
// that does not parse is an error (Check refuses such a spec first).
func (s Spec) PainsFor(sig Signals) ([]Pain, error) {
	var out []Pain
	for _, p := range s.Offer.Pains {
		ok := len(p.Needs) > 0
		for _, raw := range p.Needs {
			n, err := ParseNeed(raw)
			if err != nil {
				return nil, fmt.Errorf("campaign %q: offer.pains %q: %w", s.Slug, p.ID, err)
			}
			if !n.Holds(sig) {
				ok = false
			}
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// ResearchOpenerRegexps compiles copy.research_openers; nil when the spec lists none (the
// grounding check then uses its defaults).
func (s Spec) ResearchOpenerRegexps() ([]*regexp.Regexp, error) {
	if len(s.Copy.ResearchOpeners) == 0 {
		return nil, nil
	}
	out := make([]*regexp.Regexp, 0, len(s.Copy.ResearchOpeners))
	for _, pat := range s.Copy.ResearchOpeners {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("campaign %q: copy.research_openers %q: %w", s.Slug, pat, err)
		}
		out = append(out, re)
	}
	return out, nil
}
