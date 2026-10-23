package gates

import (
	"fmt"
	"regexp"
	"strings"
)

// locationPattern is one residence or presence requirement.
type locationPattern struct {
	re *regexp.Regexp
	// says completes "Asks candidates to …" for the reason.
	says string
	// place is the place the pattern itself implies ("Spain" for a DNI/NIE), or "".
	place string
	// needPlace patterns are ignored when the phrase after them names no known place.
	needPlace bool
	// exclusionary patterns fail even when no place can be read.
	exclusionary bool
	// negatable patterns are ignored after "no"/"not"/"without", or when followed by
	// "is not required" and the like.
	negatable bool
	// flagOnly patterns flag at most: an offer or a request to move is the candidate's call.
	flagOnly bool
}

var locationPatterns = []locationPattern{
	{re: regexp.MustCompile(`(?i)\bmust\s+(?:be\s+based|be\s+located|reside|live)\s+in\b`), says: "be based in"},
	{re: regexp.MustCompile(`(?i)\b(?:only|exclusively)\s+(?:for\s+)?candidates\s+(?:based|located|living|residing)\s+in\b`), says: "be based in"},
	{re: regexp.MustCompile(`\b(?:DNI|NIE)\b`), says: "hold a Spanish DNI or NIE", place: "Spain", exclusionary: true},
	{re: regexp.MustCompile(`(?i)\bright\s+to\s+work\s+in\b`), says: "have the right to work in", needPlace: true},
	{re: usOnlyLocationRE, says: "be a US citizen or resident", place: "US", exclusionary: true},
	{re: clearanceRE, says: "hold a security clearance", exclusionary: true, negatable: true},
	{re: regexp.MustCompile(`(?i)\brelocat(?:e|ion)\s+(?:to\b|required\b)`), says: "relocate", negatable: true, flagOnly: true},
}

var (
	// usOnlyLocationRE: "US citizens only", "open to US residents only", "only US persons".
	// "US" is matched case-sensitively, so "us" in prose never counts.
	usOnlyLocationRE = regexp.MustCompile(`\b(?:US|U\.S\.)\s+(?i:citizens?|persons?|residents?|nationals?)\s+(?i:only)\b|(?i:\bonly)\s+(?:US|U\.S\.)\s+(?i:citizens|persons|residents|nationals)\b`)
	// clearanceRE asks for a named security clearance; "customs clearance" is not one.
	clearanceRE = regexp.MustCompile(`(?i:\b(?:security|secret|top\s+secret)\s+clearance\b)|\b(?:SC|DV|NV1|NV2)\s+(?i:clearance|cleared)\b|\bTS/SCI\b`)
)

var remoteFromRE = regexp.MustCompile(`(?i)\bremote\s+(?:only\s+)?from\b`)

// hasCountryLimit reports whether the candidate named any allowed country; none means
// no limit, so a place can raise a flag but never fail the posting.
func hasCountryLimit(c CandidateGates) bool {
	for _, a := range c.AllowedCountries {
		if strings.TrimSpace(a) != "" {
			return true
		}
	}
	return false
}

func evalLocation(obs Observation, c CandidateGates) Result {
	text := obs.Description
	limited := hasCountryLimit(c)
	var fs []finding
	for _, p := range locationPatterns {
		for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
			if p.negatable && (negated(text, m[0]) || negatedAfter(text, m[1])) {
				continue
			}
			if f, ok := locationFinding(text, m, p, c, limited); ok {
				fs = append(fs, f)
			}
		}
	}
	for _, m := range remoteFromRE.FindAllStringIndex(text, -1) {
		phrase := phraseAfter(text, m[1])
		places := findPlaces(phrase)
		if len(places) == 0 || anyAllowed(places, c.AllowedCountries) || (!limited && regionOnly(places)) {
			continue
		}
		end := m[1] + len(phrase)
		f := finding{
			verdict: Fail,
			reason:  fmt.Sprintf("Remote only from %s, outside your allowed countries.", placeNames(places)),
			snippet: snippet(text, m[0], end),
			pos:     m[0],
		}
		if !limited {
			f.verdict = Flag
			f.reason = fmt.Sprintf("Remote only from %s; check that you can work from there.", placeNames(places))
		}
		fs = append(fs, f)
	}
	if f, ok := worst(fs); ok {
		return result(GateLocation, f)
	}
	return Result{Gate: GateLocation, Verdict: Pass, Reason: "No residence or location requirement found.", Snippet: opening(text)}
}

func locationFinding(text string, m []int, p locationPattern, c CandidateGates, limited bool) (finding, bool) {
	end := m[1]
	var places []place
	unknown := false
	if p.place != "" {
		places = findPlaces(p.place)
	} else {
		phrase := phraseAfter(text, m[1])
		if phrase != "" {
			end = m[1] + strings.Index(text[m[1]:], phrase) + len(phrase)
		}
		places = findPlaces(phrase)
		if len(places) == 0 && !p.needPlace {
			if name := properNoun(phrase); name != "" {
				places = []place{{name: name}}
				unknown = true
			}
		}
	}
	if len(places) == 0 && p.needPlace {
		return finding{}, false
	}
	f := finding{snippet: snippet(text, m[0], end), pos: m[0]}
	switch {
	case len(places) == 0 && p.exclusionary:
		f.verdict = Fail
		f.reason = fmt.Sprintf("Asks candidates to %s.", p.says)
	case len(places) == 0:
		f.verdict = Flag
		f.reason = fmt.Sprintf("Asks candidates to %s a place it does not name clearly; check where.", p.says)
		if p.says == "relocate" {
			f.reason = "Asks candidates to relocate."
		}
	case unknown:
		f.verdict = Flag
		f.reason = fmt.Sprintf("Asks candidates to %s %s, a place the gates do not know; check where.", sayIn(p.says), placeNames(places))
	case !limited && regionOnly(places):
		f.verdict = Pass
		f.reason = fmt.Sprintf("Asks candidates to %s %s; you have set no country limit.", sayIn(p.says), placeNames(places))
	case !limited:
		f.verdict = Flag
		f.reason = fmt.Sprintf("Asks candidates to %s %s; you have set no country limit, so check.", sayIn(p.says), placeNames(places))
	case allowedViaRegion(places, c.AllowedCountries):
		f.verdict = Pass
		f.reason = fmt.Sprintf("Asks candidates to %s %s, which includes your allowed countries.", sayIn(p.says), placeNames(places))
	case anyAllowed(places, c.AllowedCountries):
		f.verdict = Flag
		f.reason = fmt.Sprintf("Asks candidates to %s %s; check the details.", sayIn(p.says), placeNames(places))
	case p.flagOnly:
		f.verdict = Flag
		f.reason = fmt.Sprintf("Asks candidates to %s %s; your call.", sayIn(p.says), placeNames(places))
	default:
		f.verdict = Fail
		f.reason = fmt.Sprintf("Asks candidates to %s %s, outside your allowed countries.", sayIn(p.says), placeNames(places))
	}
	return f, true
}

// sayIn adds "to" or a link word where the phrase needs one before a place.
func sayIn(says string) string {
	switch says {
	case "relocate":
		return "relocate to"
	case "hold a Spanish DNI or NIE", "be a US citizen or resident", "hold a security clearance":
		return says + ", tied to"
	}
	return says
}

func anyAllowed(ps []place, allowedCountries []string) bool {
	for _, p := range ps {
		if allowed(p, allowedCountries) {
			return true
		}
	}
	return false
}

// allowedViaRegion reports whether one of ps is a region holding an allowed country:
// "the UK or the EU" is open to a candidate allowed in an EU country.
func allowedViaRegion(ps []place, allowedCountries []string) bool {
	for _, p := range ps {
		if p.region != nil && allowed(p, allowedCountries) {
			return true
		}
	}
	return false
}

func regionOnly(ps []place) bool {
	for _, p := range ps {
		if p.region == nil {
			return false
		}
	}
	return true
}

func placeNames(ps []place) string {
	names := make([]string, 0, len(ps))
	seen := map[string]bool{}
	for _, p := range ps {
		if !seen[p.name] {
			seen[p.name] = true
			names = append(names, p.name)
		}
	}
	return strings.Join(names, " or ")
}
