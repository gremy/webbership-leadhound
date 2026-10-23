package ai

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// UsageRow is one ai_usage row: what one billed call cost, on a day in the cap's zone.
type UsageRow struct {
	Provider, Model    string
	Day                time.Time // midnight of the local day, in Capped.Loc
	Calls, InputTokens int
}

// UsageStore is what Capped needs from the database (store.Queries implements it).
type UsageStore interface {
	// AICallsOn sums the calls recorded for provider on day's calendar date.
	AICallsOn(ctx context.Context, provider string, day time.Time) (int, error)
	RecordAIUsage(ctx context.Context, r UsageRow) error
}

// UsageLocker is a UsageStore that can hold a lock shared by every process on the same
// database. WithUsageLock runs fn on a store that holds the provider's lock until fn
// returns; what fn records is kept only when fn returns nil.
type UsageLocker interface {
	WithUsageLock(ctx context.Context, provider string, fn func(UsageStore) error) error
}

// Capped wraps a Decider with a daily call cap per provider, counted in ai_usage on the
// calendar day in Loc (LEADHOUND_TZ). At or over DailyCalls it returns ErrSpendCap without
// calling; after every call the provider billed (a success, or ErrBadAnswer) it records one
// row. Calls through one Capped run one at a time; when Store is a UsageLocker, the read,
// the call and the record also run under its lock, so processes sharing the database cannot
// overshoot the cap together either.
type Capped struct {
	Decider    Decider
	Store      UsageStore
	DailyCalls int
	Loc        *time.Location
	Now        func() time.Time // time.Now when nil

	mu sync.Mutex
}

// Name is the wrapped Decider's name.
func (c *Capped) Name() string { return c.Decider.Name() }

// Decide checks today's count, calls the wrapped Decider and records the call. A usage
// read or write that fails is ErrUsageStore, and the caller must stop.
func (c *Capped) Decide(ctx context.Context, state string, qs []Question) (map[string]Answer, Usage, error) {
	if c.Loc == nil {
		return nil, Usage{}, errors.New("ai: capped: nil location")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	local := now().In(c.Loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.Loc)
	provider := providerOf(c.Decider)

	var (
		answers map[string]Answer
		usage   Usage
		callErr error // the cap's or the Decider's outcome, returned as it is
	)
	// body returns only usage-store failures, so a locker keeps the recorded row whatever
	// the Decider said.
	body := func(st UsageStore) error {
		used, err := st.AICallsOn(ctx, provider, day)
		if err != nil {
			return fmt.Errorf("ai: capped: read usage: %w: %w", ErrUsageStore, err)
		}
		if used >= c.DailyCalls {
			callErr = fmt.Errorf("ai: %s: %d of %d calls used on %s: %w",
				provider, used, c.DailyCalls, day.Format(time.DateOnly), ErrSpendCap)
			return nil
		}
		answers, usage, callErr = c.Decider.Decide(ctx, state, qs)
		if callErr != nil && !errors.Is(callErr, ErrBadAnswer) {
			return nil // never billed: nothing to record
		}
		row := UsageRow{Provider: provider, Model: c.Decider.Name(), Day: day, Calls: 1, InputTokens: usage.InputTokens}
		if err := st.RecordAIUsage(ctx, row); err != nil {
			// The call was billed but not counted; stopping keeps the cap from under-counting.
			return fmt.Errorf("ai: capped: record usage: %w: %w", ErrUsageStore, err)
		}
		return nil
	}

	var err error
	if l, ok := c.Store.(UsageLocker); ok {
		err = l.WithUsageLock(ctx, provider, body)
		if err != nil && !errors.Is(err, ErrUsageStore) {
			// Taking the lock or committing the row failed.
			err = fmt.Errorf("ai: capped: usage lock: %w: %w", ErrUsageStore, err)
		}
	} else {
		err = body(c.Store)
	}
	if err != nil {
		return nil, Usage{}, err
	}
	if callErr != nil {
		return nil, usage, callErr
	}
	return answers, usage, nil
}

// providerOf is the Decider's provider when it names one, else its name.
func providerOf(d Decider) string {
	if p, ok := d.(interface{ Provider() string }); ok {
		return p.Provider()
	}
	return d.Name()
}
