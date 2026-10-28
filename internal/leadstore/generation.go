package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// Grounded Claude drafting (CP4, migration 00019): every attempt at writing a lead's
// sequence (draft_generation), the operator's verdict on a sampled draft (draft_review) and
// the recorded evals (draft_eval) the trust gate (internal/outreach/gate.go) reads. All
// three tables are append-only. The store never calls Claude.

// Why a draft was generated (draft_generation_purpose_check).
const (
	PurposeBatch  = "batch"
	PurposeSample = "sample"
	PurposeEval   = "eval"
)

// A review's verdict (draft_review_verdict_check).
const (
	VerdictApprove = "approve"
	VerdictReject  = "reject"
)

// ReviewReasons is the closed set a reject names at least one of, in the console's order
// (draft_review_reasons_check).
var ReviewReasons = []string{"wrong_tone", "too_long", "unclear", "factual", "other"}

// ReasonOther is the reason that needs a note.
const ReasonOther = "other"

// MaxReviewNote is the longest review note, in characters.
const MaxReviewNote = 500

// Eval deciders (draft_eval_decider_check).
const (
	DeciderClaude = "claude"
	DeciderFake   = "fake"
)

// MinEvalReviewed is how many reviewed leads an eval needs before it is recorded (the gate's
// floor; plan CP4: "Recorded in draft_eval only with --decider claude and at least 20
// reviewed leads").
const MinEvalReviewed = 20

var (
	// ErrBadGeneration is returned by RecordGeneration for a row the table would refuse.
	ErrBadGeneration = errors.New("leadstore: bad draft generation")
	// ErrGenerationNotFound is returned for a generation id that does not exist.
	ErrGenerationNotFound = errors.New("leadstore: draft generation not found")
	// ErrReviewRefused is returned by RecordReview for a generation that failed the
	// grounding check: only a draft that could be sent may be reviewed.
	ErrReviewRefused = errors.New("leadstore: a draft refused by the check cannot be reviewed")
	// ErrBadReview is returned by RecordReview for an unknown verdict or reason, a reject
	// with no reason, an approve with one, "other" with no note, or a note too long.
	ErrBadReview = errors.New("leadstore: bad draft review")
	// ErrBadDraftEval is returned by RecordEval for a number out of range or a decider
	// other than claude.
	ErrBadDraftEval = errors.New("leadstore: bad draft eval")
	// ErrEvalTooFew is returned by RecordEval when fewer than MinEvalReviewed leads were
	// reviewed; the error is an *EvalTooFewError.
	ErrEvalTooFew = errors.New("leadstore: too few reviewed leads to record an eval")
)

// EvalTooFewError is ErrEvalTooFew with the count.
type EvalTooFewError struct {
	Reviewed int
}

func (e *EvalTooFewError) Error() string {
	return fmt.Sprintf("%s: %d of %d reviewed", ErrEvalTooFew, e.Reviewed, MinEvalReviewed)
}

// Is makes errors.Is(err, ErrEvalTooFew) true.
func (e *EvalTooFewError) Is(target error) bool { return target == ErrEvalTooFew }

// GenerationRow is what RecordGeneration stores. The fact hash is computed from Facts, so it
// always matches the sheet stored beside it.
type GenerationRow struct {
	LeadID     int64
	CampaignID int64
	Attempt    int // 1, or 2 for the one retry
	Model      string
	Facts      outreach.FactSheet
	// Output is the model's answer as it came; stored as JSON when it is valid JSON, else
	// NULL (and CheckOK must then be false).
	Output     []byte
	CheckOK    bool
	Failures   []outreach.Failure // empty exactly when CheckOK
	CostMicros *int64             // nil: unknown
	Purpose    string
}

// Generation is one draft_generation row.
type Generation struct {
	ID         int64
	LeadID     int64
	CampaignID int64
	Attempt    int
	Model      string
	FactSHA256 string
	Facts      outreach.FactSheet
	Output     json.RawMessage // nil: the answer was not valid JSON
	CheckOK    bool
	Failures   []outreach.Failure
	CostMicros *int64
	Purpose    string
	At         time.Time
}

// RecordGeneration appends one attempt and returns its id. A row the table would refuse is
// ErrBadGeneration.
func (q Queries) RecordGeneration(ctx context.Context, g GenerationRow) (int64, error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: record generation lead %d: %w", g.LeadID, err) }
	bad := func(format string, a ...any) error {
		return wrap(fmt.Errorf("%w: "+format, append([]any{ErrBadGeneration}, a...)...))
	}
	switch {
	case g.Attempt != 1 && g.Attempt != 2:
		return 0, bad("attempt %d (1, or 2 for the retry)", g.Attempt)
	case g.Model == "" || utf8.RuneCountInString(g.Model) > 100 || strings.ContainsAny(g.Model, "\r\n"):
		return 0, bad("model %q", g.Model)
	case g.Purpose != PurposeBatch && g.Purpose != PurposeSample && g.Purpose != PurposeEval:
		return 0, bad("purpose %q", g.Purpose)
	case g.CheckOK != (len(g.Failures) == 0):
		return 0, bad("check_ok %t with %d failures", g.CheckOK, len(g.Failures))
	case g.CostMicros != nil && *g.CostMicros < 0:
		return 0, bad("cost %d", *g.CostMicros)
	}
	var output []byte
	if json.Valid(g.Output) {
		output = g.Output
	} else if g.CheckOK {
		return 0, bad("a passing draft whose answer is not valid JSON")
	}
	facts, err := json.Marshal(g.Facts)
	if err != nil {
		return 0, wrap(err)
	}
	failures := g.Failures
	if failures == nil {
		failures = []outreach.Failure{}
	}
	fails, err := json.Marshal(failures)
	if err != nil {
		return 0, wrap(err)
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO draft_generation (lead_id, campaign_id, attempt, model, fact_sha256, facts, output,
		   check_ok, check_failures, cost_micros, purpose)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11) RETURNING id`,
		g.LeadID, g.CampaignID, g.Attempt, g.Model, g.Facts.SHA256(), facts, nullJSON(output),
		g.CheckOK, fails, g.CostMicros, g.Purpose).Scan(&id); err != nil {
		return 0, wrap(err)
	}
	return id, nil
}

// nullJSON is b as a text parameter for a jsonb column, or nil (NULL) when b is empty.
func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

// generationCols are Generation's columns over draft_generation g.
const generationCols = `g.id, g.lead_id, g.campaign_id, g.attempt, g.model, g.fact_sha256, g.facts, g.output,
	g.check_ok, g.check_failures, g.cost_micros, g.purpose, g.at`

// scanGeneration reads one row of generationCols followed by extra destinations.
func scanGeneration(row pgx.Row, extra ...any) (Generation, error) {
	var g Generation
	var facts, output, fails []byte
	dest := append([]any{&g.ID, &g.LeadID, &g.CampaignID, &g.Attempt, &g.Model, &g.FactSHA256, &facts, &output,
		&g.CheckOK, &fails, &g.CostMicros, &g.Purpose, &g.At}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Generation{}, err
	}
	if err := json.Unmarshal(facts, &g.Facts); err != nil {
		return Generation{}, fmt.Errorf("generation %d facts: %w", g.ID, err)
	}
	if err := json.Unmarshal(fails, &g.Failures); err != nil {
		return Generation{}, fmt.Errorf("generation %d failures: %w", g.ID, err)
	}
	if output != nil {
		g.Output = json.RawMessage(output)
	}
	return g, nil
}

// generations runs sql (selecting generationCols) and reads every row.
func (q Queries) generations(ctx context.Context, sql string, args ...any) ([]Generation, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Generation
	for rows.Next() {
		g, err := scanGeneration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GenerationByID returns one generation; ErrGenerationNotFound when it does not exist.
func (q Queries) GenerationByID(ctx context.Context, id int64) (Generation, error) {
	g, err := scanGeneration(q.db.QueryRow(ctx, `SELECT `+generationCols+` FROM draft_generation g WHERE g.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Generation{}, fmt.Errorf("leadstore: generation %d: %w", id, ErrGenerationNotFound)
	}
	if err != nil {
		return Generation{}, fmt.Errorf("leadstore: generation %d: %w", id, err)
	}
	return g, nil
}

// GenerationsForLead returns every attempt for lead leadID in campaign campaignID, newest
// first.
func (q Queries) GenerationsForLead(ctx context.Context, leadID, campaignID int64) ([]Generation, error) {
	out, err := q.generations(ctx, `SELECT `+generationCols+` FROM draft_generation g
		WHERE g.lead_id = $1 AND g.campaign_id = $2 ORDER BY g.id DESC`, leadID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: generations for lead %d: %w", leadID, err)
	}
	return out, nil
}

// LatestGenerationForLead returns the newest generation for lead leadID in campaign
// campaignID that passed the check on the fact sheet hashed factSHA256, whatever its
// purpose: a batch reuses it instead of asking again. ok is false when there is none (the
// facts changed, or no attempt passed).
func (q Queries) LatestGenerationForLead(ctx context.Context, leadID, campaignID int64, factSHA256 string) (Generation, bool, error) {
	g, err := scanGeneration(q.db.QueryRow(ctx, `SELECT `+generationCols+` FROM draft_generation g
		WHERE g.lead_id = $1 AND g.campaign_id = $2 AND g.fact_sha256 = $3 AND g.check_ok
		ORDER BY g.id DESC LIMIT 1`, leadID, campaignID, factSHA256))
	if errors.Is(err, pgx.ErrNoRows) {
		return Generation{}, false, nil
	}
	if err != nil {
		return Generation{}, false, fmt.Errorf("leadstore: latest generation for lead %d: %w", leadID, err)
	}
	return g, true, nil
}

// Review is one draft_review row, with the lead its generation drafted for.
type Review struct {
	ID           int64
	GenerationID int64
	LeadID       int64
	Verdict      string
	Reasons      []string
	Note         string
	Actor        string
	At           time.Time
}

// checkReview checks a review's input as draft_review's constraints do, and returns the
// reasons in the closed set's order, each once.
func checkReview(verdict string, reasons []string, note, actor string) ([]string, error) {
	bad := func(format string, a ...any) ([]string, error) {
		return nil, fmt.Errorf("%w: "+format, append([]any{ErrBadReview}, a...)...)
	}
	if actor != ActorOperator && actor != ActorSystem {
		return nil, fmt.Errorf("%w: %q", ErrUnknownActor, actor)
	}
	out := []string{}
	for _, r := range ReviewReasons {
		if slices.Contains(reasons, r) {
			out = append(out, r)
		}
	}
	for _, r := range reasons {
		if !slices.Contains(ReviewReasons, r) {
			return bad("reason %q (known: %s)", r, strings.Join(ReviewReasons, ", "))
		}
	}
	switch verdict {
	case VerdictApprove:
		if len(out) > 0 {
			return bad("an approve carries no reasons")
		}
	case VerdictReject:
		if len(out) == 0 {
			return bad("a reject needs at least one reason")
		}
	default:
		return bad("verdict %q", verdict)
	}
	if utf8.RuneCountInString(note) > MaxReviewNote {
		return bad("note is longer than %d characters", MaxReviewNote)
	}
	if slices.Contains(out, ReasonOther) && strings.TrimSpace(note) == "" {
		return bad("the reason other needs a note")
	}
	return out, nil
}

// RecordReview appends the operator's verdict on generation generationID and returns its id;
// the latest review of a generation wins. A generation that failed the grounding check is
// ErrReviewRefused, one that does not exist ErrGenerationNotFound, bad input ErrBadReview.
func (q Queries) RecordReview(ctx context.Context, generationID int64, verdict string, reasons []string, note, actor string) (int64, error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: review generation %d: %w", generationID, err) }
	reasons, err := checkReview(verdict, reasons, note, actor)
	if err != nil {
		return 0, wrap(err)
	}
	// check_ok never changes (append-only), so reading it and inserting in one statement
	// cannot race with anything.
	var id *int64
	var ok *bool
	if err := q.db.QueryRow(ctx,
		`WITH g AS (SELECT id, check_ok FROM draft_generation WHERE id = $1),
		 ins AS (INSERT INTO draft_review (generation_id, verdict, reasons, note, actor)
		   SELECT id, $2, $3, $4, $5 FROM g WHERE check_ok RETURNING id)
		 SELECT (SELECT id FROM ins), (SELECT check_ok FROM g)`,
		generationID, verdict, reasons, note, actor).Scan(&id, &ok); err != nil {
		return 0, wrap(err)
	}
	switch {
	case ok == nil:
		return 0, wrap(ErrGenerationNotFound)
	case !*ok || id == nil:
		return 0, wrap(ErrReviewRefused)
	}
	return *id, nil
}

// latestReviewSQL is the latest review per generation, r, joined to its generation g.
const latestReviewSQL = `SELECT DISTINCT ON (r.generation_id) r.id, r.generation_id, g.lead_id, r.verdict, r.reasons,
	   r.note, r.actor, r.at
	 FROM draft_review r JOIN draft_generation g ON g.id = r.generation_id`

// ReviewsForCampaign returns the latest review of each of the last n reviewed generations of
// campaign campaignID (n <= 0: all), newest review first: what the gate's "approved share
// of the last 20 reviewed drafts" counts.
func (q Queries) ReviewsForCampaign(ctx context.Context, campaignID int64, n int) ([]Review, error) {
	limit := any(nil)
	if n > 0 {
		limit = n
	}
	rows, err := q.db.Query(ctx, `SELECT * FROM (`+latestReviewSQL+`
		 WHERE g.campaign_id = $1 ORDER BY r.generation_id, r.id DESC) latest
		 ORDER BY id DESC LIMIT $2`, campaignID, limit)
	if err != nil {
		return nil, fmt.Errorf("leadstore: reviews for campaign %d: %w", campaignID, err)
	}
	out, err := pgx.CollectRows(rows, scanReview)
	if err != nil {
		return nil, fmt.Errorf("leadstore: reviews for campaign %d: %w", campaignID, err)
	}
	return out, nil
}

func scanReview(row pgx.CollectableRow) (Review, error) {
	var r Review
	err := row.Scan(&r.ID, &r.GenerationID, &r.LeadID, &r.Verdict, &r.Reasons, &r.Note, &r.Actor, &r.At)
	return r, err
}

// ReviewSummary is what the gate reads from the last reviews: how many there are and how
// many approved.
type ReviewSummary struct {
	Reviewed int
	Approved int
}

// ApprovedShare is Approved / Reviewed, 0 when nothing was reviewed.
func (s ReviewSummary) ApprovedShare() float64 {
	if s.Reviewed == 0 {
		return 0
	}
	return float64(s.Approved) / float64(s.Reviewed)
}

// SummarizeReviews counts reviews (each already the latest of its generation).
func SummarizeReviews(reviews []Review) ReviewSummary {
	s := ReviewSummary{Reviewed: len(reviews)}
	for _, r := range reviews {
		if r.Verdict == VerdictApprove {
			s.Approved++
		}
	}
	return s
}

// ReviewedLeads returns the leads of campaign campaignID with at least one reviewed
// generation, in id order: the set leads drafts eval regenerates.
func (q Queries) ReviewedLeads(ctx context.Context, campaignID int64) ([]int64, error) {
	out, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT DISTINCT g.lead_id FROM draft_review r JOIN draft_generation g ON g.id = r.generation_id
		 WHERE g.campaign_id = $1 ORDER BY g.lead_id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: reviewed leads of campaign %d: %w", campaignID, err)
	}
	return out, nil
}

// SampledGeneration is one sampled draft for the Drafts tab, with its latest review (nil:
// not reviewed yet).
type SampledGeneration struct {
	Generation
	Review *Review
}

// SampleGenerations returns campaign campaignID's sampled drafts (purpose sample), newest
// first, at most limit (limit <= 0: all), each with its latest review. Drafts refused by the
// check are included (they show why; they cannot be reviewed).
func (q Queries) SampleGenerations(ctx context.Context, campaignID int64, limit int) ([]SampledGeneration, error) {
	lim := any(nil)
	if limit > 0 {
		lim = limit
	}
	rows, err := q.db.Query(ctx, `SELECT `+generationCols+`,
		   r.id, r.verdict, r.reasons, r.note, r.actor, r.at
		 FROM draft_generation g
		 LEFT JOIN LATERAL (SELECT * FROM draft_review rr WHERE rr.generation_id = g.id ORDER BY rr.id DESC LIMIT 1) r ON true
		 WHERE g.campaign_id = $1 AND g.purpose = 'sample'
		 ORDER BY g.id DESC LIMIT $2`, campaignID, lim)
	if err != nil {
		return nil, fmt.Errorf("leadstore: sample generations of campaign %d: %w", campaignID, err)
	}
	defer rows.Close()
	var out []SampledGeneration
	for rows.Next() {
		var rid *int64
		var verdict, note, actor *string
		var reasons []string
		var at *time.Time
		g, err := scanGeneration(rows, &rid, &verdict, &reasons, &note, &actor, &at)
		if err != nil {
			return nil, fmt.Errorf("leadstore: sample generations of campaign %d: %w", campaignID, err)
		}
		s := SampledGeneration{Generation: g}
		if rid != nil {
			s.Review = &Review{ID: *rid, GenerationID: g.ID, LeadID: g.LeadID, Verdict: *verdict,
				Reasons: reasons, Note: *note, Actor: *actor, At: *at}
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leadstore: sample generations of campaign %d: %w", campaignID, err)
	}
	return out, nil
}

// DraftEvalRow is one draft_eval row: what leads drafts eval measured for a campaign.
type DraftEvalRow struct {
	ID             int64
	CampaignID     int64
	Runs           int
	Reviewed       int                // reviewed leads regenerated
	FirstTryPass   float64            // share of runs that passed on the first try, 0..1
	AfterRetryPass float64            // share that passed after the one retry, 0..1
	Failures       map[string]int     // grounding code -> count
	MeanWords      map[string]float64 // step (as text, "0") -> mean words
	Tokens         int64
	CostMicros     int64
	Decider        string
	At             time.Time
}

// RecordEval appends e as its campaign's latest eval and returns its id. e.ID and e.At are
// ignored. Fewer than MinEvalReviewed reviewed leads is an *EvalTooFewError; a decider other
// than claude or a number out of range is ErrBadDraftEval.
func (q Queries) RecordEval(ctx context.Context, e DraftEvalRow) (int64, error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: record draft eval: %w", err) }
	share := func(v float64) bool { return !math.IsNaN(v) && v >= 0 && v <= 1 }
	if e.Decider != DeciderClaude {
		return 0, wrap(fmt.Errorf("%w: decider %q (only a claude eval is recorded)", ErrBadDraftEval, e.Decider))
	}
	if e.Runs < 1 || e.Reviewed < 0 || e.Tokens < 0 || e.CostMicros < 0 || !share(e.FirstTryPass) ||
		!share(e.AfterRetryPass) || e.AfterRetryPass < e.FirstTryPass {
		return 0, wrap(ErrBadDraftEval)
	}
	if e.Reviewed < MinEvalReviewed {
		return 0, wrap(&EvalTooFewError{Reviewed: e.Reviewed})
	}
	for code, n := range e.Failures {
		if n < 0 || code == "" {
			return 0, wrap(fmt.Errorf("%w: failures %q = %d", ErrBadDraftEval, code, n))
		}
	}
	for step, w := range e.MeanWords {
		if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
			return 0, wrap(fmt.Errorf("%w: mean words of step %s", ErrBadDraftEval, step))
		}
	}
	failures, mean := e.Failures, e.MeanWords
	if failures == nil {
		failures = map[string]int{}
	}
	if mean == nil {
		mean = map[string]float64{}
	}
	fb, err := json.Marshal(failures)
	if err != nil {
		return 0, wrap(err)
	}
	mb, err := json.Marshal(mean)
	if err != nil {
		return 0, wrap(err)
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO draft_eval (campaign_id, runs, reviewed, first_try_pass, after_retry_pass, failures, mean_words,
		   tokens, cost_micros, decider)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		e.CampaignID, e.Runs, e.Reviewed, e.FirstTryPass, e.AfterRetryPass, fb, mb, e.Tokens, e.CostMicros,
		e.Decider).Scan(&id); err != nil {
		return 0, wrap(err)
	}
	return id, nil
}

// LatestEval returns campaign campaignID's latest recorded eval; ok is false when it was
// never evaluated (the gate is then closed).
func (q Queries) LatestEval(ctx context.Context, campaignID int64) (e DraftEvalRow, ok bool, err error) {
	var fb, mb []byte
	err = q.db.QueryRow(ctx,
		`SELECT id, campaign_id, runs, reviewed, first_try_pass, after_retry_pass, failures, mean_words, tokens,
		   cost_micros, decider, at
		 FROM draft_eval WHERE campaign_id = $1 ORDER BY id DESC LIMIT 1`, campaignID).Scan(
		&e.ID, &e.CampaignID, &e.Runs, &e.Reviewed, &e.FirstTryPass, &e.AfterRetryPass, &fb, &mb, &e.Tokens,
		&e.CostMicros, &e.Decider, &e.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return DraftEvalRow{}, false, nil
	}
	if err != nil {
		return DraftEvalRow{}, false, fmt.Errorf("leadstore: latest draft eval of campaign %d: %w", campaignID, err)
	}
	if err := json.Unmarshal(fb, &e.Failures); err != nil {
		return DraftEvalRow{}, false, fmt.Errorf("leadstore: latest draft eval failures: %w", err)
	}
	if err := json.Unmarshal(mb, &e.MeanWords); err != nil {
		return DraftEvalRow{}, false, fmt.Errorf("leadstore: latest draft eval mean words: %w", err)
	}
	return e, true, nil
}
