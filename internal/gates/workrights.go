package gates

import (
	"regexp"
	"strings"
)

var (
	// usOnlyRE matches a requirement only a US national or resident meets. "US" is matched
	// case-sensitively, so "us" in prose never counts.
	usOnlyRE = regexp.MustCompile(`\b(?:US|U\.S\.)\s+(?i:citizens?|persons?|residents?|nationals?)\s+(?i:only)\b|(?i:\bonly)\s+(?:US|U\.S\.)\s+(?i:citizens|persons|residents|nationals)\b|(?i:\bmust\s+be\s+an?)\s+(?:US|U\.S\.)\s+(?i:citizen|person|resident|national)\b`)
	// authorisedRE asks for the right to work somewhere; the places after it are read
	// whole, so "the United States or the European Union" is not US-only.
	authorisedRE = regexp.MustCompile(`(?i)\b(?:authori[sz]ed|eligible|permitted)\s+to\s+work\s+in\b`)
	// euRightsRE is the pass signal: the posting asks for EU work rights.
	euRightsRE = regexp.MustCompile(`\bEU\s+(?i:citizens?(?:hip)?|work\s+permit|passport)\b|(?i:\bright\s+to\s+work\s+in\s+the)\s+(?:EU\b|(?i:European\s+Union))`)
	// noSponsorRE says the employer will not sponsor a visa.
	noSponsorRE = regexp.MustCompile(`(?i)\b(?:visa\s+)?sponsorship\s+(?:is\s+)?(?:not|isn't|cannot\s+be)\s+(?:available|provided|offered)\b|\bunable\s+to\s+(?:offer|provide)\s+(?:visa\s+)?sponsorship\b|\bno\s+visa\s+sponsorship\b`)
)

func evalWorkRights(obs Observation, c CandidateGates) Result {
	text := obs.Description
	euCandidate, stated := holdsEURights(c)
	us := place{name: "the United States", country: "United States"}
	usAllowed := allowed(us, c.AllowedCountries)
	// holds reports whether the candidate can work in p.
	holds := func(p place) bool {
		if allowed(p, c.AllowedCountries) {
			return true
		}
		if !euCandidate {
			return false
		}
		if p.region != nil {
			return true // every region the gates know holds an EU country, or is "anywhere"
		}
		return allowed(p, eu)
	}

	if m := usOnlyRE.FindStringIndex(text); m != nil && !usAllowed {
		return Result{Gate: GateWorkRights, Verdict: Fail, Reason: "Only citizens or residents of the United States can apply.", Snippet: snippet(text, m[0], m[1])}
	}
	for _, m := range authorisedRE.FindAllStringIndex(text, -1) {
		phrase := phraseAfter(text, m[1])
		places := findPlaces(phrase)
		if len(places) == 0 || usAllowed {
			continue
		}
		onlyUS := true
		for _, p := range places {
			if p.country != "United States" {
				onlyUS = false
			}
			if holds(p) {
				onlyUS = false
				break
			}
		}
		if onlyUS {
			end := m[1] + len(phrase)
			return Result{Gate: GateWorkRights, Verdict: Fail, Reason: "Only people authorised to work in the United States can apply.", Snippet: snippet(text, m[0], end)}
		}
	}
	if m := euRightsRE.FindStringIndex(text); m != nil {
		if euCandidate {
			return Result{Gate: GateWorkRights, Verdict: Pass, Reason: "Asks for EU work rights, which you hold.", Snippet: snippet(text, m[0], m[1])}
		}
		reason := "Asks for EU work rights; none of your allowed countries is in the EU."
		if stated {
			reason = "Asks for EU work rights; your stated work rights do not name them."
		}
		return Result{Gate: GateWorkRights, Verdict: Flag, Reason: reason, Snippet: snippet(text, m[0], m[1])}
	}
	if m := noSponsorRE.FindStringIndex(text); m != nil {
		if euCandidate {
			return Result{Gate: GateWorkRights, Verdict: Pass, Reason: "No visa sponsorship, which an EU citizen does not need.", Snippet: snippet(text, m[0], m[1])}
		}
		return Result{Gate: GateWorkRights, Verdict: Flag, Reason: "No visa sponsorship; check whether you need one.", Snippet: snippet(text, m[0], m[1])}
	}
	return Result{Gate: GateWorkRights, Verdict: Pass, Reason: "No work-rights requirement found.", Snippet: opening(text)}
}

// euRightsStatedRE reads EU work rights in the candidate's own statement: "EU citizen",
// "European Union passport", "EU/EEA work permit".
var euRightsStatedRE = regexp.MustCompile(`\bEU\b|(?i:\bEuropean\s+Union\b)`)

// holdsEURights reports whether the candidate holds EU work rights and whether that comes
// from their stated WorkRights. Without a statement it is inferred: no country limit, or
// an allowed EU country.
func holdsEURights(c CandidateGates) (holds, stated bool) {
	if strings.TrimSpace(c.WorkRights) != "" {
		return euRightsStatedRE.MatchString(c.WorkRights), true
	}
	return !hasCountryLimit(c) || anyAllowed([]place{{region: eu}}, c.AllowedCountries), false
}
