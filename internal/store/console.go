package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// LockJob takes a row lock on the job for the rest of the transaction (SELECT 1 FROM job
// WHERE id = $1 FOR UPDATE); an unknown job is ErrNotFound. Only meaningful inside InTx.
func (q Queries) LockJob(ctx context.Context, jobID int64) error {
	var one int
	err := q.db.QueryRow(ctx, `SELECT 1 FROM job WHERE id = $1 FOR UPDATE`, jobID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("store: lock job %d: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("store: lock job %d: %w", jobID, err)
	}
	return nil
}

// Bounds of RecentRuns' limit.
const (
	minRecentRuns = 1
	maxRecentRuns = 200
)

// RecentRuns returns the latest limit runs, newest first (by started_at, then id). limit is
// clamped to 1–200.
func (q Queries) RecentRuns(ctx context.Context, limit int) ([]Run, error) {
	limit = min(max(limit, minRecentRuns), maxRecentRuns)
	rows, err := q.db.Query(ctx,
		`SELECT id, command, source, started_at, finished_at, requests, failures, stop_reason, note
		 FROM run
		 ORDER BY started_at DESC, id DESC
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByName[Run])
	if err != nil {
		return nil, fmt.Errorf("store: recent runs: %w", err)
	}
	return runs, nil
}

// MaxRunsSince caps RunsSince: a night's ATS collect writes one row per ATS, so a two-day
// window stays far below it.
const MaxRunsSince = 500

// RunsSince returns the runs started at or after since, oldest first (by started_at, then
// id). Past MaxRunsSince it keeps the newest, so a cap never hides what runs now.
func (q Queries) RunsSince(ctx context.Context, since time.Time) ([]Run, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, command, source, started_at, finished_at, requests, failures, stop_reason, note
		 FROM (SELECT id, command, source, started_at, finished_at, requests, failures, stop_reason, note
		       FROM run
		       WHERE started_at >= $1
		       ORDER BY started_at DESC, id DESC
		       LIMIT $2) newest
		 ORDER BY started_at, id`, since, MaxRunsSince)
	if err != nil {
		return nil, fmt.Errorf("store: runs since %s: %w", since.Format(time.RFC3339), err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByName[Run])
	if err != nil {
		return nil, fmt.Errorf("store: runs since %s: %w", since.Format(time.RFC3339), err)
	}
	return runs, nil
}

// AIUsage is one day's Jev calls per purpose (each has its own daily cap) and the input
// tokens of both together, over every provider and model.
type AIUsage struct{ JobsCalls, LeadsCalls, InputTokens int }

// AIUsageOn sums the calls per purpose and the input tokens recorded on day's calendar
// date, read in day's own location as AICallsOn reads it (the caller passes a time in
// LEADHOUND_TZ).
func (q Queries) AIUsageOn(ctx context.Context, day time.Time) (AIUsage, error) {
	var u AIUsage
	err := q.db.QueryRow(ctx,
		`SELECT COALESCE(sum(calls) FILTER (WHERE purpose = $2), 0)::bigint,
		        COALESCE(sum(calls) FILTER (WHERE purpose = $3), 0)::bigint,
		        COALESCE(sum(input_tokens), 0)::bigint
		 FROM ai_usage WHERE day = $1::date`,
		day.Format(time.DateOnly), ai.PurposeJobs, ai.PurposeLeads).Scan(&u.JobsCalls, &u.LeadsCalls, &u.InputTokens)
	if err != nil {
		return AIUsage{}, fmt.Errorf("store: ai usage on %s: %w", day.Format(time.DateOnly), err)
	}
	return u, nil
}

// Funnel is what happened to jobs on one local day.
type Funnel struct {
	NewBySource map[string]int // jobs first seen that day, per job.source
	Gated       int            // distinct jobs with a gate_result created that day
	GateFailed  int            // of those, jobs with at least one 'fail' verdict created that day
	Scored      int            // distinct jobs with a decision created that day
}

// FunnelOn counts, for the calendar day of day in loc, the jobs first seen per source, the
// jobs gated, the jobs with a failed gate and the jobs scored. A day with nothing is zeros
// and an empty map.
func (q Queries) FunnelOn(ctx context.Context, loc *time.Location, day time.Time) (Funnel, error) {
	if loc == nil {
		return Funnel{}, errors.New("store: funnel: nil location")
	}
	start, end := dayBounds(loc, day)
	f := Funnel{NewBySource: map[string]int{}}
	err := q.db.QueryRow(ctx,
		`SELECT
		   (SELECT coalesce(jsonb_object_agg(source, n), '{}') FROM
		     (SELECT source, count(*) AS n FROM job
		      WHERE first_seen_at >= $1 AND first_seen_at < $2 GROUP BY source) s),
		   (SELECT count(DISTINCT job_id) FROM gate_result WHERE created_at >= $1 AND created_at < $2),
		   (SELECT count(DISTINCT job_id) FROM gate_result
		    WHERE verdict = 'fail' AND created_at >= $1 AND created_at < $2),
		   (SELECT count(DISTINCT job_id) FROM decision WHERE created_at >= $1 AND created_at < $2)`,
		start, end).Scan(&f.NewBySource, &f.Gated, &f.GateFailed, &f.Scored)
	if err != nil {
		return Funnel{}, fmt.Errorf("store: funnel on %s: %w", start.Format(time.DateOnly), err)
	}
	return f, nil
}
