package main

import (
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/phones"
)

// TestCallableVerdictRules: the one call rule refuses a call for each rule that fails, says
// which, and keeps a lasting refusal apart from one the hours or a screening fix; a
// campaign's own calls-off is a warning on a callable call, never a refusal.
func TestCallableVerdictRules(t *testing.T) {
	tbl := compliance.Default()
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC) // Tue, 10:00 in London and Dublin
	approved := now.Add(-2 * time.Hour)
	gb := "+441632960002"
	clean := func(n string, regs ...string) []phones.Screening {
		var out []phones.Screening
		for _, r := range regs {
			out = append(out, phones.Screening{Number: n, Register: r, Result: phones.Clean, CheckedAt: now.Add(-time.Hour)})
		}
		return out
	}
	on := specOverrides(map[string]campaign.Country{"GB": {Email: true, Call: true}, "IE": {Email: true}})
	cases := []struct {
		name     string
		f        callFacts
		at       time.Time
		callable bool
		rule     string
		lasting  bool
		warn     bool
	}{
		{"GB screened, in hours", callFacts{Country: "GB", Number: gb, Screenings: clean(gb, "tps", "ctps"), Overrides: on}, now, true, "", false, false},
		{"no number", callFacts{Country: "GB", Overrides: on}, now, false, ruleNoNumber, true, false},
		{"suppressed", callFacts{Country: "GB", Number: gb, Suppressed: true, Screenings: clean(gb, "tps", "ctps"), Overrides: on}, now, false, ruleSuppressed, true, false},
		{"bad number", callFacts{Country: "GB", Number: gb, Bad: true, Screenings: clean(gb, "tps", "ctps"), Overrides: on}, now, false, ruleBadNumber, true, false},
		{"law off (PL), no override", callFacts{Country: "PL", Number: "+48221234567", Overrides: specOverrides(map[string]campaign.Country{"PL": {Call: true}})}, now, false, compliance.RuleOff, true, false},
		{"GB unscreened", callFacts{Country: "GB", Number: gb, Overrides: on}, now, false, ruleScreening, false, false},
		{"GB outside hours", callFacts{Country: "GB", Number: gb, Screenings: clean(gb, "tps", "ctps"), Overrides: on}, now.Add(11 * time.Hour), false, compliance.RuleWindow, false, false},
		{"US region unknown", callFacts{Country: "US", Number: "+12125550100", Overrides: specOverrides(map[string]campaign.Country{"US": {Call: true}})}, now, false, compliance.RuleRegionUnknown, true, false},
		{"IE mobile", callFacts{Country: "IE", Number: "+353871234567", Screenings: clean("+353871234567", "ndd"), CampaignStart: approved, Overrides: on}, now, false, compliance.RuleMobile, true, false},
		{"IE landline, campaign calls off: warn", callFacts{Country: "IE", Number: "+353215550100", Screenings: clean("+353215550100", "ndd"), CampaignStart: approved, Overrides: on}, now, true, "", false, true},
		{"legacy campaign, no rules: no warning", callFacts{Country: "GB", Number: gb, Screenings: clean(gb, "tps", "ctps")}, now, true, "", false, false},
	}
	for _, c := range cases {
		v := callVerdictFor(tbl, c.f, c.at)
		if v.Callable != c.callable || v.Rule != c.rule || v.lasting() != c.lasting || (v.CampaignOff != "") != c.warn {
			t.Errorf("%s: callable %v rule %q lasting %v warn %q; want %v %q %v %v (reason %q)",
				c.name, v.Callable, v.Rule, v.lasting(), v.CampaignOff, c.callable, c.rule, c.lasting, c.warn, v.Reason)
		}
		if !v.Callable && (v.Reason == "" || v.Reason[len(v.Reason)-1] == '.') {
			t.Errorf("%s: reason %q must be words with no closing full stop (the card adds one)", c.name, v.Reason)
		}
	}
}
