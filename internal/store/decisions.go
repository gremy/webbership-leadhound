package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// DecisionInsert is one criterion's verdict on one observation. Probabilities is stored as
// a JSON object; InputSHA256 is the hash of the exact request the decider was sent.
type DecisionInsert struct {
	JobID         int64
	ObservationID int64
	Decider       string
	Criterion     string
	Verdict       string
	Confidence    float64
	Probabilities map[string]float64
	Reason        string
	InputSHA256   []byte
}

// Decision is one decision row.
type Decision struct {
	ID            int64           `db:"id"`
	JobID         int64           `db:"job_id"`
	ObservationID int64           `db:"observation_id"`
	Decider       string          `db:"decider"`
	Criterion     string          `db:"criterion"`
	Verdict       string          `db:"verdict"`
	Confidence    float32         `db:"confidence"`
	Probabilities json.RawMessage `db:"probabilities"`
	Reason        string          `db:"reason"`
	InputSHA256   []byte          `db:"input_sha256"`
	CreatedAt     time.Time       `db:"created_at"`
}

const decisionColumns = `id, job_id, observation_id, decider, criterion, verdict, confidence, probabilities,
	reason, input_sha256, created_at`

// InsertDecisions appends every decision in one transaction of its own, so a job is never
// left with only some of its criteria decided.
func (s *Store) InsertDecisions(ctx context.Context, ds []DecisionInsert) error {
	return s.InTx(ctx, func(q Queries) error {
		for _, d := range ds {
			if _, err := q.InsertDecision(ctx, d); err != nil {
				return err
			}
		}
		return nil
	})
}

// InsertDecision appends one decision and returns its id.
func (q Queries) InsertDecision(ctx context.Context, d DecisionInsert) (int64, error) {
	probs := d.Probabilities
	if probs == nil {
		probs = map[string]float64{}
	}
	b, err := json.Marshal(probs)
	if err != nil {
		return 0, fmt.Errorf("store: decision %s for job %d: probabilities: %w", d.Criterion, d.JobID, err)
	}
	var id int64
	err = q.db.QueryRow(ctx,
		`INSERT INTO decision (job_id, observation_id, decider, criterion, verdict, confidence, probabilities, reason, input_sha256)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		d.JobID, d.ObservationID, d.Decider, d.Criterion, d.Verdict, float32(d.Confidence), b, d.Reason, d.InputSHA256).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert decision %s for job %d: %w", d.Criterion, d.JobID, err)
	}
	return id, nil
}

// LatestDecisions returns the job's current decision per criterion, whatever observation
// it was made on (an unchanged job keeps its last decisions) and whichever decider made it,
// ordered by criterion name.
func (q Queries) LatestDecisions(ctx context.Context, jobID int64) ([]Decision, error) {
	out, err := q.latestDecisions(ctx, []int64{jobID}, true)
	if err != nil {
		return nil, fmt.Errorf("store: latest decisions for job %d: %w", jobID, err)
	}
	return out, nil
}

// latestDecisions is the latest decision per (job, criterion) for every job in jobIDs,
// ordered by job then criterion. Unless allowFake, the fake decider's decisions are left
// out, so the latest real one (or none) is what is returned.
func (q Queries) latestDecisions(ctx context.Context, jobIDs []int64, allowFake bool) ([]Decision, error) {
	rows, err := q.db.Query(ctx,
		`SELECT DISTINCT ON (job_id, criterion) `+decisionColumns+`
		 FROM decision WHERE job_id = ANY($1) AND ($2 OR decider <> $3)
		 ORDER BY job_id, criterion, created_at DESC, id DESC`, jobIDs, allowFake, ai.FakeName)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[Decision])
}

// LatestInputSHA256 returns the input hash of the job's most recent decision. A job never
// decided is ErrNotFound.
func (q Queries) LatestInputSHA256(ctx context.Context, jobID int64) ([]byte, error) {
	var h []byte
	err := q.db.QueryRow(ctx,
		`SELECT input_sha256 FROM decision WHERE job_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1`,
		jobID).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: latest input hash for job %d: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: latest input hash for job %d: %w", jobID, err)
	}
	return h, nil
}

// ScoreInput is what the scorer reads about one job: its current observation, that
// observation's gate results and the input hash of its most recent decision.
type ScoreInput struct {
	JobID           int64
	ObservationID   int64
	Company         string
	Title           string
	Location        string
	Seniority       string
	EmploymentType  string
	Description     string
	Gates           []GateResult
	LastInputSHA256 []byte // nil when the job was never decided
}

// ScoreInputs returns the jobs worth scoring: open listings whose current observation is
// not closed and whose current status is new or shortlisted (a job already applied for or
// skipped costs nothing). Gate results come with them; the scorer skips a gate fail
// itself, so the rule has one home. Ordered by job id.
func (q Queries) ScoreInputs(ctx context.Context) ([]ScoreInput, error) {
	rows, err := q.db.Query(ctx,
		`SELECT j.id, o.id, c.name, o.title, o.location_text, o.seniority, o.employment_type,
		        o.description_clean, d.input_sha256
		 FROM job j
		 JOIN company c ON c.id = j.company_id
		 JOIN LATERAL (SELECT * FROM job_observation WHERE job_id = j.id
		               ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                    ORDER BY at DESC, id DESC LIMIT 1) s ON true
		 LEFT JOIN LATERAL (SELECT input_sha256 FROM decision WHERE job_id = j.id
		                    ORDER BY created_at DESC, id DESC LIMIT 1) d ON true
		 WHERE j.listing_status = 'open' AND NOT o.closed
		   AND coalesce(s.status, 'new') IN ('new', 'shortlisted')
		 ORDER BY j.id`)
	if err != nil {
		return nil, fmt.Errorf("store: score inputs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ScoreInput, error) {
		var in ScoreInput
		err := r.Scan(&in.JobID, &in.ObservationID, &in.Company, &in.Title, &in.Location, &in.Seniority,
			&in.EmploymentType, &in.Description, &in.LastInputSHA256)
		return in, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: score inputs: %w", err)
	}
	obsIDs := make([]int64, len(out))
	for i, in := range out {
		obsIDs[i] = in.ObservationID
	}
	gates, err := q.gateResultsFor(ctx, obsIDs)
	if err != nil {
		return nil, fmt.Errorf("store: score inputs: %w", err)
	}
	for i := range out {
		out[i].Gates = gates[out[i].ObservationID]
	}
	return out, nil
}

// gateResultsFor returns the gate results of each observation, by observation id, in the
// order they were written.
func (q Queries) gateResultsFor(ctx context.Context, obsIDs []int64) (map[int64][]GateResult, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, job_id, observation_id, gate, verdict, reason, snippet, rule_id, derived_by, created_at
		 FROM gate_result WHERE observation_id = ANY($1) ORDER BY id`, obsIDs)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[GateResult])
	if err != nil {
		return nil, err
	}
	m := make(map[int64][]GateResult, len(obsIDs))
	for _, g := range list {
		m[g.ObservationID] = append(m[g.ObservationID], g)
	}
	return m, nil
}

// RankInput is one job as the ranking reads it: the current observation's facts and
// gate results, the current status, and the latest decision per criterion.
type RankInput struct {
	JobID          int64
	ListingStatus  string
	Status         string // the latest status_event; "new" when there is none
	Company        string
	Title          string
	Location       string
	Description    string // read for skip rules added since the observation
	Closed         bool
	PostedAtEst    *time.Time
	ApplicantCount *int
	// ApplyType is the effective apply type. For now it is the observation's apply_type;
	// CP6b adds the join to the latest apply_route, which makes it "ats" when that route's
	// kind is employer_job.
	ApplyType string
	Gates     []GateResult
	Decisions []Decision // the latest per criterion, regardless of observation
}

// RankOptions choose what RankInputs reads.
type RankOptions struct {
	// AllowFake reads the fake decider's decisions too. Off, they are ignored, so a job
	// only the fake decided has no decisions and ranks as unscored, never as Apply.
	AllowFake bool
}

// RankInputs returns every job that has an observation, ordered by job id, for the pure
// ranking to sort and exclude. No job is filtered here, so the brief can count what was
// skipped and why; only the fake decider's decisions are, unless opt.AllowFake.
func (q Queries) RankInputs(ctx context.Context, opt RankOptions) ([]RankInput, error) {
	rows, err := q.db.Query(ctx,
		`SELECT j.id, j.listing_status, coalesce(s.status, 'new'), c.name, o.title, o.location_text,
		        o.description_clean, o.closed, o.posted_at_est, o.applicant_count, o.apply_type, o.id
		 FROM job j
		 JOIN company c ON c.id = j.company_id
		 JOIN LATERAL (SELECT * FROM job_observation WHERE job_id = j.id
		               ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                    ORDER BY at DESC, id DESC LIMIT 1) s ON true
		 ORDER BY j.id`)
	if err != nil {
		return nil, fmt.Errorf("store: rank inputs: %w", err)
	}
	var obsIDs []int64
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RankInput, error) {
		var in RankInput
		var obsID int64
		err := r.Scan(&in.JobID, &in.ListingStatus, &in.Status, &in.Company, &in.Title, &in.Location,
			&in.Description, &in.Closed, &in.PostedAtEst, &in.ApplicantCount, &in.ApplyType, &obsID)
		obsIDs = append(obsIDs, obsID)
		return in, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: rank inputs: %w", err)
	}
	gates, err := q.gateResultsFor(ctx, obsIDs)
	if err != nil {
		return nil, fmt.Errorf("store: rank inputs: %w", err)
	}
	jobIDs := make([]int64, len(out))
	for i := range out {
		out[i].Gates = gates[obsIDs[i]]
		jobIDs[i] = out[i].JobID
	}
	decisions, err := q.latestDecisions(ctx, jobIDs, opt.AllowFake)
	if err != nil {
		return nil, fmt.Errorf("store: rank inputs: %w", err)
	}
	byJob := make(map[int64][]Decision, len(out))
	for _, d := range decisions {
		byJob[d.JobID] = append(byJob[d.JobID], d)
	}
	for i := range out {
		out[i].Decisions = byJob[out[i].JobID]
	}
	return out, nil
}
