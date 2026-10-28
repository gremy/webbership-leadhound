package leadstore

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func TestResearchWantFor(t *testing.T) {
	for _, c := range []struct {
		policy string
		want   []string
	}{
		{leadconfig.ChannelEmail, []string{WantEmail}},
		{leadconfig.ChannelForm, []string{WantPhone}}, // DE, CH: never email, so a business phone
		{leadconfig.ChannelNone, nil},
		{"", nil}, // a country missing from leads.yaml is off
	} {
		if got := WantFor(c.policy); !reflect.DeepEqual(got, c.want) {
			t.Errorf("WantFor(%q) = %v, want %v", c.policy, got, c.want)
		}
	}
}

func TestResearchNormWant(t *testing.T) {
	got, err := normWant([]string{WantPhone, WantEmail, WantPhone})
	if err != nil || !reflect.DeepEqual(got, []string{WantEmail, WantPhone}) {
		t.Errorf("normWant = %v %v, want [email phone]", got, err)
	}
	for _, bad := range [][]string{nil, {}, {"fax"}, {WantEmail, ""}} {
		if _, err := normWant(bad); !errors.Is(err, ErrBadResearch) {
			t.Errorf("normWant(%q) err = %v, want ErrBadResearch", bad, err)
		}
	}
}

func TestResearchTooSoonError(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	var err error = &ResearchTooSoonError{LastID: 3, StartedAt: at, NextAt: at.Add(ResearchWindow)}
	if !errors.Is(err, ErrResearchTooSoon) {
		t.Fatal("errors.Is(ResearchTooSoonError, ErrResearchTooSoon) = false")
	}
	if !strings.Contains(err.Error(), "2026-12-30") {
		t.Errorf("message %q does not name the next date 2026-12-30", err)
	}
	if ResearchWindow != 90*24*time.Hour {
		t.Errorf("ResearchWindow = %v, want 90 days", ResearchWindow)
	}
}

// TestStarredReasonOrder pins the order StarredStatus names reasons in: each case holds the
// facts of one starred lead the batch does not offer, carrying every lower-ranked reason
// too, so the first that applies must win.
func TestStarredReasonOrder(t *testing.T) {
	str := func(s string) *string { return &s }
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	rank := 900
	// base carries every reason at once.
	base := func() starredFacts {
		return starredFacts{LeadID: 1, Country: "US", Kind: LeadD10, Channel: ChannelNone, State: LeadSkipped,
			Outreach: false, Selftest: true, ProbeStale: true, Reserved: true, HasProbe: true, ListRank: &rank,
			DeferUntil: &day, Suppressed: true, Reached: true, ResearchStatus: str(ResearchQueued), ResearchAt: &day}
	}
	o := StarredStatusOptions{
		CandidateOptions: CandidateOptions{KindsOff: []string{LeadD10}, SkipRankOver: 100},
		Countries:        map[string]string{"GB": leadconfig.ChannelEmail, "DE": leadconfig.ChannelForm},
	}
	profile := leadconfig.Profile{ExcludeCountries: []string{"FR"}}
	steps := []struct {
		reason string
		next   func(*starredFacts) // takes this reason away, revealing the next
	}{
		{StarredSuppressed, func(f *starredFacts) { f.Suppressed = false }},
		{StarredNotNew, func(f *starredFacts) { f.State = LeadNew }},
		{StarredSuppressed, func(f *starredFacts) { f.Reached = false }},
		{StarredDeferred, func(f *starredFacts) { f.DeferUntil = nil; f.Country = "FR" }},
		{StarredCountryRule, func(f *starredFacts) { f.Country = "US" }}, // excluded by the profile
		{StarredCountryRule, func(f *starredFacts) { f.Country = "GB" }}, // off in leads.yaml
		{StarredOther, func(f *starredFacts) { f.Outreach = true }},
		{StarredOther, func(f *starredFacts) { f.Kind = LeadUnsupported }},
		{StarredOther, func(f *starredFacts) { f.Selftest = false }},
		{StarredOther, func(f *starredFacts) { f.ProbeStale = false }},
		{StarredOther, func(f *starredFacts) { f.Reserved = false }},
		{StarredRankedOut, func(f *starredFacts) { f.ListRank = nil; f.HasProbe = false }},
		{StarredLooking, func(f *starredFacts) { f.ResearchStatus = str(ResearchNothing) }},
		{StarredFoundNothing, func(f *starredFacts) { f.ResearchStatus = nil; f.Country = "DE" }},
		{StarredCountryRule, func(f *starredFacts) { f.Country = "GB" }}, // form-only, no form
		{StarredNoEmailYet, func(f *starredFacts) { f.Channel = ChannelEmail }},
		{StarredOther, nil}, // a predicate the list does not name
	}
	f := base()
	for i, s := range steps {
		got, detail, _ := starredReason(f, map[int64]int{}, o, profile)
		if got != s.reason {
			t.Fatalf("step %d: reason %s (%s), want %s", i, got, detail, s.reason)
		}
		if s.next != nil {
			s.next(&f)
		}
	}

	// Offered: ready, unless past the batch's limit.
	f = base()
	if got, _, _ := starredReason(f, map[int64]int{1: 0}, o, profile); got != StarredReady {
		t.Errorf("offered lead: %s, want ready", got)
	}
	o.Limit = 1
	if got, _, _ := starredReason(f, map[int64]int{1: 1}, o, profile); got != StarredRankedOut {
		t.Errorf("offered past the limit: %s, want ranked_out", got)
	}
	if got, _, _ := starredReason(f, map[int64]int{1: 0}, o, profile); got != StarredReady {
		t.Errorf("offered within the limit: %s, want ready", got)
	}

	// Without a country map the country rule is not judged: an unknown country's lead with
	// no route is no email yet.
	g := starredFacts{LeadID: 2, Country: "US", Kind: LeadD10, Channel: ChannelNone, State: LeadNew, Outreach: true}
	if got, _, _ := starredReason(g, nil, StarredStatusOptions{}, leadconfig.Profile{}); got != StarredNoEmailYet {
		t.Errorf("no country map: %s, want no_email_yet", got)
	}
}
