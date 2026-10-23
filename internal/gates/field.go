package gates

import (
	"fmt"
	"regexp"
	"strings"
)

// Workplace kinds a posting's location field can state.
const (
	workRemote = "remote"
	workHybrid = "hybrid"
	workOnsite = "on-site"
)

var (
	// atsLabelRE is the label the ATS collectors append: "Lisbon (On-site)".
	atsLabelRE   = regexp.MustCompile(`(?i)\((remote|hybrid|on-site)\)\s*$`)
	remoteWordRE = regexp.MustCompile(`(?i)\b(?:remote|home[- ]based|work\s+from\s+home|WFH)\b`)
	hybridWordRE = regexp.MustCompile(`(?i)\bhybrid\b`)
	onsiteWordRE = regexp.MustCompile(`(?i)\b(?:on[- ]?site|in[- ]office)\b`)
	// globalRE reads "Global" as anywhere, in the location field only: in a description
	// "a global company" is not a place.
	globalRE = regexp.MustCompile(`(?i)\bglobal(?:ly)?\b`)
	// workplaceOnlyRE is a segment that holds only workplace words, not a place name.
	workplaceOnlyRE = regexp.MustCompile(`(?i)^(?:remote|hybrid|on[- ]?site|in[- ]office|home[- ]based|fully\s+remote|work\s+from\s+home)$`)
)

// fieldReading is what a posting's location field says about where the work is.
type fieldReading struct {
	text   string  // the field, whitespace collapsed
	work   string  // workRemote, workHybrid, workOnsite, or "" when it says none or several
	places []place // the places it names
	city   string  // an allowed city it names, as the candidate wrote it
}

// readField reads the location field: its workplace from the ATS label or its words,
// and its places from the place tables.
func readField(location string, c CandidateGates) fieldReading {
	fr := fieldReading{text: squash(location)}
	if fr.text == "" {
		return fr
	}
	kinds := map[string]bool{}
	if m := atsLabelRE.FindStringSubmatch(fr.text); m != nil {
		kinds[strings.ToLower(m[1])] = true
	}
	if remoteWordRE.MatchString(fr.text) {
		kinds[workRemote] = true
	}
	if hybridWordRE.MatchString(fr.text) {
		kinds[workHybrid] = true
	}
	if onsiteWordRE.MatchString(fr.text) {
		kinds[workOnsite] = true
	}
	if len(kinds) == 1 {
		for k := range kinds {
			fr.work = k
		}
	}
	fr.places = findPlaces(fr.text)
	if globalRE.MatchString(fr.text) {
		fr.places = append(fr.places, place{name: "Global", region: []string{"*"}, kind: kindRegion})
	}
	fr.city = allowedCity(fr.text, c.AllowedCities)
	return fr
}

// fieldSnippet quotes the location field.
func (fr fieldReading) snippet() string { return truncate(fr.text, maxSnippet) }

// fieldLocation is the location gate's finding from a remote location field: remote from
// a country or region that holds none of the allowed countries fails, remote from a city
// outside them flags, and remote from somewhere allowed passes. A field that is not
// remote, or names no place, gives none.
func fieldLocation(fr fieldReading, c CandidateGates, limited bool) (finding, bool) {
	if fr.work != workRemote || (len(fr.places) == 0 && fr.city == "") {
		return finding{}, false
	}
	f := finding{snippet: fr.snippet(), pos: -1}
	names := placeNames(fr.places)
	switch {
	case fr.city != "":
		f.verdict = Pass
		f.reason = fmt.Sprintf("The location field says remote from %s, one of your allowed cities.", fr.city)
	case !limited && regionOnly(fr.places):
		f.verdict = Pass
		f.reason = fmt.Sprintf("The location field says remote from %s; you have set no country limit.", names)
	case !limited:
		f.verdict = Flag
		f.reason = fmt.Sprintf("The location field says remote from %s; you have set no country limit, so check.", names)
	case allowedViaRegion(fr.places, c.AllowedCountries):
		f.verdict = Pass
		f.reason = fmt.Sprintf("The location field says remote from %s, which includes your allowed countries.", names)
	case anyAllowed(fr.places, c.AllowedCountries):
		f.verdict = Pass
		f.reason = fmt.Sprintf("The location field says remote from %s, in your allowed countries.", names)
	case anyCity(fr.places):
		f.verdict = Flag
		f.reason = fmt.Sprintf("The location field says remote from %s; it may be limited to that country, your call.", names)
	default:
		f.verdict = Fail
		f.reason = fmt.Sprintf("The location field says remote, limited to %s, outside your allowed countries.", names)
	}
	return f, true
}

// fieldWorkplace is the workplace gate's finding from a hybrid or on-site location field
// for a remote-only candidate: office work outside every allowed city fails. When the
// description says fully remote, the two disagree and it only flags. Otherwise none; the
// field's words still flag through the workplace patterns as before.
func fieldWorkplace(fr fieldReading, c CandidateGates, saysRemote bool) (finding, bool) {
	if !c.RemoteOnly || fr.city != "" || (fr.work != workHybrid && fr.work != workOnsite) {
		return finding{}, false
	}
	where := placeNames(fr.places)
	if where == "" {
		where = "a place outside your allowed cities"
	}
	f := finding{
		verdict: Fail,
		reason:  fmt.Sprintf("The location field says %s in %s; you want remote only.", fr.work, where),
		snippet: fr.snippet(),
		pos:     -1,
	}
	if saysRemote {
		f.verdict = Flag
	}
	return f, true
}

func anyCity(ps []place) bool {
	for _, p := range ps {
		if p.kind == kindCity {
			return true
		}
	}
	return false
}
