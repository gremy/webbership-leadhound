package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // Europe/Bucharest without relying on the host's zoneinfo

	"github.com/gremy/webbership-leadhound/internal/config"
)

// memUsage is an in-memory UsageStore.
type memUsage struct {
	mu      sync.Mutex
	rows    []UsageRow
	err     error // returned by RecordAIUsage when set
	readErr error // returned by AICallsOn when set
}

func (m *memUsage) AICallsOn(_ context.Context, provider string, day time.Time) (int, error) {
	if m.readErr != nil {
		return 0, m.readErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.rows {
		if r.Provider == provider && r.Day.Format(time.DateOnly) == day.Format(time.DateOnly) {
			n += r.Calls
		}
	}
	return n, nil
}

func (m *memUsage) RecordAIUsage(_ context.Context, r UsageRow) error {
	if m.err != nil {
		return m.err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, r)
	return nil
}

func loadBucharest(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func TestCappedStopsAtTheCapWithoutCalling(t *testing.T) {
	const capN = 3
	qs := fiveQuestions()
	srv, rec := serve(t, reply{200, okBody(qs)})
	j, _ := newTestJev(t, srv)
	loc := loadBucharest(t)
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	st := &memUsage{}
	c := &Capped{Decider: j, Store: st, DailyCalls: capN, Loc: loc, Now: func() time.Time { return now }}

	if c.Name() != "jev-1.13.0" {
		t.Fatalf("Name = %q", c.Name())
	}
	for i := range capN {
		if _, _, err := c.Decide(context.Background(), fiveState, qs); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if rec.hits() != capN {
		t.Fatalf("HTTP calls = %d, want %d", rec.hits(), capN)
	}
	_, _, err := c.Decide(context.Background(), fiveState, qs)
	if !errors.Is(err, ErrSpendCap) {
		t.Fatalf("call %d: err = %v, want ErrSpendCap", capN+1, err)
	}
	if rec.hits() != capN {
		t.Fatalf("HTTP calls after the cap = %d, want %d (zero more)", rec.hits(), capN)
	}

	if len(st.rows) != capN {
		t.Fatalf("usage rows = %d, want %d", len(st.rows), capN)
	}
	r := st.rows[0]
	if r.Provider != "typesafe" || r.Model != "jev-1.13.0" || r.Calls != 1 || r.InputTokens != 321 ||
		r.Day.Format(time.DateOnly) != "2026-10-23" || r.Day.Location() != loc {
		t.Fatalf("usage row = %+v", r)
	}

	// The next local day starts a fresh count.
	now = time.Date(2026, 10, 23, 21, 30, 0, 0, time.UTC) // 00:30 on the 24th in Bucharest
	if _, _, err := c.Decide(context.Background(), fiveState, qs); err != nil {
		t.Fatalf("next local day: %v", err)
	}
	if got := st.rows[len(st.rows)-1].Day.Format(time.DateOnly); got != "2026-10-24" {
		t.Fatalf("next-day row day = %s, want 2026-10-24", got)
	}
}

func TestCappedCountsPerProvider(t *testing.T) {
	loc := loadBucharest(t)
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)
	st := &memUsage{rows: []UsageRow{{Provider: "typesafe", Model: "jev-1.13.0", Day: now.In(loc), Calls: 5}}}
	qs := []Question{{Name: "q", Options: []Option{{Name: "a"}}}}
	f := &Fake{Answers: map[string]Answer{"q": {Choice: "a"}}}
	c := &Capped{Decider: f, Store: st, DailyCalls: 5, Loc: loc, Now: func() time.Time { return now }}
	if _, _, err := c.Decide(context.Background(), "s", qs); err != nil {
		t.Fatalf("fake calls must not count against the typesafe cap: %v", err)
	}
	if st.rows[1].Provider != "fake" || st.rows[1].Model != "fake" {
		t.Fatalf("fake usage row = %+v", st.rows[1])
	}
}

func TestCappedFailures(t *testing.T) {
	loc := loadBucharest(t)
	qs := []Question{{Name: "q", Options: []Option{{Name: "a"}}}}

	// A failed call records nothing.
	st := &memUsage{}
	c := &Capped{Decider: &Fake{Err: ErrTooLong}, Store: st, DailyCalls: 2, Loc: loc}
	if _, _, err := c.Decide(context.Background(), "s", qs); !errors.Is(err, ErrTooLong) {
		t.Fatalf("err = %v, want ErrTooLong", err)
	}
	if len(st.rows) != 0 {
		t.Fatalf("rows after a failed call = %d, want 0", len(st.rows))
	}

	// A usage row that cannot be written fails the call as ErrUsageStore, so the caller
	// stops instead of making calls the cap cannot count.
	st = &memUsage{err: errors.New("db down")}
	c = &Capped{Decider: &Fake{Answers: map[string]Answer{"q": {Choice: "a"}}}, Store: st, DailyCalls: 2, Loc: loc}
	if _, _, err := c.Decide(context.Background(), "s", qs); !errors.Is(err, ErrUsageStore) {
		t.Fatalf("err = %v when the usage row could not be written, want ErrUsageStore", err)
	}
	// So is a count that cannot be read, and then no call is made.
	rf := &Fake{Answers: map[string]Answer{"q": {Choice: "a"}}}
	c = &Capped{Decider: rf, Store: &memUsage{readErr: errors.New("db down")}, DailyCalls: 2, Loc: loc}
	if _, _, err := c.Decide(context.Background(), "s", qs); !errors.Is(err, ErrUsageStore) || rf.Calls() != 0 {
		t.Fatalf("read failure: err = %v, calls %d; want ErrUsageStore and 0", err, rf.Calls())
	}

	// A zero cap allows nothing; a missing location is an error, not UTC.
	f := &Fake{Answers: map[string]Answer{"q": {Choice: "a"}}}
	c = &Capped{Decider: f, Store: &memUsage{}, DailyCalls: 0, Loc: loc}
	if _, _, err := c.Decide(context.Background(), "s", qs); !errors.Is(err, ErrSpendCap) {
		t.Fatalf("zero cap: err = %v, want ErrSpendCap", err)
	}
	c = &Capped{Decider: f, Store: &memUsage{}, DailyCalls: 2}
	if _, _, err := c.Decide(context.Background(), "s", qs); err == nil || errors.Is(err, ErrSpendCap) {
		t.Fatalf("nil location: err = %v", err)
	}
	if f.Calls() != 0 {
		t.Fatalf("fake calls = %d, want 0", f.Calls())
	}
}

// TestCappedKeyNeverPrinted formats a Capped around a real Jev.
func TestCappedKeyNeverPrinted(t *testing.T) {
	j := NewJev(config.Secret(testKey), "jev-1.13.0")
	c := &Capped{Decider: j, Store: &memUsage{}, DailyCalls: 1, Loc: time.UTC}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		if s := fmt.Sprintf(verb, c); strings.Contains(s, testKey) {
			t.Fatalf("%s of Capped printed the key", verb)
		}
	}
}

// TestCappedCountsBilledBadAnswers: a 200 whose answer is unusable was billed, so it is
// recorded with its tokens and counts against the cap; the error still reaches the caller.
func TestCappedCountsBilledBadAnswers(t *testing.T) {
	qs := fiveQuestions()
	bad := strings.Replace(okBody(qs), `"choice":"yes"`, `"choice":"maybe"`, 1)
	srv, rec := serve(t, reply{200, bad}, reply{200, bad})
	j, _ := newTestJev(t, srv)
	st := &memUsage{}
	c := &Capped{Decider: j, Store: st, DailyCalls: 1, Loc: time.UTC}

	_, u, err := c.Decide(context.Background(), fiveState, qs)
	if !errors.Is(err, ErrBadAnswer) || u.InputTokens != 321 {
		t.Fatalf("err = %v, usage %+v; want ErrBadAnswer with 321 tokens", err, u)
	}
	if len(st.rows) != 1 || st.rows[0].Calls != 1 || st.rows[0].InputTokens != 321 {
		t.Fatalf("usage rows = %+v, want one row of 321 tokens", st.rows)
	}
	if _, _, err := c.Decide(context.Background(), fiveState, qs); !errors.Is(err, ErrSpendCap) {
		t.Fatalf("second call: err = %v, want ErrSpendCap", err)
	}
	if rec.hits() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", rec.hits())
	}

	// A call that never reached a 200 is not billed and not recorded.
	srv, _ = serve(t, reply{401, `{"detail":"bad key"}`})
	j, _ = newTestJev(t, srv)
	st = &memUsage{}
	c = &Capped{Decider: j, Store: st, DailyCalls: 1, Loc: time.UTC}
	if _, _, err := c.Decide(context.Background(), fiveState, qs); err == nil || len(st.rows) != 0 {
		t.Fatalf("401: err = %v, rows %d; want an error and 0 rows", err, len(st.rows))
	}
}

// lockingUsage is a memUsage with a UsageLocker that, like a transaction, keeps what fn
// recorded only when fn returns nil.
type lockingUsage struct {
	memUsage
	locks   int
	lockErr error
}

func (l *lockingUsage) WithUsageLock(_ context.Context, provider string, fn func(UsageStore) error) error {
	if l.lockErr != nil {
		return l.lockErr
	}
	l.locks++
	if provider == "" {
		return errors.New("no provider")
	}
	before := len(l.rows)
	if err := fn(&l.memUsage); err != nil {
		l.rows = l.rows[:before]
		return err
	}
	return nil
}

// TestCappedUsesTheLock: with a UsageLocker every call runs under it, a billed bad answer
// is still kept (the lock body does not fail on it), and a lock that cannot be taken is
// ErrUsageStore with no call made.
func TestCappedUsesTheLock(t *testing.T) {
	qs := []Question{{Name: "q", Options: []Option{{Name: "a"}}}}
	st := &lockingUsage{}
	f := &Fake{Answers: map[string]Answer{"q": {Choice: "a"}}, Usage: Usage{InputTokens: 4}}
	c := &Capped{Decider: f, Store: st, DailyCalls: 1, Loc: time.UTC}
	if _, _, err := c.Decide(context.Background(), "s", qs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Decide(context.Background(), "s", qs); !errors.Is(err, ErrSpendCap) {
		t.Fatalf("err = %v, want ErrSpendCap", err)
	}
	if st.locks != 2 || len(st.rows) != 1 || f.Calls() != 1 {
		t.Fatalf("locks %d, rows %d, calls %d; want 2, 1, 1", st.locks, len(st.rows), f.Calls())
	}

	st = &lockingUsage{}
	c = &Capped{Decider: &Fake{Err: fmt.Errorf("x: %w", ErrBadAnswer)}, Store: st, DailyCalls: 1, Loc: time.UTC}
	if _, _, err := c.Decide(context.Background(), "s", qs); !errors.Is(err, ErrBadAnswer) || len(st.rows) != 1 {
		t.Fatalf("bad answer under the lock: err = %v, rows %d; want ErrBadAnswer and 1", err, len(st.rows))
	}

	f = &Fake{Answers: map[string]Answer{"q": {Choice: "a"}}}
	c = &Capped{Decider: f, Store: &lockingUsage{lockErr: errors.New("conn refused")}, DailyCalls: 1, Loc: time.UTC}
	if _, _, err := c.Decide(context.Background(), "s", qs); !errors.Is(err, ErrUsageStore) || f.Calls() != 0 {
		t.Fatalf("lock failure: err = %v, calls %d; want ErrUsageStore and 0", err, f.Calls())
	}
}
