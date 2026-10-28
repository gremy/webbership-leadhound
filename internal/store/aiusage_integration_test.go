//go:build integration

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func TestAIUsageCounts(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, loc)

	if n, err := q.AICallsOn(ctx, "typesafe", "", day); err != nil || n != 0 {
		t.Fatalf("empty AICallsOn = %d, %v; want 0, nil", n, err)
	}
	rows := []ai.UsageRow{
		{Provider: "typesafe", Model: "jev-1.13.0", Day: day, Calls: 1, InputTokens: 300},
		{Provider: "typesafe", Model: "jev-1.13.0", Day: day, Calls: 1, InputTokens: 250},
		{Provider: "fake", Model: "fake", Day: day, Calls: 1},
		{Provider: "typesafe", Model: "jev-1.13.0", Day: day.AddDate(0, 0, 1), Calls: 1, InputTokens: 10},
	}
	for _, r := range rows {
		if err := q.RecordAIUsage(ctx, r); err != nil {
			t.Fatalf("RecordAIUsage %+v: %v", r, err)
		}
	}
	if n, err := q.AICallsOn(ctx, "typesafe", "", day); err != nil || n != 2 {
		t.Fatalf("AICallsOn typesafe = %d, %v; want 2, nil", n, err)
	}
	if n, err := q.AICallsOn(ctx, "fake", "", day); err != nil || n != 1 {
		t.Fatalf("AICallsOn fake = %d, %v; want 1, nil", n, err)
	}

	// The local date is stored, not the UTC one: Bucharest midnight is 21:00 UTC the day before.
	var stored string
	var tokens int
	err := storetest.AppConn(t).QueryRow(ctx,
		`SELECT day::text, input_tokens FROM ai_usage WHERE id = 1`).Scan(&stored, &tokens)
	if err != nil || stored != "2026-10-23" || tokens != 300 {
		t.Fatalf("row 1 = %s, %d, %v; want 2026-10-23, 300", stored, tokens, err)
	}
}

// TestAIUsagePurposeDefault proves 00008: a row written without a purpose is a jobs row, the
// count reads one purpose or (with "") every purpose, and a purpose outside the two is refused.
func TestAIUsagePurposeDefault(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	day := time.Date(2026, 10, 23, 0, 0, 0, 0, bucharest(t))
	app := storetest.AppConn(t)

	// A writer that predates the column names no purpose.
	if _, err := app.Exec(ctx,
		`INSERT INTO ai_usage (provider, model, day, calls, input_tokens) VALUES ('typesafe', 'm', '2026-10-23', 1, 5)`); err != nil {
		t.Fatalf("insert without purpose: %v", err)
	}
	for _, r := range []ai.UsageRow{
		{Provider: "typesafe", Model: "m", Day: day, Calls: 1},                           // "" is jobs
		{Provider: "typesafe", Model: "m", Purpose: ai.PurposeLeads, Day: day, Calls: 1}, // leads
		{Provider: "typesafe", Model: "m", Purpose: ai.PurposeLeads, Day: day, Calls: 1}, // leads
		{Provider: "fake", Model: "fake", Purpose: ai.PurposeLeads, Day: day, Calls: 1},  // another provider
	} {
		if err := q.RecordAIUsage(ctx, r); err != nil {
			t.Fatalf("RecordAIUsage %+v: %v", r, err)
		}
	}
	var jobsRows int
	if err := app.QueryRow(ctx,
		`SELECT count(*) FROM ai_usage WHERE provider = 'typesafe' AND purpose = 'jobs'`).Scan(&jobsRows); err != nil || jobsRows != 2 {
		t.Fatalf("typesafe jobs rows = %d, %v; want 2 (the default and the empty purpose)", jobsRows, err)
	}
	for _, c := range []struct {
		purpose string
		want    int
	}{{"", 4}, {ai.PurposeJobs, 2}, {ai.PurposeLeads, 2}} {
		if n, err := q.AICallsOn(ctx, "typesafe", c.purpose, day); err != nil || n != c.want {
			t.Fatalf("AICallsOn typesafe %q = %d, %v; want %d", c.purpose, n, err, c.want)
		}
	}

	err := q.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "m", Purpose: "other", Day: day, Calls: 1})
	if sqlState(err) != "23514" {
		t.Fatalf("purpose other: err = %v, want 23514 check_violation", err)
	}
}

// TestCappedAgainstTheDatabase runs the cap end to end on ai_usage: two calls pass, the
// third is ErrSpendCap and never reaches the Decider.
func TestCappedAgainstTheDatabase(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	qs := []ai.Question{{Name: "q", Options: []ai.Option{{Name: "a"}}}}
	f := &ai.Fake{Answers: map[string]ai.Answer{"q": {Choice: "a"}}, Usage: ai.Usage{InputTokens: 7}}
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	c := &ai.Capped{Decider: f, Store: q, DailyCalls: 2, Loc: bucharest(t), Now: func() time.Time { return now }}

	for i := range 2 {
		if _, _, err := c.Decide(ctx, "s", qs); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if _, _, err := c.Decide(ctx, "s", qs); !errors.Is(err, ai.ErrSpendCap) {
		t.Fatalf("call 3: err = %v, want ErrSpendCap", err)
	}
	if f.Calls() != 2 {
		t.Fatalf("decider calls = %d, want 2", f.Calls())
	}
}

// slowDecider holds each call open for a while, so two callers overlap.
type slowDecider struct {
	mu    sync.Mutex
	calls int
}

func (*slowDecider) Name() string     { return "slow" }
func (*slowDecider) Provider() string { return "slow" }

func (d *slowDecider) Decide(context.Context, string, []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	time.Sleep(200 * time.Millisecond)
	return map[string]ai.Answer{"q": {Choice: "a"}}, ai.Usage{InputTokens: 1}, nil
}

// TestCappedLockAcrossCappers: two Capped values (as two processes would hold) share the
// database's advisory lock, so a cap of one allows exactly one call between them.
func TestCappedLockAcrossCappers(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	qs := []ai.Question{{Name: "q", Options: []ai.Option{{Name: "a"}}}}
	d := &slowDecider{}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		c := &ai.Capped{Decider: d, Store: s.Q(), DailyCalls: 1, Loc: time.UTC}
		wg.Go(func() { _, _, errs[i] = c.Decide(ctx, "s", qs) })
	}
	wg.Wait()
	capped := 0
	for _, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ai.ErrSpendCap):
			capped++
		default:
			t.Fatalf("err = %v", err)
		}
	}
	if d.calls != 1 || capped != 1 {
		t.Fatalf("decider calls = %d, capped = %d; want 1 and 1", d.calls, capped)
	}
	if n, err := s.Q().AICallsOn(ctx, "slow", "", time.Now().UTC()); err != nil || n != 1 {
		t.Fatalf("recorded calls = %d, %v; want 1", n, err)
	}
}

// TestUsageLockRollsBackOnError: a failing fn leaves no row, and its error comes back as it is.
func TestUsageLockRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	boom := errors.New("boom")
	err := q.WithUsageLock(ctx, "typesafe", func(st ai.UsageStore) error {
		if err := st.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "m", Day: time.Now(), Calls: 1}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if n, err := q.AICallsOn(ctx, "typesafe", "", time.Now()); err != nil || n != 0 {
		t.Fatalf("calls after rollback = %d, %v; want 0", n, err)
	}
}

// TestUsageLockCommitsAfterCancel: a context cancelled after fn recorded its row (a Claude
// call billed, then the caller timed out) still commits that row, so the spend is never lost.
func TestUsageLockCommitsAfterCancel(t *testing.T) {
	q := appStore(t).Q()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := q.WithUsageLock(ctx, "typesafe", func(st ai.UsageStore) error {
		if err := st.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "m", Day: time.Now(), Calls: 1}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err != nil {
		t.Fatalf("WithUsageLock after cancel = %v, want nil", err)
	}
	if ctx.Err() == nil {
		t.Fatal("ctx was not cancelled")
	}
	if n, err := q.AICallsOn(context.Background(), "typesafe", "", time.Now()); err != nil || n != 1 {
		t.Fatalf("calls after a cancelled commit = %d, %v; want 1", n, err)
	}
}
