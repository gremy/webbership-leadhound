package outreach

import (
	"slices"
	"strings"
	"testing"
)

// The no-link rule holds the lead's first email to it: step 0 on the default sequence, the
// lowest email step (2) on call_first, whose emails follow the calls; a later email may
// carry the link.
func TestNoLinkInFirstEmailOfSequence(t *testing.T) {
	d, cfg := checkDraft(t)
	if cfg.Sender.SiteURL == "" {
		t.Fatal("demo config has no site_url")
	}
	linked := checkInsert(d, "\nThe demo: "+cfg.Sender.SiteURL+"\n")
	firstEmailLink := func(p []Problem) bool {
		return slices.ContainsFunc(p, func(x Problem) bool { return x.Rule == RuleURL && strings.Contains(x.Detail, "first email") })
	}
	wide := map[int]int{0: 500, 1: 500, 2: 500, 3: 500, 4: 500}
	for _, c := range []struct {
		name  string
		steps []int
		step  int
		want  bool
	}{
		{"default first email", []int{0, 1, 3, 4}, 0, true},
		{"default follow-up", []int{0, 1, 3, 4}, 1, false},
		{"call_first first email", []int{2, 3}, 2, true},
		{"call_first follow-up", []int{2, 3}, 3, false},
		{"legacy steps", nil, 0, true},
	} {
		rules := StepRules{EmailSteps: c.steps, NoLinksInStep0: true}
		if c.steps != nil {
			rules.MaxWords = wide
		}
		if got := firstEmailLink(CheckIn(linked, c.step, cfg, rules)); got != c.want {
			t.Errorf("%s (step %d): first-email link refused = %v, want %v", c.name, c.step, got, c.want)
		}
	}
	if got := (StepRules{EmailSteps: []int{3, 2}}).FirstEmail(); got != 2 {
		t.Errorf("FirstEmail = %d, want 2", got)
	}
}
