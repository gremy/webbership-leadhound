package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DatabaseSize returns the current database's size on disk in bytes, as Postgres counts it
// (pg_database_size), for the collectors' storage budget.
func (q Queries) DatabaseSize(ctx context.Context) (int64, error) {
	var n int64
	if err := q.db.QueryRow(ctx, `SELECT pg_database_size(current_database())`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: database size: %w", err)
	}
	return n, nil
}

// TrimOptions says which postings lose their description text: rejected ones observed more
// than RejectedAfter before Now, and expired ones whose listing was last checked more than
// ExpiredAfter before Now. Batch is how many observations one TrimBatch scans. A zero Now is
// the current time.
type TrimOptions struct {
	RejectedAfter time.Duration
	ExpiredAfter  time.Duration
	Batch         int
	Now           time.Time
}

// TrimBatchResult is what one TrimBatch did. LastID is the last observation id it scanned,
// the keyset for the next batch; Done is true when there was nothing left to scan after the
// given id. An observation both rejected and expired counts as rejected.
type TrimBatchResult struct {
	LastID   int64
	Rejected int
	Expired  int
	Done     bool
}

// errTrimOptions is returned for options the trim cannot run with.
var errTrimOptions = errors.New("store: trim: batch and ages must be positive")

// TrimBatch blanks the description text of the next opt.Batch untrimmed observations after
// afterID that are old rejected or old expired postings, through leadhound_trim_batch (00007):
// the app role may run that function but the function decides what is trimmed, and a job the
// operator has touched, applied to or been briefed on is never trimmed. Every row stays.
func (q Queries) TrimBatch(ctx context.Context, opt TrimOptions, afterID int64) (TrimBatchResult, error) {
	if opt.Batch <= 0 || opt.RejectedAfter <= 0 || opt.ExpiredAfter <= 0 {
		return TrimBatchResult{}, errTrimOptions
	}
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	var last *int64
	var r TrimBatchResult
	err := q.db.QueryRow(ctx,
		`SELECT last_id, rejected, expired FROM leadhound_trim_batch($1, $2, $3, $4)`,
		now.Add(-opt.RejectedAfter), now.Add(-opt.ExpiredAfter), afterID, opt.Batch).
		Scan(&last, &r.Rejected, &r.Expired)
	if err != nil {
		return TrimBatchResult{}, fmt.Errorf("store: trim batch after %d: %w", afterID, err)
	}
	if last == nil {
		return TrimBatchResult{LastID: afterID, Done: true}, nil
	}
	r.LastID = *last
	return r, nil
}

// TrimResult is what TrimDescriptions did: the observations it trimmed, rejected and
// expired, and the batches that scanned something.
type TrimResult struct {
	Rejected int
	Expired  int
	Batches  int
}

// TrimDescriptions runs TrimBatch from the first observation until a batch scans nothing,
// calling pause (when not nil) after every batch that scanned something, so the trim yields
// between its small transactions. It stops when ctx ends, before the next batch or inside
// one, and returns what the batches so far did with ctx's error; what they trimmed stays
// trimmed, and the next run finds the rest again (the scan reads untrimmed rows only).
func (q Queries) TrimDescriptions(ctx context.Context, opt TrimOptions, pause func()) (TrimResult, error) {
	var res TrimResult
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		r, err := q.TrimBatch(ctx, opt, after)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return res, ctxErr
			}
			return res, err
		}
		if r.Done {
			return res, nil
		}
		after = r.LastID
		res.Rejected += r.Rejected
		res.Expired += r.Expired
		res.Batches++
		if pause != nil {
			pause()
		}
	}
}
