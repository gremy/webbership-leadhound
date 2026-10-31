package outreach

import (
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func TestParseOverrides(t *testing.T) {
	ov, err := ParseOverrides([]string{"ramp", "window", "ramp"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ov.Codes(), []string{"ramp", "window"}; !slices.Equal(got, want) {
		t.Errorf("Codes = %v, want %v", got, want)
	}
	if !ov.Has(OverrideRamp) || ov.Has(OverrideDomainDaily) {
		t.Errorf("Has: %v", ov)
	}
	for _, bad := range []string{"", "mailbox_ceiling", "domain_mailbox", "RAMP", "spacing"} {
		_, err := ParseOverrides([]string{"ramp", bad})
		if err == nil || !strings.Contains(err.Error(), `"`+bad+`"`) {
			t.Errorf("ParseOverrides(%q) err = %v, want it named", bad, err)
		}
	}
	all := []string{}
	for _, c := range AllOverrideCodes() {
		all = append(all, string(c))
	}
	if !slices.IsSorted(all) || len(all) != 6 {
		t.Errorf("AllOverrideCodes = %v", all)
	}
	ov, err = ParseOverrides(all)
	if err != nil || !slices.Equal(ov.Codes(), all) {
		t.Errorf("ParseOverrides(all) = %v, %v", ov.Codes(), err)
	}
	var none Overrides
	if none.Has(OverrideRamp) || len(none.Codes()) != 0 {
		t.Error("nil Overrides is not empty")
	}
	if MinSpacing != time.Minute {
		t.Errorf("MinSpacing = %v", MinSpacing)
	}
}

func TestRoomTodayWith(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	day := time.Date(2026, 12, 2, 10, 0, 0, 0, london)
	long := day.AddDate(0, -2, 0) // ramp 60, clamped to 40
	week := day.AddDate(0, 0, -8) // ramp 12
	cfg := mailboxFirstSendConfig(t, 50, mailboxRamp()...)
	mb := func(id int64, domain string, first *time.Time, sent int, ready bool) MailboxLoad {
		return MailboxLoad{ID: id, Domain: domain, FirstSend: first, SentToday: sent, Ready: ready}
	}
	ramp := Overrides{OverrideRamp: true}
	dom := Overrides{OverrideDomainDaily: true}
	both := Overrides{OverrideRamp: true, OverrideDomainDaily: true}
	other := Overrides{OverrideWindow: true, OverrideSchedule: true, OverrideSpacingJitter: true, OverrideBusinessHours: true}
	tests := []struct {
		name string
		mbs  []MailboxLoad
		ov   Overrides
		want map[int64]int
	}{
		{"ramp lifts to the ceiling", []MailboxLoad{mb(1, "a.example", &week, 3, true)}, ramp, map[int64]int{1: 37}},
		{"cold start ramp", []MailboxLoad{mb(1, "a.example", nil, 0, true)}, ramp, map[int64]int{1: 40}},
		{"other codes change nothing", []MailboxLoad{mb(1, "a.example", &week, 3, true)}, other, map[int64]int{1: 9}},
		{"domain_daily lifts the domain only", []MailboxLoad{mb(1, "a.example", &long, 0, true), mb(2, "a.example", &long, 0, true)}, dom, map[int64]int{1: 40, 2: 40}},
		{"both stop at the domain ceiling", []MailboxLoad{mb(1, "a.example", &long, 0, true), mb(2, "a.example", &long, 0, true), mb(3, "a.example", &long, 0, true)}, both, map[int64]int{1: 40, 2: 40, 3: 20}},
		{"ramp keeps the domain cap; base room is kept", []MailboxLoad{mb(1, "a.example", &week, 0, true), mb(2, "a.example", &week, 0, true)}, ramp, map[int64]int{1: 38, 2: 12}},
		{"not ready stays 0", []MailboxLoad{mb(1, "a.example", &long, 10, false), mb(2, "a.example", &week, 0, true)}, both, map[int64]int{1: 0, 2: 40}},
		{"sent past the ceiling is no room", []MailboxLoad{mb(1, "a.example", &long, 45, true)}, both, map[int64]int{1: 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RoomTodayWith(cfg, tc.mbs, day, tc.ov); !maps.Equal(got, tc.want) {
				t.Errorf("RoomTodayWith = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckLimits(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	day := time.Date(2026, 12, 2, 10, 0, 0, 0, london)
	long := day.AddDate(0, -2, 0) // ramp clamped to 40
	week := day.AddDate(0, 0, -8) // ramp 12
	cfg := mailboxFirstSendConfig(t, 50, mailboxRamp()...)
	mb := func(id int64, domain string, first *time.Time, sent int, ready bool) MailboxLoad {
		return MailboxLoad{ID: id, Domain: domain, FirstSend: first, SentToday: sent, Ready: ready}
	}
	tests := []struct {
		name  string
		mbs   []MailboxLoad
		need  map[int64]int
		warns []LimitWarning
		stops []LimitStop
	}{
		{"within room is clean", []MailboxLoad{mb(1, "a.example", &week, 2, true)}, map[int64]int{1: 10}, nil, nil},
		{"cold start over the ramp", []MailboxLoad{mb(1, "a.example", nil, 0, true)}, map[int64]int{1: 8},
			[]LimitWarning{{Code: OverrideRamp, MailboxID: 1, Domain: "a.example", Used: 0, Allowed: 5, Over: 3}}, nil},
		{"steady state over the ramp", []MailboxLoad{mb(1, "a.example", &week, 10, true)}, map[int64]int{1: 5},
			[]LimitWarning{{Code: OverrideRamp, MailboxID: 1, Domain: "a.example", Used: 10, Allowed: 12, Over: 3}}, nil},
		{"shared domain over its cap", []MailboxLoad{mb(1, "a.example", &long, 20, true), mb(2, "a.example", &long, 20, true)}, map[int64]int{1: 5, 2: 10},
			[]LimitWarning{{Code: OverrideDomainDaily, MailboxID: 2, Domain: "a.example", Used: 40, Allowed: 50, Over: 10}}, nil},
		{"lifted ramp uses the domain's remainder", []MailboxLoad{mb(1, "a.example", &week, 0, true), mb(2, "a.example", &week, 0, true)}, map[int64]int{1: 30, 2: 30},
			[]LimitWarning{
				{Code: OverrideRamp, MailboxID: 1, Domain: "a.example", Used: 0, Allowed: 12, Over: 18},
				{Code: OverrideDomainDaily, MailboxID: 2, Domain: "a.example", Used: 0, Allowed: 50, Over: 18},
				{Code: OverrideRamp, MailboxID: 2, Domain: "a.example", Used: 0, Allowed: 12, Over: 18},
			}, nil},
		{"not ready is a stop", []MailboxLoad{mb(1, "a.example", &long, 0, false)}, map[int64]int{1: 1}, nil,
			[]LimitStop{{MailboxID: 1, Domain: "a.example", Reason: "not_ready"}}},
		{"unknown mailbox is a stop", nil, map[int64]int{4: 1}, nil, []LimitStop{{MailboxID: 4, Reason: "not_ready"}}},
		{"past the mailbox ceiling", []MailboxLoad{mb(1, "a.example", &long, 30, true)}, map[int64]int{1: 11}, nil,
			[]LimitStop{{MailboxID: 1, Domain: "a.example", Reason: "mailbox_ceiling", Used: 30, Allowed: 40}}},
		{"past the domain ceiling", []MailboxLoad{mb(1, "a.example", &long, 40, true), mb(2, "a.example", &long, 40, true), mb(3, "a.example", &long, 0, true)}, map[int64]int{3: 21}, nil,
			[]LimitStop{{MailboxID: 3, Domain: "a.example", Reason: "domain_ceiling", Used: 80, Allowed: 100}}},
		{"zero need is ignored", []MailboxLoad{mb(1, "a.example", &long, 0, false)}, map[int64]int{1: 0}, nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warns, stops := CheckLimits(cfg, tc.mbs, day, tc.need)
			if !reflect.DeepEqual(warns, tc.warns) {
				t.Errorf("warns = %+v, want %+v", warns, tc.warns)
			}
			if !reflect.DeepEqual(stops, tc.stops) {
				t.Errorf("stops = %+v, want %+v", stops, tc.stops)
			}
		})
	}
}

// TestCheckLimitsInvariant sweeps random states (cold start, steady state, shared domains,
// not ready) and checks the contract both the preview and the run rely on: the overrides
// warned about are enough on every mailbox without a stop, a mailbox is named exactly when
// RoomToday is short of its need, and no override ever passes a hard ceiling.
func TestCheckLimitsInvariant(t *testing.T) {
	london := scheduleZone(t, "Europe/London")
	day := time.Date(2026, 12, 2, 10, 0, 0, 0, london)
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: deterministic test RNG, so each random case repeats
	firsts := []*time.Time{nil, ptr(day), ptr(day.AddDate(0, 0, -8)), ptr(day.AddDate(0, 0, -15)), ptr(day.AddDate(0, -2, 0))}
	domains := []string{"a.example", "b.example", "A.Example"}
	for i := range 5000 {
		cfg := mailboxFirstSendConfig(t, []int{0, 10, 50, 100, 300}[r.IntN(5)], mailboxRamp()...)
		var mbs []MailboxLoad
		need := map[int64]int{}
		for id := range int64(1 + r.IntN(5)) {
			mbs = append(mbs, MailboxLoad{
				ID: id + 1, Domain: domains[r.IntN(len(domains))], FirstSend: firsts[r.IntN(len(firsts))],
				SentToday: r.IntN(45), Ready: r.IntN(5) > 0,
			})
			need[id+1] = r.IntN(50)
		}
		warns, stops := CheckLimits(cfg, mbs, day, need)
		named := map[int64]bool{}
		stopped := map[int64]bool{}
		var codes []string
		for _, w := range warns {
			named[w.MailboxID] = true
			codes = append(codes, string(w.Code))
			if w.Over <= 0 {
				t.Fatalf("state %d: warning with Over %d: %+v", i, w.Over, w)
			}
		}
		for _, s := range stops {
			named[s.MailboxID], stopped[s.MailboxID] = true, true
		}
		ov, err := ParseOverrides(codes)
		if err != nil {
			t.Fatal(err)
		}
		room := RoomTodayWith(cfg, mbs, day, ov)
		base := RoomToday(cfg, mbs, day)
		full := RoomTodayWith(cfg, mbs, day, Overrides{OverrideRamp: true, OverrideDomainDaily: true})
		domainUse := map[string]int{}
		for _, m := range mbs {
			id := m.ID
			if !stopped[id] && room[id] < need[id] {
				t.Fatalf("state %d mailbox %d: room %d with %v < need %d\nmbs %+v\nneed %v\nwarns %+v", i, id, room[id], ov.Codes(), need[id], mbs, need, warns)
			}
			if short := base[id] < need[id]; short != named[id] {
				t.Fatalf("state %d mailbox %d: RoomToday %d need %d, named %v\nmbs %+v\nwarns %+v stops %+v", i, id, base[id], need[id], named[id], mbs, warns, stops)
			}
			if room[id] < base[id] {
				t.Fatalf("state %d mailbox %d: override took room (%d < %d)", i, id, room[id], base[id])
			}
			if !m.Ready && full[id] != 0 {
				t.Fatalf("state %d mailbox %d: not ready has room %d", i, id, full[id])
			}
			if m.SentToday+full[id] > leadconfig.HardDailyCeiling && full[id] > 0 {
				t.Fatalf("state %d mailbox %d: past the mailbox ceiling", i, id)
			}
			domainUse[mailboxDomain(m.Domain)] += m.SentToday + full[id]
		}
		for d, used := range domainUse {
			sent := 0
			for _, m := range mbs {
				if mailboxDomain(m.Domain) == d {
					sent += m.SentToday
				}
			}
			if used > max(sent, leadconfig.HardDomainDailyCeiling) {
				t.Fatalf("state %d domain %s: %d past the domain ceiling", i, d, used)
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }
