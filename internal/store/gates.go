package store

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// GateResultInsert is one gate verdict on one observation. RuleID is set when a stored rule
// decided it.
type GateResultInsert struct {
	JobID         int64
	ObservationID int64
	Gate          string
	Verdict       string
	Reason        string
	Snippet       string // the posting's words the verdict rests on; cut to maxSnippet runes
	RuleID        *int64
	DerivedBy     string
}

// maxSnippet is the longest snippet stored, in runes. The gates already keep to it; the
// store cuts again so no caller can write a longer excerpt of a posting.
const maxSnippet = 120

// GateResult is one gate_result row.
type GateResult struct {
	ID            int64     `db:"id"`
	JobID         int64     `db:"job_id"`
	ObservationID int64     `db:"observation_id"`
	Gate          string    `db:"gate"`
	Verdict       string    `db:"verdict"`
	Reason        string    `db:"reason"`
	Snippet       string    `db:"snippet"`
	RuleID        *int64    `db:"rule_id"`
	DerivedBy     string    `db:"derived_by"`
	CreatedAt     time.Time `db:"created_at"`
}

// InsertGateResult appends a gate_result and returns its id. pipeline.Observe is its one
// caller, inside the transaction that writes the observation.
func (q Queries) InsertGateResult(ctx context.Context, g GateResultInsert) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO gate_result (job_id, observation_id, gate, verdict, reason, snippet, rule_id, derived_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		g.JobID, g.ObservationID, g.Gate, g.Verdict, g.Reason, firstRunes(g.Snippet, maxSnippet), g.RuleID, g.DerivedBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert %s gate result for job %d: %w", g.Gate, g.JobID, err)
	}
	return id, nil
}

// LatestGateResults returns the gate results of the job's current (latest) observation, in
// the order they were written. A job with no observation has none.
func (q Queries) LatestGateResults(ctx context.Context, jobID int64) ([]GateResult, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, job_id, observation_id, gate, verdict, reason, snippet, rule_id, derived_by, created_at
		 FROM gate_result
		 WHERE observation_id = (
		   SELECT id FROM job_observation WHERE job_id = $1 ORDER BY observed_at DESC, id DESC LIMIT 1)
		 ORDER BY id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("store: latest gate results for job %d: %w", jobID, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[GateResult])
	if err != nil {
		return nil, fmt.Errorf("store: latest gate results for job %d: %w", jobID, err)
	}
	return out, nil
}

// firstRunes returns s cut to at most n runes, never splitting one.
func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
