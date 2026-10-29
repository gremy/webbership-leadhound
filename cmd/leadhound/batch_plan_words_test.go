package main

import (
	"maps"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// A lead moved onto call_first takes that sequence's own word limits; the default sequence
// keeps copy.max_words, and a spec without per-sequence limits gives call_first the same.
func TestForLeadWordLimitsBySequence(t *testing.T) {
	callFirstTable(t)
	spec := campaign.Spec{Copy: campaign.Copy{
		MaxWords:           map[int]int{0: 120, 1: 80},
		MaxWordsBySequence: map[string]map[int]int{campaign.SequenceCallFirst: {2: 100, 3: 50}},
	}}
	p := batchPlan{spec: spec, callFirst: campaign.DefaultCallFirst(), rules: outreach.StepRules{MaxWords: spec.Copy.MaxWords}}
	if q, name := p.forLead("RO"); name != campaign.SequenceCallFirst || !maps.Equal(q.rules.MaxWords, map[int]int{2: 100, 3: 50}) {
		t.Errorf("RO lead: %q %v", name, q.rules.MaxWords)
	}
	if q, _ := p.forLead("GB"); !maps.Equal(q.rules.MaxWords, spec.Copy.MaxWords) {
		t.Errorf("GB lead: %v", q.rules.MaxWords)
	}
	p.spec.Copy.MaxWordsBySequence = nil
	if q, _ := p.forLead("RO"); !maps.Equal(q.rules.MaxWords, spec.Copy.MaxWords) {
		t.Errorf("RO lead without per-sequence limits: %v", q.rules.MaxWords)
	}
}

// The operator, 29 Oct 2026: every country is email first, so on the shipped table a lead
// from a country that used to be call-first stays on the default sequence.
func TestForLeadEveryCountryEmailFirst(t *testing.T) {
	p := batchPlan{callFirst: campaign.DefaultCallFirst()}
	for _, code := range []string{"RO", "IT", "ES", "DE", "DK", "NL", "LV", "AT", "PL", "CZ", "BG", "GB"} {
		if _, name := p.forLead(code); name != "" {
			t.Errorf("%s lead on sequence %q, want the default", code, name)
		}
	}
	if got := p.phoneOnlyCountries(); len(got) != 0 {
		t.Errorf("phone-only countries %v, want none", got)
	}
}
