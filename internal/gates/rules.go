package gates

import (
	"fmt"
	"regexp"
	"sync"
)

// ruleCache holds compiled rule patterns by pattern text; rules are few and stable.
var ruleCache sync.Map // string → *regexp.Regexp or error

func compileRule(pattern string) (*regexp.Regexp, error) {
	if v, ok := ruleCache.Load(pattern); ok {
		if re, ok := v.(*regexp.Regexp); ok {
			return re, nil
		}
		return nil, v.(error)
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		ruleCache.Store(pattern, err)
		return nil, err
	}
	ruleCache.Store(pattern, re)
	return re, nil
}

func ruleField(obs Observation, field string) (string, bool) {
	switch field {
	case "company":
		return obs.Company, true
	case "title":
		return obs.Title, true
	case "description":
		return obs.Description, true
	case "location":
		return obs.Location, true
	}
	return "", false
}

// evalRules gives one result per matching rule, in the order given: a skip rule fails,
// a flag rule flags. A rule that cannot be applied (bad pattern, unknown field or kind)
// is flagged rather than dropped, so it is seen.
func evalRules(obs Observation, rules []Rule) []Result {
	var out []Result
	for _, r := range rules {
		id := r.ID
		broken := func(why string) {
			out = append(out, Result{Gate: GateRule, Verdict: Flag, Reason: fmt.Sprintf("Rule %d %s, so it was not applied.", id, why), RuleID: &id})
		}
		text, ok := ruleField(obs, r.Field)
		if !ok {
			broken(fmt.Sprintf("names an unknown field %q", r.Field))
			continue
		}
		verdict := Fail
		switch r.Kind {
		case KindSkip:
		case KindFlag:
			verdict = Flag
		default:
			broken(fmt.Sprintf("has an unknown kind %q", r.Kind))
			continue
		}
		re, err := compileRule(r.Pattern)
		if err != nil {
			broken("has a pattern that does not compile")
			continue
		}
		m := re.FindStringIndex(text)
		if m == nil {
			continue
		}
		reason := r.Reason
		if reason == "" {
			reason = fmt.Sprintf("Matches %s rule %d on the %s.", r.Kind, id, r.Field)
		} else {
			reason = fmt.Sprintf("%s (rule %d on the %s)", reason, id, r.Field)
		}
		out = append(out, Result{Gate: GateRule, Verdict: verdict, Reason: reason, Snippet: snippet(text, m[0], m[1]), RuleID: &id})
	}
	return out
}
