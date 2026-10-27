package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// Queries is the usage store ai.Capped counts against, and its cross-process lock.
var (
	_ ai.UsageStore  = Queries{}
	_ ai.UsageLocker = Queries{}
)

// usageLockClass is the first key of the provider's advisory lock; the second is the
// provider's name hashed by Postgres.
const usageLockClass = "leadhound.ai_usage"

// WithUsageLock runs fn inside a transaction holding a transaction-level advisory lock for
// provider, so every process counting against the same database reads, calls and records
// one at a time. fn's rows commit only when it returns nil; fn's error is returned as it is.
// The lock (and one pooled connection) is held for as long as fn runs, the provider call
// included.
func (q Queries) WithUsageLock(ctx context.Context, provider string, fn func(ai.UsageStore) error) error {
	b, ok := q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("store: usage lock: %T cannot begin a transaction", q.db)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: usage lock: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, usageLockClass, provider); err != nil {
		return fmt.Errorf("store: usage lock %s: %w", provider, err)
	}
	if err := fn(Queries{db: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: usage lock %s: commit: %w", provider, err)
	}
	return nil
}

// AICallsOn sums the calls recorded for provider and purpose on day's calendar date, read in
// day's own location (the caller passes local midnight in LEADHOUND_TZ). An empty purpose
// sums every purpose.
func (q Queries) AICallsOn(ctx context.Context, provider, purpose string, day time.Time) (int, error) {
	var n int
	err := q.db.QueryRow(ctx,
		`SELECT COALESCE(sum(calls), 0)::int FROM ai_usage
		 WHERE provider = $1 AND day = $2::date AND ($3::text = '' OR purpose = $3::text)`,
		provider, day.Format(time.DateOnly), purpose).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: ai calls %s %q on %s: %w", provider, purpose, day.Format(time.DateOnly), err)
	}
	return n, nil
}

// RecordAIUsage inserts one ai_usage row; an empty purpose is recorded as jobs.
func (q Queries) RecordAIUsage(ctx context.Context, r ai.UsageRow) error {
	purpose := r.Purpose
	if purpose == "" {
		purpose = ai.PurposeJobs
	}
	_, err := q.db.Exec(ctx,
		`INSERT INTO ai_usage (provider, model, day, calls, input_tokens, purpose) VALUES ($1, $2, $3::date, $4, $5, $6)`,
		r.Provider, r.Model, r.Day.Format(time.DateOnly), r.Calls, r.InputTokens, purpose)
	if err != nil {
		return fmt.Errorf("store: record ai usage %s %s %s: %w", r.Provider, r.Model, purpose, err)
	}
	return nil
}
