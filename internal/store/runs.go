package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Run is one run row: one command invocation.
type Run struct {
	ID         int64      `db:"id"`
	Command    string     `db:"command"`
	Source     string     `db:"source"`
	StartedAt  time.Time  `db:"started_at"`
	FinishedAt *time.Time `db:"finished_at"`
	Requests   int        `db:"requests"`
	Failures   int        `db:"failures"`
	StopReason string     `db:"stop_reason"`
	Note       string     `db:"note"`
}

// RunResult is what FinishRun records. StopReason is empty for a clean finish, or one of the
// run.stop_reason values.
type RunResult struct {
	Requests   int
	Failures   int
	StopReason string
	Note       string
}

// StartRun opens a run row and returns its id.
func (q Queries) StartRun(ctx context.Context, command, source string) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO run (command, source) VALUES ($1, $2) RETURNING id`, command, source).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: start run %s %s: %w", command, source, err)
	}
	return id, nil
}

// FinishRun sets the run's finished_at, counters, stop reason and note. A missing run is
// ErrNotFound.
func (q Queries) FinishRun(ctx context.Context, id int64, r RunResult) error {
	tag, err := q.db.Exec(ctx,
		`UPDATE run SET finished_at = now(), requests = $2, failures = $3, stop_reason = $4, note = $5
		 WHERE id = $1`, id, r.Requests, r.Failures, r.StopReason, r.Note)
	if err != nil {
		return fmt.Errorf("store: finish run %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: finish run %d: %w", id, ErrNotFound)
	}
	return nil
}

// dayBounds is the calendar day of now in loc, as [start, end).
func dayBounds(loc *time.Location, now time.Time) (start, end time.Time) {
	local := now.In(loc)
	start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

// LatestRunToday returns the latest run of command for source that ended on the calendar
// day of now in loc (or, while unfinished, started on it), so a run stopped after local
// midnight counts for the new day. With stopReasons, only runs that stopped for one of them
// count. No such run is ErrNotFound.
func (q Queries) LatestRunToday(ctx context.Context, command, source string, loc *time.Location, now time.Time, stopReasons ...string) (Run, error) {
	if loc == nil {
		return Run{}, errors.New("store: latest run today: nil location")
	}
	start, end := dayBounds(loc, now)
	if stopReasons == nil {
		stopReasons = []string{}
	}
	rows, err := q.db.Query(ctx,
		`SELECT id, command, source, started_at, finished_at, requests, failures, stop_reason, note
		 FROM run
		 WHERE command = $1 AND source = $2
		   AND coalesce(finished_at, started_at) >= $3 AND coalesce(finished_at, started_at) < $4
		   AND (cardinality($5::text[]) = 0 OR stop_reason = ANY($5::text[]))
		 ORDER BY coalesce(finished_at, started_at) DESC, id DESC
		 LIMIT 1`, command, source, start, end, stopReasons)
	if err != nil {
		return Run{}, fmt.Errorf("store: latest run today %s %s: %w", command, source, err)
	}
	run, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Run])
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("store: latest run today %s %s: %w", command, source, ErrNotFound)
	}
	if err != nil {
		return Run{}, fmt.Errorf("store: latest run today %s %s: %w", command, source, err)
	}
	return run, nil
}

// PausedToday reports whether any run for source, of any command, stopped as throttled or
// blocked on the calendar day of now in loc (by when it ended, as LatestRunToday), and the
// latest such reason. Every command that fetches from source checks it before it fetches.
func (q Queries) PausedToday(ctx context.Context, source string, loc *time.Location, now time.Time) (reason string, paused bool, err error) {
	if loc == nil {
		return "", false, errors.New("store: paused today: nil location")
	}
	start, end := dayBounds(loc, now)
	err = q.db.QueryRow(ctx,
		`SELECT stop_reason FROM run
		 WHERE source = $1 AND stop_reason IN ('throttled', 'blocked')
		   AND coalesce(finished_at, started_at) >= $2 AND coalesce(finished_at, started_at) < $3
		 ORDER BY coalesce(finished_at, started_at) DESC, id DESC
		 LIMIT 1`, source, start, end).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: paused today %s: %w", source, err)
	}
	return reason, true, nil
}

// HasRunToday reports whether command ran for source on the calendar day of now in loc;
// with stopReasons, only a run that stopped for one of them counts.
func (q Queries) HasRunToday(ctx context.Context, command, source string, loc *time.Location, now time.Time, stopReasons ...string) (bool, error) {
	_, err := q.LatestRunToday(ctx, command, source, loc, now, stopReasons...)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// HasWarmRun reports whether command has ever run for source far enough to end a cold
// start: it finished, made at least one request, and stopped cleanly or only because a
// budget was spent. A throttled, blocked, failed or parser_broke run does not count.
func (q Queries) HasWarmRun(ctx context.Context, command, source string) (bool, error) {
	var ok bool
	err := q.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM run
		   WHERE command = $1 AND source = $2 AND finished_at IS NOT NULL
		     AND requests > 0 AND stop_reason IN ('', 'budget'))`,
		command, source).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("store: warm run %s %s: %w", command, source, err)
	}
	return ok, nil
}
