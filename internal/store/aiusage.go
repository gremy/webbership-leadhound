package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
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

// Queries is the usage store claude.Capped counts dollars against, and its lock.
var (
	_ claude.UsageStore  = Queries{}
	_ claude.UsageLocker = Queries{}
)

// WithClaudeUsageLock is WithUsageLock for provider anthropic: fn reads, calls and records
// under the same advisory lock every other Claude caller takes, with a store bound to the
// lock's transaction.
func (q Queries) WithClaudeUsageLock(ctx context.Context, fn func(claude.UsageStore) error) error {
	return q.WithUsageLock(ctx, claude.Provider, func(st ai.UsageStore) error {
		return fn(st.(Queries))
	})
}

// ClaudeSpendOn sums the cost in micro-dollars recorded for Claude (provider anthropic only)
// by accountID on purpose on day's calendar date, read in day's own location as AICallsOn
// reads it. An empty purpose sums every purpose. Jev rows never count.
func (q Queries) ClaudeSpendOn(ctx context.Context, accountID int64, purpose string, day time.Time) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx,
		`SELECT COALESCE(sum(cost_micros), 0)::bigint FROM ai_usage
		 WHERE provider = $1 AND account_id = $2 AND day = $3::date AND ($4::text = '' OR purpose = $4::text)`,
		claude.Provider, accountID, day.Format(time.DateOnly), purpose).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: claude spend account %d %q on %s: %w", accountID, purpose, day.Format(time.DateOnly), err)
	}
	return n, nil
}

// ClaudeSpendForLead sums the cost in micro-dollars Claude has ever cost on leadID for
// purpose; an empty purpose sums every purpose.
func (q Queries) ClaudeSpendForLead(ctx context.Context, leadID int64, purpose string) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx,
		`SELECT COALESCE(sum(cost_micros), 0)::bigint FROM ai_usage
		 WHERE provider = $1 AND lead_id = $2 AND ($3::text = '' OR purpose = $3::text)`,
		claude.Provider, leadID, purpose).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: claude spend lead %d %q: %w", leadID, purpose, err)
	}
	return n, nil
}

// RecordClaudeUsage inserts one ai_usage row for one billed Claude call (calls = 1, provider
// anthropic). A LeadID of 0 stores no lead. An account below 1 or a negative cost is refused
// before the insert, so a call is never recorded against nobody or for less than nothing.
func (q Queries) RecordClaudeUsage(ctx context.Context, r claude.UsageRow) error {
	if r.AccountID < 1 {
		return fmt.Errorf("store: record claude usage %s %s: account %d", r.Model, r.Purpose, r.AccountID)
	}
	if r.CostMicros < 0 {
		return fmt.Errorf("store: record claude usage %s %s: negative cost %d", r.Model, r.Purpose, r.CostMicros)
	}
	var lead *int64
	if r.LeadID != 0 {
		lead = &r.LeadID
	}
	_, err := q.db.Exec(ctx,
		`INSERT INTO ai_usage (provider, model, day, calls, purpose, account_id, lead_id, batch,
		   input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, web_searches, cost_micros)
		 VALUES ($1, $2, $3::date, 1, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		claude.Provider, r.Model, r.Day.Format(time.DateOnly), r.Purpose, r.AccountID, lead, r.Batch,
		r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens, r.WebSearches, r.CostMicros)
	if err != nil {
		return fmt.Errorf("store: record claude usage %s %s lead %d: %w", r.Model, r.Purpose, r.LeadID, err)
	}
	return nil
}
