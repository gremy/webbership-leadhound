package gates

import (
	"fmt"
	"regexp"
	"strings"
)

// fullyRemoteRE says the job is remote. It outweighs "hybrid" ("Fully remote, or hybrid
// if you prefer"), but not office days or on-site terms said alongside it: "some teams
// are fully remote, this role is 3 days a week in the office" is not a remote job.
var fullyRemoteRE = regexp.MustCompile(`(?i)\b(?:fully\s+remote|100\s*%\s*remote|remote[- ]first)\b`)

// hybridRE captures the word after "hybrid", so "hybrid cloud" can be told from a
// hybrid work arrangement.
var hybridRE = regexp.MustCompile(`(?i)\bhybrid\b(?:[\s-]+([\pL]+))?`)

// hybridTech are the words that make "hybrid" a technical term, not a workplace.
var hybridTech = map[string]bool{
	"cloud": true, "clouds": true, "app": true, "apps": true, "application": true, "applications": true,
	"mobile": true, "infrastructure": true, "integration": true,
	"integrations": true, "architecture": true, "architectures": true, "models": true,
	"solution": true, "solutions": true, "search": true, "systems": true, "system": true, "storage": true,
	"multi": true, "multicloud": true, "network": true, "networks": true, "deployment": true, "deployments": true,
	"rendering": true, "it": true, "ai": true, "vehicle": true, "vehicles": true, "data": true,
}

// officeDaysRE matches office days: "2 days a week in the office", "two days per week at
// the office", "2 days/week at the office", "3 days in the office", "3 zile pe săptămână
// la birou", "in the office 2 days a week", "two days a week in our Lisbon office",
// "from our Lisbon office three days a week", and "our Lisbon office, three days a week".
// Each form captures the office's city when the phrase names one.
var officeDaysRE = regexp.MustCompile(`(?i)` +
	`\b` + officeNum + `\s*(?:days?|zile)\b(?:\s*(?:(?:a|per|pe|each|every)\s+|/\s*)(?:week|săptămân\pL*))?\s*(?:in|at|la|în|from)\s+(?:the\s+|our\s+)?` + officeCityOpt + `(?:office|birou)\b` +
	`|\b(?:in|at|from)\s+(?:the|our)\s+` + officeCityOpt + `office\s+` + officeNum + `\s+days?\b` +
	`|\b(?:the|our)\s+` + officeCityOpt + `office\b[^.\n]{0,40}?\b` + officeNum + `\s+days?\s+(?:a|per|each|every)\s+week\b`)

const (
	officeNum = `(?:[1-5]|one|two|three|four|five)`
	// officeCityOpt is an optional capitalised word before "office": the office's city.
	officeCityOpt = `(?:(?-i:(\p{Lu}[\pL-]+))\s+)?`
)

var (
	onsiteRE   = regexp.MustCompile(`(?i)\bon[- ]?site\b`)
	inOfficeRE = regexp.MustCompile(`(?i)\bin[- ]office\b`)
	// officeCityRE names an office's city: "office in Berlin", "our Paris office",
	// "in Exampleville office", "at the Paris office".
	officeCityRE = regexp.MustCompile(`\b(?i:office\s+in)\s+(\p{Lu}[\pL-]+)|\b(?i:in|at|our)\s+(?:(?i:our|the)\s+)?(\p{Lu}[\pL-]+)\s+(?i:office)\b`)
)

func evalWorkplace(obs Observation, c CandidateGates) Result {
	// The title and the location line can carry "(Hybrid)"; scan them with the body.
	text := strings.Join(nonEmpty(obs.Title, obs.Location, obs.Description), "\n")

	remote := -1 // where the first un-negated "fully remote" sits
	for _, m := range fullyRemoteRE.FindAllStringIndex(text, -1) {
		if !negated(text, m[0]) {
			remote = m[0]
			break
		}
	}

	// A plain remote word ("Remote, EU", "a remote role", "work remotely") says remote
	// without saying fully remote or from where.
	remoteWord := false
	for _, m := range remoteWordRE.FindAllStringIndex(text, -1) {
		if !negated(text, m[0]) && !negatedAfter(text, m[1]) {
			remoteWord = true
			break
		}
	}

	var fs []finding
	for _, m := range officeDaysRE.FindAllStringSubmatchIndex(text, -1) {
		f := finding{verdict: Flag, reason: "Asks for days in the office.", snippet: snippet(text, m[0], m[1]), pos: m[0]}
		if c.RemoteOnly {
			// Office days in a city the candidate allows stay their call. The city the
			// phrase itself names comes first, then one named elsewhere, then the location line.
			city := submatchCity(text, m)
			if city == "" {
				city = officeCity(text, obs.Location)
			}
			if city != "" && allowedCity(city, c.AllowedCities) == "" {
				f.verdict = Fail
				f.reason = fmt.Sprintf("Asks for days in the office in %s; you want remote only.", city)
			}
		}
		fs = append(fs, f)
	}
	if remote < 0 {
		for _, m := range hybridRE.FindAllStringSubmatchIndex(text, -1) {
			if m[2] >= 0 && hybridTech[strings.ToLower(text[m[2]:m[3]])] {
				continue
			}
			fs = append(fs, finding{verdict: Flag, reason: "Posting says the job is hybrid.", snippet: snippet(text, m[0], m[1]), pos: m[0]})
		}
	}
	for _, m := range onsiteRE.FindAllStringIndex(text, -1) {
		fs = append(fs, finding{verdict: Flag, reason: "Posting says the job is on-site.", snippet: snippet(text, m[0], m[1]), pos: m[0]})
	}
	for _, m := range inOfficeRE.FindAllStringIndex(text, -1) {
		fs = append(fs, finding{verdict: Flag, reason: "Posting says the job is in-office.", snippet: snippet(text, m[0], m[1]), pos: m[0]})
	}
	if f, ok := fieldWorkplace(readField(obs.Location, c), c, fieldSignals{saysRemote: remote >= 0, remoteWord: remoteWord, listedRemote: obs.ListedRemote}); ok {
		fs = append(fs, f)
	}
	f, found := worst(fs)
	switch {
	case found && remote >= 0:
		f.reason += " It also says fully remote; check which applies."
		return result(GateWorkplace, f)
	case found:
		return result(GateWorkplace, f)
	case remote >= 0:
		end := remote + len(fullyRemoteRE.FindString(text[remote:]))
		return Result{Gate: GateWorkplace, Verdict: Pass, Reason: "Posting says the job is fully remote.", Snippet: snippet(text, remote, end)}
	}
	return Result{Gate: GateWorkplace, Verdict: Pass, Reason: "No office or hybrid terms found.", Snippet: opening(text)}
}

// officeCity names the office's city from the text, else the posting's location line.
func officeCity(text, location string) string {
	for _, m := range officeCityRE.FindAllStringSubmatchIndex(text, -1) {
		if city := submatchCity(text, m); city != "" {
			return city
		}
	}
	city, _, _ := strings.Cut(strings.TrimSpace(location), ",")
	return strings.TrimSpace(city)
}

// notOfficeCity are capitalised words that can stand before "office" without naming a
// city ("the Main office", "Our Head office").
var notOfficeCity = map[string]bool{
	"the": true, "our": true, "main": true, "head": true, "central": true, "local": true,
	"regional": true, "nearest": true, "home": true, "company": true, "global": true,
	"european": true, "headquarters": true, "corporate": true,
}

// submatchCity returns the first city group a match captured, skipping a capitalised
// word that is not a city ("the Main office").
func submatchCity(text string, m []int) string {
	for i := 2; i+1 < len(m); i += 2 {
		if m[i] < 0 {
			continue
		}
		city := text[m[i]:m[i+1]]
		if notOfficeCity[strings.ToLower(city)] {
			continue
		}
		return city
	}
	return ""
}

func nonEmpty(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
