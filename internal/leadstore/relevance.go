package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// The bands a relevance answer can fall in (lead_relevance_band_check, migration 00013).
const (
	BandStrong   = "strong"
	BandLikely   = "likely"
	BandMaybe    = "maybe"
	BandUnlikely = "unlikely"
	BandNo       = "no"
)

// ErrBadEval is returned by RecordRelevanceEval for a number outside its range (a share
// outside 0..1, rho outside -1..1 or NaN, no runs).
var ErrBadEval = errors.New("leadstore: relevance eval out of range")

// RelevanceRow is one lead_relevance row to write.
type RelevanceRow struct {
	LeadID        int64
	Question      string // e.g. "relevance@1"
	Decider       string // jev | fake
	Model         string // the model the response named ("" when none)
	InputSHA256   []byte // 32 bytes
	Band          string // a Band* value
	Score         int    // 0..100
	Probabilities map[string]float64
	InputTokens   int
	RunID         int64
}

// Relevance is one stored rating of a lead.
type Relevance struct {
	ID            int64              `db:"id"`
	LeadID        int64              `db:"lead_id"`
	Question      string             `db:"question"`
	Decider       string             `db:"decider"`
	Model         string             `db:"model"`
	InputSHA256   []byte             `db:"input_sha256"`
	Band          string             `db:"band"`
	Score         int                `db:"score"`
	Probabilities map[string]float64 `db:"probabilities"`
	InputTokens   int                `db:"input_tokens"`
	RunID         int64              `db:"run_id"`
	CreatedAt     time.Time          `db:"created_at"`
}

// InsertRelevance appends r as lead r.LeadID's latest rating and returns its id.
func (q Queries) InsertRelevance(ctx context.Context, r RelevanceRow) (int64, error) {
	probs := r.Probabilities
	if probs == nil {
		probs = map[string]float64{}
	}
	b, err := json.Marshal(probs)
	if err != nil {
		return 0, fmt.Errorf("leadstore: insert relevance: %w", err)
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO lead_relevance (lead_id, question, decider, model, input_sha256, band, score, probabilities, input_tokens, run_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10) RETURNING id`,
		r.LeadID, r.Question, r.Decider, r.Model, r.InputSHA256, r.Band, r.Score, string(b), r.InputTokens, r.RunID).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: insert relevance: %w", err)
	}
	return id, nil
}

// LatestRelevance returns the latest rating of each of the leads leadIDs, by lead id; a lead
// never rated has no entry.
func (q Queries) LatestRelevance(ctx context.Context, leadIDs []int64) (map[int64]Relevance, error) {
	out := map[int64]Relevance{}
	if len(leadIDs) == 0 {
		return out, nil
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[Relevance],
		`SELECT r.id, r.lead_id, r.question, r.decider, r.model, r.input_sha256, r.band, r.score, r.probabilities,
		   r.input_tokens, r.run_id, r.created_at
		 FROM unnest($1::bigint[]) AS l(id)
		 CROSS JOIN LATERAL (
		   SELECT * FROM lead_relevance r WHERE r.lead_id = l.id ORDER BY r.id DESC LIMIT 1
		 ) r`, leadIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: latest relevance: %w", err)
	}
	for _, r := range rows {
		out[r.LeadID] = r
	}
	return out, nil
}

// RelevanceEval is one eval of a question over a labelled set.
type RelevanceEval struct {
	ID        int64     `db:"id"`
	Question  string    `db:"question"`
	Model     string    `db:"model"`
	N         int       `db:"n"`          // labelled leads
	Exact     float64   `db:"exact"`      // share whose band matched the label, 0..1
	WithinOne float64   `db:"within_one"` // share within one band of the label, 0..1
	MAE       float64   `db:"mae"`        // mean absolute error against the label weights, 0..100
	Spearman  float64   `db:"spearman"`   // Spearman's rho, the mean over the runs, -1..1
	Runs      int       `db:"runs"`
	Trusted   bool      `db:"trusted"` // the eval passed the trust gate
	At        time.Time `db:"at"`
}

// RecordRelevanceEval appends e as its question's latest eval and returns its id. e.ID and
// e.At are ignored. A number out of range is ErrBadEval.
func (q Queries) RecordRelevanceEval(ctx context.Context, e RelevanceEval) (int64, error) {
	share := func(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }
	if e.N < 0 || e.Runs < 1 || !share(e.Exact) || !share(e.WithinOne) ||
		math.IsNaN(e.MAE) || e.MAE < 0 || e.MAE > 100 || math.IsNaN(e.Spearman) || e.Spearman < -1 || e.Spearman > 1 {
		return 0, fmt.Errorf("leadstore: record relevance eval: %w", ErrBadEval)
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO lead_relevance_eval (question, model, n, exact, within_one, mae, spearman, runs, trusted)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		e.Question, e.Model, e.N, e.Exact, e.WithinOne, e.MAE, e.Spearman, e.Runs, e.Trusted).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: record relevance eval: %w", err)
	}
	return id, nil
}

// LatestRelevanceEval returns question's latest eval; ok is false when it was never
// evaluated (the number is then not trusted).
func (q Queries) LatestRelevanceEval(ctx context.Context, question string) (e RelevanceEval, ok bool, err error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[RelevanceEval],
		`SELECT id, question, model, n, exact, within_one, mae, spearman, runs, trusted, at
		 FROM lead_relevance_eval WHERE question = $1 ORDER BY id DESC LIMIT 1`, question)
	if err != nil {
		return RelevanceEval{}, false, fmt.Errorf("leadstore: latest relevance eval: %w", err)
	}
	if len(rows) == 0 {
		return RelevanceEval{}, false, nil
	}
	return rows[0], true, nil
}
