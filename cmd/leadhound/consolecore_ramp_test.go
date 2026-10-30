package main

import (
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// TestMailboxRampAgreesWithCapOn: the mailbox card's day of the ramp and today's limit come
// from the same day 0 as outreach.CapOn, so the card never says the ramp has not started while
// the send path already allows sends, nor counts from a different day. CapOn's day 0 is
// caps.start_date when fixed, else the first live send; the card shows day 1 on day 0.
func TestMailboxRampAgreesWithCapOn(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	ramp := []leadconfig.Ramp{{FromDay: 0, PerDay: 5}, {FromDay: 7, PerDay: 10}, {FromDay: 14, PerDay: 20}}
	at := func(s string) *time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		d = d.Add(9 * time.Hour)
		return &d
	}
	for _, tc := range []struct {
		name      string
		start     string
		firstSend *time.Time
		wantCap   int
		wantDay   int
	}{
		{"fixed start, nothing sent yet", "2026-10-20", nil, 10, 11},
		{"fixed start, first send later than the start", "2026-10-20", at("2026-10-25"), 10, 11},
		{"fixed start, first send before the start", "2026-10-28", at("2026-10-26"), 5, 3},
		{"fixed start in the future", "2026-11-02", nil, 0, 0},
		{"first-send, nothing sent yet", leadconfig.FirstSend, nil, 5, 0},
		{"first-send, sent before", leadconfig.FirstSend, at("2026-10-21"), 10, 10},
		{"first-send, first send today", leadconfig.FirstSend, at("2026-10-30"), 5, 1},
		{"no start date", "", nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := leadconfig.Config{}
			cfg.Caps.StartDate = leadconfig.ParseDate(tc.start)
			cfg.Caps.Ramp = ramp
			m := leadstore.MailboxStatus{FirstLiveSend: tc.firstSend}
			m.ID, m.Domain = 1, "sender.example"
			capToday, rampDay := mailboxRamp(cfg, m, time.UTC, now)
			if want := outreach.CapOn(cfg, tc.firstSend, now); capToday != want {
				t.Errorf("limit %d, CapOn %d", capToday, want)
			}
			if capToday != tc.wantCap || rampDay != tc.wantDay {
				t.Errorf("limit %d, day %d; want limit %d, day %d", capToday, rampDay, tc.wantCap, tc.wantDay)
			}
			// The card's day, as CapOn counts it (day 0 is the card's day 1), gives CapOn's
			// limit: the two count from the same day.
			if rampDay > 0 {
				if got := rampCapOnDay(cfg, rampDay-1); got != capToday {
					t.Errorf("day %d of the ramp is limit %d by the ramp, but today's limit is %d", rampDay, got, capToday)
				}
			} else if capToday > 0 && !cfg.Caps.StartDate.IsFirstSend() {
				t.Errorf("the card says the ramp has not started, but today's limit is %d", capToday)
			}
		})
	}
}

// rampCapOnDay is the ramp's limit days after day 0, read straight from the config.
func rampCapOnDay(cfg leadconfig.Config, days int) int {
	perDay := 0
	for _, r := range cfg.Caps.Ramp {
		if r.FromDay <= days {
			perDay = r.PerDay
		}
	}
	return min(perDay, leadconfig.HardDailyCeiling)
}
