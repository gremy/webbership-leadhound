package leadview

import (
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// testFacts are end-of-life dates as the demo file states them.
var testFacts = leadconfig.Facts{
	D6EOL: leadconfig.ParseDate("2016-02-24"), D7EOL: leadconfig.ParseDate("2025-01-05"),
	D8EOL: leadconfig.ParseDate("2021-11-02"), D9EOL: leadconfig.ParseDate("2023-11-01"),
	D10EOL: leadconfig.ParseDate("2026-12-09"),
}

func TestFindingLine(t *testing.T) {
	today := time.Date(2026, 10, 27, 15, 4, 0, 0, time.UTC)
	site := func(major int, exact, ambiguous string) leadstore.FindingSubject {
		return leadstore.FindingSubject{Kind: leadstore.LeadUnsupported, Detector: "drupal@1", Major: major, ExactVersion: exact, Ambiguous: ambiguous}
	}
	cases := []struct {
		name  string
		s     leadstore.FindingSubject
		facts leadconfig.Facts
		today time.Time
		want  string
	}{
		{"major 6", site(6, "", ""), testFacts, today, "Drupal 6, out of support since 24 February 2016"},
		{"major 7", site(7, "", ""), testFacts, today, "Drupal 7, out of support since 5 January 2025"},
		{"major 8", site(8, "", ""), testFacts, today, "Drupal 8, out of support since 2 November 2021"},
		{"major 9", site(9, "", ""), testFacts, today, "Drupal 9, out of support since 1 November 2023"},
		{"major 10, future EOL", site(10, "", ""), testFacts, today, "Drupal 10, security support ends 9 December 2026"},
		{"major 11 has no date", site(11, "", ""), testFacts, today, "Drupal 11"},
		{"exact version", site(10, "10.2", ""), testFacts, today, "Drupal 10.2, security support ends 9 December 2026"},
		{"exact version without a major", site(0, "7.98", ""), testFacts, today, "Drupal 7.98, out of support since 5 January 2025"},
		{"unknown major", site(12, "", ""), testFacts, today, "Drupal 12"},
		{"version not shown", site(0, "", ""), testFacts, today, "Drupal (version not shown)"},
		{"ambiguous 10 or 11", site(0, "", drupal.Ambiguous10Or11), testFacts, today, "Drupal 10 or 11"},
		{"ambiguous 8+", site(0, "", drupal.Ambiguous8Plus), testFacts, today, "Drupal 8 or later"},
		{"ambiguous 6 or 7", site(0, "", drupal.Ambiguous6Or7), testFacts, today, "Drupal 6 or 7"},
		{"EOL day itself still ends", site(10, "", ""), testFacts, time.Date(2026, 12, 9, 23, 0, 0, 0, time.UTC), "Drupal 10, security support ends 9 December 2026"},
		{"day after EOL", site(10, "", ""), testFacts, time.Date(2026, 12, 10, 0, 1, 0, 0, time.UTC), "Drupal 10, out of support since 9 December 2026"},
		{"EOL date not set", site(10, "", ""), leadconfig.Facts{}, today, "Drupal 10"},
		{"agency", leadstore.FindingSubject{Kind: leadstore.LeadAgencyPartner, Detector: "agency@1", URL: "https://agency.example/services", Phrase: "Drupal 7 to 10 upgrades"},
			testFacts, today, "Agency page sells Drupal upgrades: “Drupal 7 to 10 upgrades”"},
		{"agency without a phrase", leadstore.FindingSubject{Kind: leadstore.LeadAgencyPartner, URL: "https://agency.example/"}, testFacts, today, "Agency page sells Drupal upgrades"},
		{"no finding", leadstore.FindingSubject{}, testFacts, today, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FindingLine(tc.s, tc.facts, tc.today); got != tc.want {
				t.Errorf("FindingLine = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWhyLines words every reason ScoreReasons emits, from a real profile and lead.
func TestWhyLines(t *testing.T) {
	under := func(n int) *int { return &n }
	p := leadconfig.Profile{
		Kinds:        map[string]int{leadstore.LeadUnsupported: 20, leadstore.LeadD10: 10, leadstore.LeadAgencyPartner: 15},
		Majors:       map[int]int{10: 30},
		Countries:    map[string]int{"GB": 10, "NL": -10},
		Channels:     map[string]int{leadstore.ChannelEmail: 5, leadstore.ChannelForm: 2},
		RankBands:    []leadconfig.RankBand{{Under: 10000, Points: 15}, {Under: 1000000, Points: 4}},
		ExactVersion: 5,
	}
	cases := []struct {
		name string
		c    leadstore.Candidate
		want []string
	}{
		{"every rule", leadstore.Candidate{Kind: leadstore.LeadD10, Major: 10, ExactVersion: "10.2", Country: "GB", Channel: leadstore.ChannelEmail, ListRank: under(9000)},
			[]string{"Lead type, Drupal 10 site: +10", "Runs Drupal 10: +30", "Exact version known, 10.2: +5", "Country you want, GB: +10", "Has an email address: +5", "Big site, list rank under 10,000: +15"}},
		{"weighed down, form, wide band", leadstore.Candidate{Kind: leadstore.LeadUnsupported, Major: 7, Country: "NL", Channel: leadstore.ChannelForm, ListRank: under(500000)},
			[]string{"Lead type, out-of-support Drupal: +20", "Country you rank lower, NL: -10", "Has a contact form: +2", "Big site, list rank under 1,000,000: +4"}},
		{"agency", leadstore.Candidate{Kind: leadstore.LeadAgencyPartner, Channel: leadstore.ChannelNone},
			[]string{"Lead type, agency that sells Drupal upgrades: +15"}},
		{"nothing scored", leadstore.Candidate{Kind: leadstore.LeadHand}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WhyLines(leadstore.ScoreReasons(p, tc.c)); !slices.Equal(got, tc.want) {
				t.Errorf("WhyLines = %q, want %q", got, tc.want)
			}
		})
	}
	if got := WhyLines([]leadstore.Reason{{Kind: "new_rule", Label: "x", Points: 3}}); !slices.Equal(got, []string{"new rule x: +3"}) {
		t.Errorf("unknown reason = %q", got)
	}
}

func TestGroupDigits(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 10000: "10,000", 100000: "100,000", 1234567: "1,234,567", -1500: "-1,500"} {
		if got := groupDigits(n); got != want {
			t.Errorf("groupDigits(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestRelevanceWords(t *testing.T) {
	n := func(v int) *int { return &v }
	cases := []struct {
		score   *int
		band    string
		trusted bool
		want    string
	}{
		{nil, "", true, "not rated yet"},
		{nil, leadstore.BandStrong, false, "not rated yet"},
		{n(92), leadstore.BandStrong, true, "92, strong match"},
		{n(72), leadstore.BandLikely, true, "72, likely match"},
		{n(72), leadstore.BandLikely, false, "72, likely match, early number"},
		{n(50), leadstore.BandMaybe, true, "50, maybe a match"},
		{n(25), leadstore.BandUnlikely, true, "25, unlikely match"},
		{n(0), leadstore.BandNo, false, "0, no match, early number"},
		{n(40), "", true, "40"},
		{n(40), "odd", false, "40, early number"},
	}
	for _, tc := range cases {
		if got := RelevanceWords(tc.score, tc.band, tc.trusted); got != tc.want {
			t.Errorf("RelevanceWords(%v, %q, %v) = %q, want %q", tc.score, tc.band, tc.trusted, got, tc.want)
		}
	}
}
