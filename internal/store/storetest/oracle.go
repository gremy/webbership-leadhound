package storetest

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// OracleRankInputs is the ranking read as it was before the streamed pass: every job with an
// observation, ordered by job id, with its current observation's gate results and its latest
// decision per criterion (the fake decider's only when allowFake). Its SQL is the old
// store.RankInputs and its two follow-up queries, copied verbatim, so a test can hold the
// streamed pass to exactly what the old read returned.
//
// It reads every job and holds them all in memory: a test oracle only, never for production.
func OracleRankInputs(ctx context.Context, s *store.Store, allowFake bool) ([]store.RankInput, error) {
	db := s.Pool()
	rows, err := db.Query(ctx,
		`SELECT j.id, j.listing_status, coalesce(s.status, 'new'), c.name, o.title, o.location_text,
		        o.description_clean, o.closed, o.posted_at_est, o.applicant_count, o.apply_type,
		        greatest(j.first_seen_at, o.observed_at), o.id
		 FROM job j
		 JOIN company c ON c.id = j.company_id
		 JOIN LATERAL (SELECT * FROM job_observation WHERE job_id = j.id
		               ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                    ORDER BY at DESC, id DESC LIMIT 1) s ON true
		 ORDER BY j.id`)
	if err != nil {
		return nil, fmt.Errorf("storetest: oracle rank inputs: %w", err)
	}
	var obsIDs []int64
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (store.RankInput, error) {
		var in store.RankInput
		var obsID int64
		err := r.Scan(&in.JobID, &in.ListingStatus, &in.Status, &in.Company, &in.Title, &in.Location,
			&in.Description, &in.Closed, &in.PostedAtEst, &in.ApplicantCount, &in.ApplyType, &in.SeenAt, &obsID)
		obsIDs = append(obsIDs, obsID)
		return in, err
	})
	if err != nil {
		return nil, fmt.Errorf("storetest: oracle rank inputs: %w", err)
	}

	// The gate results of each current observation, in the order they were written.
	rows, err = db.Query(ctx,
		`SELECT id, job_id, observation_id, gate, verdict, reason, snippet, rule_id, derived_by, created_at
		 FROM gate_result WHERE observation_id = ANY($1) ORDER BY id`, obsIDs)
	if err != nil {
		return nil, fmt.Errorf("storetest: oracle rank inputs: %w", err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.GateResult])
	if err != nil {
		return nil, fmt.Errorf("storetest: oracle rank inputs: %w", err)
	}
	gates := make(map[int64][]store.GateResult, len(obsIDs))
	for _, g := range list {
		gates[g.ObservationID] = append(gates[g.ObservationID], g)
	}
	jobIDs := make([]int64, len(out))
	for i := range out {
		out[i].Gates = gates[obsIDs[i]]
		jobIDs[i] = out[i].JobID
	}

	// The latest decision per (job, criterion), the fake decider's left out unless allowFake.
	rows, err = db.Query(ctx,
		`SELECT DISTINCT ON (job_id, criterion) id, job_id, observation_id, decider, criterion, verdict, confidence, probabilities,
		 reason, input_sha256, created_at
		 FROM decision WHERE job_id = ANY($1) AND ($2 OR decider <> $3)
		 ORDER BY job_id, criterion, created_at DESC, id DESC`, jobIDs, allowFake, ai.FakeName)
	if err != nil {
		return nil, fmt.Errorf("storetest: oracle rank inputs: %w", err)
	}
	decisions, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Decision])
	if err != nil {
		return nil, fmt.Errorf("storetest: oracle rank inputs: %w", err)
	}
	byJob := make(map[int64][]store.Decision, len(out))
	for _, d := range decisions {
		byJob[d.JobID] = append(byJob[d.JobID], d)
	}
	for i := range out {
		out[i].Decisions = byJob[out[i].JobID]
	}
	return out, nil
}
