//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
)

func bucharest(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func TestStartFinishRun(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	id, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if id != 1 {
		t.Fatalf("first run id = %d, want 1", id)
	}
	ok, err := q.HasWarmRun(ctx, "collect", "linkedin")
	if err != nil || ok {
		t.Fatalf("HasWarmRun before finish = %v, %v; want false, nil", ok, err)
	}

	res := store.RunResult{Requests: 7, Failures: 2, StopReason: "budget", Note: "budget spent"}
	if err := q.FinishRun(ctx, id, res); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	run, err := q.LatestRunToday(ctx, "collect", "linkedin", time.UTC, time.Now())
	if err != nil {
		t.Fatalf("LatestRunToday: %v", err)
	}
	if run.ID != id || run.FinishedAt == nil || run.Requests != 7 || run.Failures != 2 ||
		run.StopReason != "budget" || run.Note != "budget spent" {
		t.Fatalf("finished run = %+v", run)
	}
	// A run that only ran out of budget still ends the cold start.
	if ok, err := q.HasWarmRun(ctx, "collect", "linkedin"); err != nil || !ok {
		t.Fatalf("HasWarmRun after a budget stop = %v, %v; want true, nil", ok, err)
	}
	// Another source or command does not count.
	if ok, err := q.HasWarmRun(ctx, "collect", "greenhouse"); err != nil || ok {
		t.Fatalf("HasWarmRun other source = %v, %v; want false, nil", ok, err)
	}
	if ok, err := q.HasWarmRun(ctx, "canary", "linkedin"); err != nil || ok {
		t.Fatalf("HasWarmRun other command = %v, %v; want false, nil", ok, err)
	}
}

func TestHasWarmRun(t *testing.T) {
	cases := []struct {
		name string
		res  *store.RunResult // nil: never finished
		want bool
	}{
		{"unfinished", nil, false},
		{"clean", &store.RunResult{Requests: 3}, true},
		{"budget", &store.RunResult{Requests: 3, StopReason: "budget"}, true},
		{"clean without a request", &store.RunResult{}, false},
		{"budget without a request", &store.RunResult{StopReason: "budget"}, false},
		{"throttled", &store.RunResult{Requests: 2, StopReason: "throttled"}, false},
		{"blocked", &store.RunResult{Requests: 1, StopReason: "blocked"}, false},
		{"failure_rate", &store.RunResult{Requests: 5, StopReason: "failure_rate"}, false},
		{"parser_broke", &store.RunResult{Requests: 1, StopReason: "parser_broke"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			q := appStore(t).Q()
			id, err := q.StartRun(ctx, "collect", "linkedin")
			if err != nil {
				t.Fatal(err)
			}
			if tc.res != nil {
				if err := q.FinishRun(ctx, id, *tc.res); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := q.HasWarmRun(ctx, "collect", "linkedin"); err != nil || got != tc.want {
				t.Fatalf("HasWarmRun = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestFinishRunErrors(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	if err := q.FinishRun(ctx, 999, store.RunResult{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("FinishRun missing id = %v, want ErrNotFound", err)
	}
	id, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := q.FinishRun(ctx, id, store.RunResult{StopReason: "no-such-reason"}); err == nil {
		t.Fatal("FinishRun with an unknown stop reason succeeded; want the CHECK to refuse it")
	}
}

// TestHasRunTodayAcrossMidnight places runs either side of midnight in Europe/Bucharest (UTC+3
// in summer) and asks with fixed "now" values, so "today" is the calendar day in that zone,
// not in UTC.
func TestHasRunTodayAcrossMidnight(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)

	// 23:30 on 14 July in Bucharest is 20:30 UTC on 14 July; 00:30 on 15 July is 21:30 UTC on 14 July.
	lateEvening := time.Date(2026, 7, 14, 23, 30, 0, 0, loc)
	afterMidnight := time.Date(2026, 7, 15, 0, 30, 0, 0, loc)

	throttled, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := q.FinishRun(ctx, throttled, store.RunResult{StopReason: "throttled"}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	ownerExec(t, "UPDATE run SET started_at = $2, finished_at = $2 WHERE id = $1", throttled, lateEvening)

	cases := []struct {
		name    string
		command string
		source  string
		now     time.Time
		reasons []string
		want    bool
	}{
		{"same evening, any reason", "collect", "linkedin", lateEvening.Add(10 * time.Minute), nil, true},
		{"same evening, throttled or blocked", "collect", "linkedin", lateEvening.Add(10 * time.Minute), []string{"throttled", "blocked"}, true},
		{"same evening, blocked only", "collect", "linkedin", lateEvening.Add(10 * time.Minute), []string{"blocked"}, false},
		// Still 14 July in UTC, but already 15 July in Bucharest: a new day.
		{"after local midnight", "collect", "linkedin", afterMidnight, []string{"throttled", "blocked"}, false},
		{"after local midnight, any reason", "collect", "linkedin", afterMidnight, nil, false},
		{"the morning before", "collect", "linkedin", time.Date(2026, 7, 14, 0, 0, 0, 0, loc), nil, true},
		{"the day before", "collect", "linkedin", time.Date(2026, 7, 13, 23, 59, 59, 0, loc), nil, false},
		{"other source", "collect", "greenhouse", lateEvening, nil, false},
		{"other command", "canary", "linkedin", lateEvening, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.HasRunToday(ctx, tc.command, tc.source, loc, tc.now, tc.reasons...)
			if err != nil {
				t.Fatalf("HasRunToday: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasRunToday = %v, want %v", got, tc.want)
			}
		})
	}

	// A run started just after local midnight counts for the new day, not the old one.
	canary, err := q.StartRun(ctx, "canary", "linkedin")
	if err != nil {
		t.Fatalf("StartRun canary: %v", err)
	}
	ownerExec(t, "UPDATE run SET started_at = $2 WHERE id = $1", canary, afterMidnight)
	if got, err := q.HasRunToday(ctx, "canary", "linkedin", loc, lateEvening); err != nil || got {
		t.Fatalf("canary on the old day = %v, %v; want false, nil", got, err)
	}
	if got, err := q.HasRunToday(ctx, "canary", "linkedin", loc, afterMidnight.Add(time.Hour)); err != nil || !got {
		t.Fatalf("canary on the new day = %v, %v; want true, nil", got, err)
	}

	run, err := q.LatestRunToday(ctx, "collect", "linkedin", loc, lateEvening, "throttled")
	if err != nil {
		t.Fatalf("LatestRunToday: %v", err)
	}
	if run.ID != throttled || run.StopReason != "throttled" {
		t.Fatalf("LatestRunToday = %+v, want run %d throttled", run, throttled)
	}
	if _, err := q.LatestRunToday(ctx, "collect", "linkedin", loc, afterMidnight); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestRunToday on an empty day = %v, want ErrNotFound", err)
	}
	if _, err := q.HasRunToday(ctx, "collect", "linkedin", nil, lateEvening); err == nil {
		t.Fatal("HasRunToday with a nil location succeeded; want an error")
	}
}

// TestPausedToday: a throttled or blocked run of any command pauses its source for the
// calendar day it ended on in Europe/Bucharest, and no other source or day.
func TestPausedToday(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)
	lateEvening := time.Date(2026, 7, 14, 23, 30, 0, 0, loc)
	afterMidnight := time.Date(2026, 7, 15, 0, 30, 0, 0, loc)

	add := func(command, source, reason string, started, finished time.Time) {
		t.Helper()
		id, err := q.StartRun(ctx, command, source)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.FinishRun(ctx, id, store.RunResult{StopReason: reason}); err != nil {
			t.Fatal(err)
		}
		ownerExec(t, "UPDATE run SET started_at = $2, finished_at = $3 WHERE id = $1", id, started, finished)
	}
	// A probe started before midnight and blocked after it: the new day is paused.
	add("probe", "linkedin", "blocked", lateEvening, afterMidnight)
	// Reasons that do not pause, and another source.
	add("collect", "linkedin", "budget", lateEvening, lateEvening)
	add("collect", "linkedin", "parser_broke", lateEvening, lateEvening)
	add("canary", "linkedin-fixture", "throttled", lateEvening, lateEvening)

	cases := []struct {
		name   string
		source string
		now    time.Time
		want   string
	}{
		{"the old day", "linkedin", lateEvening.Add(10 * time.Minute), ""},
		{"the new day", "linkedin", afterMidnight.Add(time.Hour), "blocked"},
		{"the day after", "linkedin", afterMidnight.Add(24 * time.Hour), ""},
		{"the fixture source on its own day", "linkedin-fixture", lateEvening, "throttled"},
		{"the fixture source the next day", "linkedin-fixture", afterMidnight, ""},
		{"another source", "greenhouse", afterMidnight, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, paused, err := q.PausedToday(ctx, tc.source, loc, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tc.want || paused != (tc.want != "") {
				t.Fatalf("PausedToday = %q, %v; want %q", reason, paused, tc.want)
			}
		})
	}

	// A later throttled collect the same day is the reason reported.
	add("collect", "linkedin", "throttled", afterMidnight.Add(time.Hour), afterMidnight.Add(2*time.Hour))
	if reason, _, err := q.PausedToday(ctx, "linkedin", loc, afterMidnight.Add(3*time.Hour)); err != nil || reason != "throttled" {
		t.Fatalf("PausedToday = %q, %v; want throttled", reason, err)
	}
	if _, _, err := q.PausedToday(ctx, "linkedin", nil, afterMidnight); err == nil {
		t.Fatal("PausedToday with a nil location succeeded; want an error")
	}
}

// TestLatestRunTodayUsesFinishTime: a run started before local midnight and finished after
// it belongs to the new day.
func TestLatestRunTodayUsesFinishTime(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)
	lateEvening := time.Date(2026, 7, 14, 23, 50, 0, 0, loc)
	afterMidnight := time.Date(2026, 7, 15, 0, 10, 0, 0, loc)
	id, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.FinishRun(ctx, id, store.RunResult{StopReason: "throttled"}); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, "UPDATE run SET started_at = $2, finished_at = $3 WHERE id = $1", id, lateEvening, afterMidnight)
	if got, err := q.HasRunToday(ctx, "collect", "linkedin", loc, afterMidnight.Add(time.Hour), "throttled"); err != nil || !got {
		t.Fatalf("new day = %v, %v; want true", got, err)
	}
	if got, err := q.HasRunToday(ctx, "collect", "linkedin", loc, lateEvening, "throttled"); err != nil || got {
		t.Fatalf("old day = %v, %v; want false", got, err)
	}
}
