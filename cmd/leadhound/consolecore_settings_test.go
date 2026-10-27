package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestTrustGateWords: the trust gate says in plain words whether the list sorts by relevance
// yet and what the latest check read; the rule is one plain sentence, and its exact
// thresholds (the band, the rank correlation) are only in the fold's words. None of it
// repeats the labelled count or sends him to To label, which the page says once itself.
func TestTrustGateWords(t *testing.T) {
	loc := time.UTC
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, loc)
	const rule = "Sorting by relevance starts once Jev agrees with your calls on at least 30 leads: close to your call on 8 in 10 of them, and ranking them in roughly the same order."
	const exact = "In exact terms: a check on at least 30 labelled leads, where Jev's band is your call or the one next to it on 80% or more of them, and the two orders agree with a rank correlation (Spearman) of 0.5 or more, where 1 is the same order."
	cases := []struct {
		name   string
		e      leadstore.RelevanceEval
		ok     bool
		status []string
	}{
		{"never checked", leadstore.RelevanceEval{}, false, []string{
			"For now the list and the queue sort leads that are not starred by points: Jev's numbers have not been checked against your calls yet."}},
		{"checked, not passed", leadstore.RelevanceEval{At: at, N: 31, WithinOne: 0.74, Spearman: 0.42}, true, []string{
			"For now the list and the queue sort leads that are not starred by points: Jev has not passed its check yet.",
			"The latest check, on 2 November 2026, read 31 of your calls: Jev was close to your call on 74% of them, and its order matched yours at 0.42."}},
		{"passed", leadstore.RelevanceEval{At: at, N: 40, WithinOne: 0.85, Spearman: 0.61, Trusted: true}, true, []string{
			"The list and the queue sort leads that are not starred by relevance.",
			"The latest check, on 2 November 2026, read 40 of your calls: Jev was close to your call on 85% of them, and its order matched yours at 0.61."}},
	}
	for _, c := range cases {
		g := trustGateWords(c.e, c.ok, loc)
		if !slices.Equal(g.Status, c.status) {
			t.Errorf("%s: status %q, want %q", c.name, g.Status, c.status)
		}
		if g.Rule != rule || g.Exact != exact {
			t.Errorf("%s: rule %q, exact %q", c.name, g.Rule, g.Exact)
		}
		plain := strings.Join(append(slices.Clone(g.Status), g.Rule), " ")
		for _, bad := range []string{"Spearman", "within one band", "rank correlation", "answer key", "To label", "labelled:"} {
			if strings.Contains(plain, bad) {
				t.Errorf("%s: the plain words hold %q: %s", c.name, bad, plain)
			}
		}
	}
}

// TestOptOutLanguages: the opt-out lines are named by language, in the order of their codes.
func TestOptOutLanguages(t *testing.T) {
	got := optOutLanguages(map[string]string{"en": "x", "de": "y"})
	if want := []string{"German", "English"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := optOutLanguages(nil); len(got) != 0 {
		t.Errorf("none: %q", got)
	}
}
