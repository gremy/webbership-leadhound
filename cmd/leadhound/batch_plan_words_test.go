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
