package leadstore

import (
	"fmt"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func rank(n int) *int { return &n }

// demoProfile is the suggested defaults plus a weighed and an excluded country.
var demoProfile = leadconfig.Profile{
	Kinds:        map[string]int{LeadUnsupported: 20, LeadD10: 10, LeadAgencyPartner: 15},
	Majors:       map[int]int{6: 5, 7: 30, 8: 20, 9: 15, 10: 10},
	Countries:    map[string]int{"GB": 20, "NL": -10},
	Channels:     map[string]int{ChannelEmail: 10, ChannelForm: 0},
	RankBands:    []leadconfig.RankBand{{Under: 10000, Points: 15}, {Under: 100000, Points: 8}},
	ExactVersion: 5, ExcludeCountries: []string{"DE"},
}

// scoreCases are TestScore's candidates under demoProfile; TestScoreReasonsMatchScore runs
// over them too.
var scoreCases = []struct {
	name    string
	c       Candidate
	score   int
	why     []string
	offered bool
}{
	{"every rule", Candidate{Kind: LeadUnsupported, Major: 7, ExactVersion: "7.98", Country: "GB", Channel: ChannelEmail, ListRank: rank(9999)},
		30 + 20 + 5 + 20 + 10 + 15 + 0, []string{"unsupported +20", "Drupal 7 +30", "exact version +5", "GB +20", "email +10", "rank under 10k +15"}, true},
	{"second band", Candidate{Kind: LeadD10, Major: 10, Channel: ChannelForm, ListRank: rank(10000)},
		10 + 10 + 8, []string{"d10 +10", "Drupal 10 +10", "rank under 100k +8"}, true},
	{"beyond every band", Candidate{Kind: LeadD10, Major: 10, Channel: ChannelForm, ListRank: rank(100000)},
		20, []string{"d10 +10", "Drupal 10 +10"}, true},
	{"unranked", Candidate{Kind: LeadUnsupported, Major: 6, Channel: ChannelEmail},
		35, []string{"unsupported +20", "Drupal 6 +5", "email +10"}, true},
	{"negative country", Candidate{Kind: LeadAgencyPartner, Country: "NL", Channel: ChannelEmail},
		15, []string{"agency partner +15", "NL -10", "email +10"}, true},
	{"major not weighed", Candidate{Kind: LeadD10, Major: 11, Channel: ChannelForm},
		10, []string{"d10 +10"}, true},
	{"unlisted country", Candidate{Kind: LeadD10, Country: "FR", Channel: ChannelForm}, 10, []string{"d10 +10"}, true},
	{"excluded", Candidate{Kind: LeadUnsupported, Major: 7, Country: "DE", Channel: ChannelEmail}, 60, nil, false},
	{"agency lead has no major", Candidate{Kind: LeadAgencyPartner, Major: 0, Channel: ChannelForm, ListRank: rank(1)},
		30, []string{"agency partner +15", "rank under 10k +15"}, true},
}

func TestScore(t *testing.T) {
	for _, tc := range scoreCases {
		t.Run(tc.name, func(t *testing.T) {
			score, why := Score(demoProfile, tc.c)
			if score != tc.score {
				t.Errorf("score = %d, want %d", score, tc.score)
			}
			if tc.why != nil && !slices.Equal(why, tc.why) {
				t.Errorf("why = %q, want %q", why, tc.why)
			}
			if got := Offered(demoProfile, tc.c); got != tc.offered {
				t.Errorf("offered = %v, want %v", got, tc.offered)
			}
		})
	}
}

// TestScoreTies: two leads with the same facts score the same, and an empty profile scores
// every lead 0 with no reasons.
func TestScoreTies(t *testing.T) {
	a := Candidate{LeadID: 1, Kind: LeadUnsupported, Major: 7, Channel: ChannelEmail, ListRank: rank(5)}
	b := a
	b.LeadID, b.ListRank = 2, rank(50)
	sa, _ := Score(demoProfile, a)
	sb, _ := Score(demoProfile, b)
	if sa != sb {
		t.Errorf("same band, same facts: %d vs %d", sa, sb)
	}
	if s, why := Score(leadconfig.Profile{}, a); s != 0 || len(why) != 0 {
		t.Errorf("empty profile: %d %q", s, why)
	}
}

func TestRankLabel(t *testing.T) {
	for n, want := range map[int]string{500: "500", 1000: "1k", 10000: "10k", 1500: "1500", 1000000: "1M", 2500000: "2500k"} {
		if got := rankLabel(n); got != want {
			t.Errorf("rankLabel(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestScoreReasonsMatchScore: over every TestScore candidate (and an empty profile), Score is the sum of
// ScoreReasons' points and its reasons are theirs, one for one, so the two cannot drift.
func TestScoreReasonsMatchScore(t *testing.T) {
	for _, p := range []leadconfig.Profile{demoProfile, {}} {
		for _, tc := range scoreCases {
			c := tc.c
			score, why := Score(p, c)
			reasons := ScoreReasons(p, c)
			sum := 0
			for _, r := range reasons {
				sum += r.Points
				if r.Points == 0 {
					t.Errorf("%+v: a reason with 0 points: %+v", c, r)
				}
			}
			if sum != score {
				t.Errorf("%+v: reasons sum to %d, Score %d", c, sum, score)
			}
			if len(why) != len(reasons) {
				t.Fatalf("%+v: %d reasons, Score gave %q", c, len(reasons), why)
			}
			for i, r := range reasons {
				if want := r.shortLabel() + " " + fmt.Sprintf("%+d", r.Points); why[i] != want {
					t.Errorf("%+v: reason %d is %q, Score says %q", c, i, want, why[i])
				}
			}
		}
	}
	got := ScoreReasons(demoProfile, scoreCases[0].c)
	want := []Reason{
		{ReasonKind, LeadUnsupported, 20}, {ReasonMajor, "7", 30}, {ReasonExactVersion, "7.98", 5},
		{ReasonCountry, "GB", 20}, {ReasonChannel, ChannelEmail, 10}, {ReasonRankBand, "10000", 15},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ScoreReasons = %+v, want %+v", got, want)
	}
}
