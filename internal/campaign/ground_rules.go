package campaign

import (
	"fmt"

	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// GroundRules returns the campaign-owned outreach.GroundRules fields for one lead: the pains
// its stored signals allow (PainsFor), the allowed names, research openers, inflation words
// and echo run length from the copy rules. A list the spec leaves empty stays nil, so
// outreach.Ground uses its defaults.
//
// The caller fills the rest: StepRules (the batch plan's rules for the lead's kind),
// ConfigBanned, CallStep and EchoTexts.
//
// It lives in campaign because campaign already depends on outreach (its sequence tests use
// outreach.FollowupDue); outreach must therefore never import campaign.
func (s Spec) GroundRules(sig Signals) (outreach.GroundRules, error) {
	chosen, err := s.PainsFor(sig)
	if err != nil {
		return outreach.GroundRules{}, err
	}
	openers, err := s.ResearchOpenerRegexps()
	if err != nil {
		return outreach.GroundRules{}, err
	}
	var pains []outreach.Pain
	for _, p := range chosen {
		pains = append(pains, outreach.Pain{ID: p.ID, Text: p.Text})
	}
	r := outreach.GroundRules{
		Pains:           pains,
		ResearchOpeners: openers,
		EchoWords:       s.Copy.EchoWords,
	}
	if len(s.Copy.AllowedNames) > 0 {
		r.AllowedNames = append([]string(nil), s.Copy.AllowedNames...)
	}
	if len(s.Copy.InflationWords) > 0 {
		r.InflationWords = append([]string(nil), s.Copy.InflationWords...)
	}
	if r.EchoWords < 0 {
		return outreach.GroundRules{}, fmt.Errorf("campaign %q: copy.echo_words %d", s.Slug, r.EchoWords)
	}
	return r, nil
}
