package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// DetailSources are the job sources whose board list carries no description, so a job is
// stored first from its board answer and its description arrives with a later detail
// fetch: SmartRecruiters, live and dry run (the ats package's source names, which a test
// there keeps in step).
var DetailSources = []string{"smartrecruiters", "smartrecruiters-fixture"}

// openForScoring is the condition a job must meet to be worth scoring once it has its
// description: an open listing whose current observation (o) is not closed and whose
// current status (s) is new or shortlisted (a job already applied for or skipped costs
// nothing).
const openForScoring = `j.listing_status = 'open' AND NOT o.closed AND coalesce(s.status, 'new') IN ('new', 'shortlisted')`

// awaitingDetail is true for a job of a DetailSources source whose current observation (o)
// was read from a board fetch: it has no description yet, and scoring it now would decide
// on its title and location alone and pay again when the detail comes.
var awaitingDetail = `(j.source IN ('` + strings.Join(DetailSources, "', '") + `')
	 AND EXISTS (SELECT 1 FROM http_fetch f WHERE f.id = o.fetch_id AND f.kind = 'board'))`

// scoreable is the condition a job must meet to be worth scoring now: openForScoring, and
// not awaiting its detail.
var scoreable = openForScoring + ` AND NOT ` + awaitingDetail

// latestObsAndStatus joins a job's current observation as o and current status as s.
const latestObsAndStatus = `
		 JOIN LATERAL (SELECT * FROM job_observation WHERE job_id = j.id
		               ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                    ORDER BY at DESC, id DESC LIMIT 1) s ON true`

// QueueEntry is one job waiting to be scored, as the light first pass reads it: no
// description, no gates.
type QueueEntry struct {
	JobID       int64
	Title       string // the current observation's title, for the title screen
	FirstSeenAt time.Time
	// Decided is true when the job has a decision on its current observation.
	Decided bool
}

// ScoreQueue is the light first pass over every job worth scoring.
type ScoreQueue struct {
	// Entries are the scoreable jobs with no failing gate on their current observation,
	// newest first (first_seen_at descending, then job id).
	Entries []QueueEntry
	// GateFailed counts the scoreable jobs left out because a gate failed: they are never
	// sent, and their descriptions are never read.
	GateFailed int
	// AwaitingDetail counts the jobs otherwise worth scoring that are held back until their
	// detail arrives (see awaitingDetail): the detail's observation gets gates of its own.
	// One whose list observation already failed a gate other than the language gate (which
	// read the title, the description being empty) is never asked for a detail (the ATS
	// collector's list-data gates), so it counts as GateFailed instead.
	AwaitingDetail int
}

// ScoreQueue reads id, title, first-seen time and whether the current observation is
// decided for every job worth scoring, about a hundred bytes a job. A job whose current
// observation has a failing gate, or that awaits its detail, is counted, not returned.
func (q Queries) ScoreQueue(ctx context.Context) (ScoreQueue, error) {
	rows, err := q.db.Query(ctx,
		`SELECT j.id, o.title, j.first_seen_at,
		        EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.verdict = 'fail'),
		        EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.verdict = 'fail' AND g.gate <> 'language'),
		        EXISTS (SELECT 1 FROM decision d WHERE d.job_id = j.id AND d.observation_id = o.id),
		        `+awaitingDetail+`
		 FROM job j`+latestObsAndStatus+`
		 WHERE `+openForScoring+`
		 ORDER BY j.first_seen_at DESC, j.id`)
	if err != nil {
		return ScoreQueue{}, fmt.Errorf("store: score queue: %w", err)
	}
	defer rows.Close()
	var out ScoreQueue
	for rows.Next() {
		var e QueueEntry
		var gateFailed, listFailed, awaiting bool
		if err := rows.Scan(&e.JobID, &e.Title, &e.FirstSeenAt, &gateFailed, &listFailed, &e.Decided, &awaiting); err != nil {
			return ScoreQueue{}, fmt.Errorf("store: score queue: %w", err)
		}
		if awaiting && !listFailed {
			out.AwaitingDetail++
			continue
		}
		if gateFailed {
			out.GateFailed++
			continue
		}
		out.Entries = append(out.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return ScoreQueue{}, fmt.Errorf("store: score queue: %w", err)
	}
	return out, nil
}

// ScoreInputsFor returns the full score input (description and gates included) of each
// job in ids that is still worth scoring, in the order of ids. A job that stopped being
// worth scoring since the queue was read, or that awaits its detail, is left out.
func (q Queries) ScoreInputsFor(ctx context.Context, ids []int64) ([]ScoreInput, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT j.id, o.id, c.name, o.title, o.location_text, o.seniority, o.employment_type,
		        o.description_clean, d.input_sha256
		 FROM job j
		 JOIN company c ON c.id = j.company_id`+latestObsAndStatus+`
		 LEFT JOIN LATERAL (SELECT input_sha256 FROM decision WHERE job_id = j.id
		                    ORDER BY created_at DESC, id DESC LIMIT 1) d ON true
		 WHERE j.id = ANY($1) AND `+scoreable, ids)
	if err != nil {
		return nil, fmt.Errorf("store: score inputs: %w", err)
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ScoreInput, error) {
		var in ScoreInput
		err := r.Scan(&in.JobID, &in.ObservationID, &in.Company, &in.Title, &in.Location, &in.Seniority,
			&in.EmploymentType, &in.Description, &in.LastInputSHA256)
		return in, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: score inputs: %w", err)
	}
	obsIDs := make([]int64, len(got))
	byJob := make(map[int64]ScoreInput, len(got))
	for i, in := range got {
		obsIDs[i] = in.ObservationID
		byJob[in.JobID] = in
	}
	gates, err := q.gateResultsFor(ctx, obsIDs)
	if err != nil {
		return nil, fmt.Errorf("store: score inputs: %w", err)
	}
	out := make([]ScoreInput, 0, len(got))
	for _, id := range ids {
		if in, ok := byJob[id]; ok {
			in.Gates = gates[in.ObservationID]
			out = append(out, in)
		}
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

// RankInput is one job with everything the ranking reads: the current observation's facts
// and gate results, the current status, and the latest decision per criterion. The ranking
// itself streams RankRows; RankInput is what the test oracle (storetest.OracleRankInputs)
// and scoring.CandidatesFrom read.
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
	// SeenAt is when the job was last observed: first seen, or observed again since.
	SeenAt    time.Time
	Gates     []GateResult
	Decisions []Decision // the latest per criterion, regardless of observation
}

// ForEachRankRow is (*Store).ForEachRankRow over q, for a caller that holds only queries:
// the pass runs in a transaction of its own (nothing is written in it) with a 10-minute
// statement timeout set for that transaction alone, so the timeout never outlives the pass
// on a pooled connection. fn's error stops the pass and is returned as it is.
func (q Queries) ForEachRankRow(ctx context.Context, opt RankStreamOptions, fn func(RankRow) error) error {
	b, ok := q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("store: rank rows: %T cannot begin a transaction", q.db)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: rank rows: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// SET takes no bind parameters; the value is an integer of milliseconds.
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(rankPassTimeout.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("store: rank rows: set statement_timeout: %w", err)
	}
	return Queries{db: tx}.forEachRankRow(ctx, opt, fn)
}
