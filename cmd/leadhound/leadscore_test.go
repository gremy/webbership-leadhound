package main

import (
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

// sizingConfig is a ramp of perDay a day from startDate (a date or first-send), weekdays,
// two follow-ups (three steps a lead) and a batch size of 10.
func sizingConfig(t *testing.T, startDate string, perDay int) leadconfig.Config {
	t.Helper()
	var cfg leadconfig.Config
	if err := yaml.Unmarshal([]byte("start_date: "+startDate+"\n"), &cfg.Caps); err != nil {
		t.Fatal(err)
	}
	cfg.Caps.Ramp = []leadconfig.Ramp{{FromDay: 0, PerDay: perDay}, {FromDay: 7, PerDay: perDay * 3}}
	cfg.Caps.BatchSize = 10
	cfg.Schedule.FollowupBusinessDays = []int{4, 7}
	cfg.Schedule.BusinessHours = leadconfig.BusinessHours{Start: "09:30", End: "16:30", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri"}}
	return cfg
}

// TestBatchSizing: room = the ramp over today and the next business days (3 in all) minus
// the approved emails not yet sent; leads = room / steps, at most the size asked for.
func TestBatchSizing(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	tue := time.Date(2026, 10, 27, 11, 0, 0, 0, loc)
	sat := time.Date(2026, 10, 31, 11, 0, 0, 0, loc)
	day := func(d time.Time) *time.Time { return &d }
	for _, c := range []struct {
		name      string
		start     string
		perDay    int
		firstSend *time.Time
		now       time.Time
		waiting   int
		size      int
		want      BatchSizing
		words     string
		sentToday int // emails today's cap has used
		toCheck   int // maybe sent or left sending
	}{
		{"ramp 4, nothing waiting", "2026-10-27", 4, nil, tue, 0, 10,
			BatchSizing{Days: 3, Today: true, Ramp: 12, Room: 12, Steps: 3, Size: 10, Leads: 4},
			"4 leads: room for 12 emails over the next 3 days, and each lead has 3 steps", 0, 0},
		{"ramp 4, 10 approved unsent", "2026-10-27", 4, nil, tue, 10, 10,
			BatchSizing{Days: 3, Today: true, Ramp: 12, Waiting: 10, Room: 2, Steps: 3, Size: 10, Leads: 0},
			"0 leads: room for 2 emails over the next 3 days (12 by the warm-up ramp, 10 approved and waiting), and each lead has 3 steps", 0, 0},
		{"ramp 4, 9 waiting", "2026-10-27", 4, nil, tue, 9, 10,
			BatchSizing{Days: 3, Today: true, Ramp: 12, Waiting: 9, Room: 3, Steps: 3, Size: 10, Leads: 1}, "", 0, 0},
		{"size below room", "2026-10-27", 4, nil, tue, 0, 2,
			BatchSizing{Days: 3, Today: true, Ramp: 12, Room: 12, Steps: 3, Size: 2, Leads: 2},
			"2 leads (the most asked for): room for 12 emails over the next 3 days, and each lead has 3 steps", 0, 0},
		{"more waiting than the ramp", "2026-10-27", 4, nil, tue, 20, 10,
			BatchSizing{Days: 3, Today: true, Ramp: 12, Waiting: 20, Room: 0, Steps: 3, Size: 10, Leads: 0}, "", 0, 0},
		// Days 6, 7 and 8 of the ramp: one day at 4, two at 12.
		{"the ramp steps up inside the window", "2026-10-21", 4, nil, tue, 0, 10,
			BatchSizing{Days: 3, Today: true, Ramp: 4 + 12 + 12, Room: 28, Steps: 3, Size: 10, Leads: 9}, "", 0, 0},
		// A Saturday counts Mon, Tue and Wed (days 6, 7 and 8), not itself.
		{"weekend skips to the business days", "2026-10-27", 4, nil, sat, 0, 10,
			BatchSizing{Days: 3, Ramp: 4 + 12 + 12, Room: 28, Steps: 3, Size: 10, Leads: 9}, "", 0, 0},
		{"before the ramp starts", "2026-12-01", 4, nil, tue, 0, 10,
			BatchSizing{Days: 3, Today: true, Steps: 3, Size: 10}, "", 0, 0},
		// first-send with no send yet: today is day 0 for all three days, not each its own.
		{"first-send, none yet", leadconfig.FirstSend, 4, nil, tue, 0, 10,
			BatchSizing{Days: 3, Today: true, Ramp: 12, Room: 12, Steps: 3, Size: 10, Leads: 4}, "", 0, 0},
		{"first-send 8 days ago", leadconfig.FirstSend, 4, day(tue.AddDate(0, 0, -8)), tue, 0, 10,
			BatchSizing{Days: 3, Today: true, Ramp: 36, Room: 36, Steps: 3, Size: 10, Leads: 10}, "", 0, 0},
		// The review's case: today's 4 first emails sent, their 8 follow-ups waiting. Today
		// has nothing left and the next two days hold 8, all taken: no room.
		{name: "today's cap used, follow-ups waiting", start: "2026-10-27", perDay: 4, now: tue, waiting: 8, size: 10, sentToday: 4,
			want:  BatchSizing{Days: 3, Today: true, SentToday: 4, Ramp: 0 + 4 + 4, Waiting: 8, Steps: 3, Size: 10},
			words: "0 leads: room for 0 emails over the next 3 days (8 left in the warm-up ramp after 4 sent today, 8 approved and waiting), and each lead has 3 steps"},
		{name: "today half used", start: "2026-10-27", perDay: 4, now: tue, size: 10, sentToday: 2,
			want: BatchSizing{Days: 3, Today: true, SentToday: 2, Ramp: 2 + 4 + 4, Room: 10, Steps: 3, Size: 10, Leads: 3}},
		// After today's window (16:30 in the operator's zone, with no email country set):
		// Wed, Thu and Fri, and today's sends no longer matter.
		{name: "after hours today", start: "2026-10-27", perDay: 4, now: tue.Add(6 * time.Hour), size: 10, sentToday: 1,
			want:  BatchSizing{Days: 3, Ramp: 12, Room: 12, Steps: 3, Size: 10, Leads: 4},
			words: "4 leads: room for 12 emails over the next 3 business days from tomorrow (today's sending is over), and each lead has 3 steps"},
		{name: "maybe sent counted", start: "2026-10-27", perDay: 4, now: tue, waiting: 8, toCheck: 2, size: 10,
			want:  BatchSizing{Days: 3, Today: true, Ramp: 12, Waiting: 10, ToCheck: 2, Room: 2, Steps: 3, Size: 10},
			words: "0 leads: room for 2 emails over the next 3 days (12 by the warm-up ramp, 8 approved and waiting, 2 to check by hand), and each lead has 3 steps"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ld := sizingLoad{firstSend: c.firstSend, sentToday: c.sentToday, approved: c.waiting, toCheck: c.toCheck}
			got := batchSizing(sizingConfig(t, c.start, c.perDay), ld, c.now, c.size)
			if got != c.want {
				t.Errorf("sizing = %+v, want %+v", got, c.want)
			}
			if c.words != "" && got.Words() != c.words {
				t.Errorf("words = %q, want %q", got.Words(), c.words)
			}
		})
	}
}
