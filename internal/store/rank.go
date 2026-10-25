package store

import (
	"context"
	"fmt"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// rankPassTimeout bounds one streamed ranking pass: it reads every job, so it runs past the
// app role's default statement timeout on a production-sized store.
const rankPassTimeout = 10 * time.Minute

// DescMode chooses which rows of the streamed ranking pass carry their description.
type DescMode int

const (
	// DescNone reads no description: no active skip rule reads one.
	DescNone DescMode = iota
	// DescWhereRanked reads the description of the rows seen in [DayStart, DayEnd) or with
	// no failing gate on their current observation: the only rows where a description rule
	// can change the brief's numbers (a row with a failing gate is skipped whatever a rule
	// says, and counts only on the day it was seen).
	DescWhereRanked
	// DescAll reads every row's description, for a count that splits every skip exactly.
	DescAll
)

// GateVerdictRow is one gate result as the streamed pass reads it: gate and verdict only.
type GateVerdictRow struct {
	Gate    string
	Verdict string
}

// DecisionVerdictRow is one latest decision as the streamed pass reads it: criterion and
// verdict only.
type DecisionVerdictRow struct {
	Criterion string
	Verdict   string
}

// RankRow is one job as the streamed ranking pass reads it: everything the ranking reads
// except the gate reasons and snippets and the decision reasons, which RankDetails reads for
// the jobs the ranking keeps.
type RankRow struct {
	JobID          int64
	ObservationID  int64 // the current observation
	Source         string
	ListingStatus  string
	Status         string // the latest status_event; "new" when there is none
	Company        string
	Title          string
	Location       string
	Description    string // "" unless RankStreamOptions.DescriptionFor reads it
	ApplyType      string
	Closed         bool
	PostedAtEst    *time.Time
	ApplicantCount *int
	// SeenAt is when the job was last observed: first seen, or observed again since.
	SeenAt time.Time
	// LastSeenAt is when the job was last listed by its source (job.last_seen_at).
	LastSeenAt time.Time
	Gates      []GateVerdictRow     // the current observation's, in the order written
	Decisions  []DecisionVerdictRow // the latest per criterion, ordered by criterion
}

// RankStreamOptions choose what ForEachRankRow reads.
type RankStreamOptions struct {
	// AllowFake reads the fake decider's decisions too; otherwise the latest real decision
	// per criterion stands, even when a newer fake one exists.
	AllowFake bool
	// DescriptionFor chooses which rows carry their description (see DescMode).
	DescriptionFor DescMode
	// DayStart and DayEnd are the brief's day, [DayStart, DayEnd), for DescWhereRanked.
	DayStart, DayEnd time.Time
}

// rankRowsQuery reads every job with an observation in job id order, one row per job, so
// the caller ranks them one at a time and nothing is collected. Walking job_pkey in order
// lets Postgres stream rows as it finds them, with no sort over the whole store; each
// lateral is one index lookup per job. Gates and latest decisions come back as two
// parallel arrays each (no reasons, no snippets), and a description is read only where $1
// (the DescMode) asks for it: an unread description is never fetched from its TOAST table.
var rankRowsQuery = `SELECT j.id, o.id, j.source, j.listing_status, coalesce(s.status, 'new'), c.name, o.title,
		        o.location_text,
		        CASE WHEN $1 = 2 OR ($1 = 1 AND ((greatest(j.first_seen_at, o.observed_at) >= $2
		                                          AND greatest(j.first_seen_at, o.observed_at) < $3)
		                                         OR NOT coalesce(g.failed, false)))
		             THEN o.description_clean ELSE '' END,
		        o.apply_type, o.closed, o.posted_at_est, o.applicant_count,
		        greatest(j.first_seen_at, o.observed_at), j.last_seen_at,
		        g.gates, g.verdicts, d.criteria, d.verdicts
		 FROM job j
		 JOIN company c ON c.id = j.company_id` + latestObsAndStatus + `
		 LEFT JOIN LATERAL (SELECT array_agg(gate ORDER BY id) AS gates, array_agg(verdict ORDER BY id) AS verdicts,
		                           bool_or(verdict = 'fail') AS failed
		                    FROM gate_result WHERE observation_id = o.id) g ON true
		 LEFT JOIN LATERAL (SELECT array_agg(criterion ORDER BY criterion) AS criteria,
		                           array_agg(verdict ORDER BY criterion) AS verdicts
		                    FROM (SELECT DISTINCT ON (criterion) criterion, verdict FROM decision
		                          WHERE job_id = j.id AND ($4 OR decider <> $5)
		                          ORDER BY criterion, created_at DESC, id DESC) x) d ON true
		 ORDER BY j.id`

// ForEachRankRow streams every job with an observation in job id order, calling fn once per
// row; nothing is collected, so memory follows one row, not the store. No job is filtered:
// the ranking counts what it skips and why. The pass holds one pool connection for its
// length and runs with a 10-minute statement timeout; fn may query through the store's
// other connections. fn's error stops the pass and is returned as it is.
func (s *Store) ForEachRankRow(ctx context.Context, opt RankStreamOptions, fn func(RankRow) error) error {
	return s.WithStatementTimeout(ctx, rankPassTimeout, func(q Queries) error {
		return q.forEachRankRow(ctx, opt, fn)
	})
}

func (q Queries) forEachRankRow(ctx context.Context, opt RankStreamOptions, fn func(RankRow) error) error {
	rows, err := q.db.Query(ctx, rankRowsQuery, int(opt.DescriptionFor), opt.DayStart, opt.DayEnd,
		opt.AllowFake, ai.FakeName)
	if err != nil {
		return fmt.Errorf("store: rank rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r RankRow
		var gates, gateVerdicts, criteria, criterionVerdicts []string
		if err := rows.Scan(&r.JobID, &r.ObservationID, &r.Source, &r.ListingStatus, &r.Status, &r.Company,
			&r.Title, &r.Location, &r.Description, &r.ApplyType, &r.Closed, &r.PostedAtEst, &r.ApplicantCount,
			&r.SeenAt, &r.LastSeenAt, &gates, &gateVerdicts, &criteria, &criterionVerdicts); err != nil {
			return fmt.Errorf("store: rank rows: %w", err)
		}
		if len(gates) != len(gateVerdicts) || len(criteria) != len(criterionVerdicts) {
			return fmt.Errorf("store: rank rows: job %d: verdict arrays out of step", r.JobID)
		}
		for i := range gates {
			r.Gates = append(r.Gates, GateVerdictRow{Gate: gates[i], Verdict: gateVerdicts[i]})
		}
		for i := range criteria {
			r.Decisions = append(r.Decisions, DecisionVerdictRow{Criterion: criteria[i], Verdict: criterionVerdicts[i]})
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: rank rows: %w", err)
	}
	return nil
}

// RankDetail is what the ranking shows of a job it keeps: the current description, the
// current observation's full gate results and the latest decisions with their reasons.
type RankDetail struct {
	Description string
	Gates       []GateResult // in the order written
	Decisions   []Decision   // the latest per criterion, ordered by criterion
}

// RankDetails returns the details of the given jobs only, by job id, read as the streamed
// pass reads them (the fake decider's decisions only when allowFake). A job with no
// observation, or no such job, is absent. It reads three bounded queries, whatever the size
// of the store.
func (q Queries) RankDetails(ctx context.Context, jobIDs []int64, allowFake bool) (map[int64]RankDetail, error) {
	out := map[int64]RankDetail{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT j.id, o.id, o.description_clean
		 FROM job j
		 JOIN LATERAL (SELECT id, description_clean FROM job_observation WHERE job_id = j.id
		               ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 WHERE j.id = ANY($1)`, jobIDs)
	if err != nil {
		return nil, fmt.Errorf("store: rank details: %w", err)
	}
	obsOf := map[int64]int64{}
	var obsIDs, found []int64
	for rows.Next() {
		var jobID, obsID int64
		var desc string
		if err := rows.Scan(&jobID, &obsID, &desc); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: rank details: %w", err)
		}
		obsOf[jobID] = obsID
		obsIDs = append(obsIDs, obsID)
		found = append(found, jobID)
		out[jobID] = RankDetail{Description: desc}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: rank details: %w", err)
	}
	if len(found) == 0 {
		return out, nil
	}
	gates, err := q.gateResultsFor(ctx, obsIDs)
	if err != nil {
		return nil, fmt.Errorf("store: rank details: %w", err)
	}
	decisions, err := q.latestDecisions(ctx, found, allowFake)
	if err != nil {
		return nil, fmt.Errorf("store: rank details: %w", err)
	}
	byJob := make(map[int64][]Decision, len(found))
	for _, d := range decisions {
		byJob[d.JobID] = append(byJob[d.JobID], d)
	}
	for jobID, d := range out {
		d.Gates = gates[obsOf[jobID]]
		d.Decisions = byJob[jobID]
		out[jobID] = d
	}
	return out, nil
}
