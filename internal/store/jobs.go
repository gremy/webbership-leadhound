package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// JobUpsert is a job as a search card shows it.
type JobUpsert struct {
	Source      string
	SourceJobID string
	CompanyID   int64
	Title       string
	URL         string
}

// Job is one job row.
type Job struct {
	ID              int64      `db:"id"`
	Source          string     `db:"source"`
	SourceJobID     string     `db:"source_job_id"`
	CompanyID       int64      `db:"company_id"`
	Title           string     `db:"title"`
	URL             string     `db:"url"`
	FirstSeenAt     time.Time  `db:"first_seen_at"`
	LastSeenAt      time.Time  `db:"last_seen_at"`
	ListingStatus   string     `db:"listing_status"`
	StatusCheckedAt *time.Time `db:"status_checked_at"`
}

// UpsertJob creates the job, or, when (source, source_job_id) exists, only bumps its
// last_seen_at: the first-seen title, URL and company stay. It returns the job's id.
func (q Queries) UpsertJob(ctx context.Context, j JobUpsert) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO job (source, source_job_id, company_id, title, url) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (source, source_job_id) DO UPDATE SET last_seen_at = now()
		 RETURNING id`, j.Source, j.SourceJobID, j.CompanyID, j.Title, j.URL).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert job %s/%s: %w", j.Source, j.SourceJobID, err)
	}
	return id, nil
}

// JobBySourceID returns the job for (source, sourceJobID), or ErrNotFound.
func (q Queries) JobBySourceID(ctx context.Context, source, sourceJobID string) (Job, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, source, source_job_id, company_id, title, url, first_seen_at, last_seen_at,
		        listing_status, status_checked_at
		 FROM job WHERE source = $1 AND source_job_id = $2`, source, sourceJobID)
	if err != nil {
		return Job{}, fmt.Errorf("store: job %s/%s: %w", source, sourceJobID, err)
	}
	j, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Job])
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, fmt.Errorf("store: job %s/%s: %w", source, sourceJobID, ErrNotFound)
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: job %s/%s: %w", source, sourceJobID, err)
	}
	return j, nil
}

// JobsWithoutObservation returns the ids among sourceJobIDs that have no job_observation
// yet for source, a job row or not: the new ones. They come back in input order, once each.
func (q Queries) JobsWithoutObservation(ctx context.Context, source string, sourceJobIDs []string) ([]string, error) {
	if len(sourceJobIDs) == 0 {
		return []string{}, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT u.id FROM (
		   SELECT id, min(ord) AS ord FROM unnest($2::text[]) WITH ORDINALITY AS t(id, ord) GROUP BY id
		 ) u
		 LEFT JOIN job j ON j.source = $1 AND j.source_job_id = u.id
		 WHERE j.id IS NULL OR NOT EXISTS (SELECT 1 FROM job_observation o WHERE o.job_id = j.id)
		 ORDER BY u.ord`, source, sourceJobIDs)
	if err != nil {
		return nil, fmt.Errorf("store: jobs without observation: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: jobs without observation: %w", err)
	}
	return ids, nil
}

// JobsAwaitingDetail returns the ids among sourceJobIDs whose job of source has an
// observation and whose latest one (by observed_at, the higher id breaking a tie, as
// LatestObservation) was read from a board answer (http_fetch.kind 'board'): for an ATS
// whose list carries no description, a job still waiting for its detail. A job whose detail
// came has a latest observation read from a detail answer. They come back in input order,
// once each.
func (q Queries) JobsAwaitingDetail(ctx context.Context, source string, sourceJobIDs []string) ([]string, error) {
	if len(sourceJobIDs) == 0 {
		return []string{}, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT u.id FROM (
		   SELECT id, min(ord) AS ord FROM unnest($2::text[]) WITH ORDINALITY AS t(id, ord) GROUP BY id
		 ) u
		 JOIN job j ON j.source = $1 AND j.source_job_id = u.id
		 JOIN LATERAL (
		   SELECT o.fetch_id FROM job_observation o WHERE o.job_id = j.id
		   ORDER BY o.observed_at DESC, o.id DESC LIMIT 1
		 ) latest ON true
		 JOIN http_fetch f ON f.id = latest.fetch_id AND f.kind = 'board'
		 ORDER BY u.ord`, source, sourceJobIDs)
	if err != nil {
		return nil, fmt.Errorf("store: jobs awaiting detail: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: jobs awaiting detail: %w", err)
	}
	return ids, nil
}

// ObservationInsert is the facts one fetch gave about a job. GuardFindings nil is stored as
// {}; a nil ApplicantCount or PostedAtEst is stored as NULL (unknown).
type ObservationInsert struct {
	JobID            int64
	FetchID          int64
	DerivedBy        string
	Title            string
	LocationText     string
	DescriptionClean string
	GuardFindings    json.RawMessage
	Lang             string
	LangConfidence   float32
	Seniority        string
	EmploymentType   string
	Industries       string
	ApplicantCount   *int
	ApplicantText    string
	PostedText       string
	PostedAtEst      *time.Time
	ApplyType        string
	ApplyURL         string
	Closed           bool
}

// Observation is one job_observation row.
type Observation struct {
	ID               int64           `db:"id"`
	JobID            int64           `db:"job_id"`
	FetchID          int64           `db:"fetch_id"`
	ObservedAt       time.Time       `db:"observed_at"`
	DerivedBy        string          `db:"derived_by"`
	Title            string          `db:"title"`
	LocationText     string          `db:"location_text"`
	DescriptionClean string          `db:"description_clean"`
	GuardFindings    json.RawMessage `db:"guard_findings"`
	Lang             string          `db:"lang"`
	LangConfidence   float32         `db:"lang_confidence"`
	Seniority        string          `db:"seniority"`
	EmploymentType   string          `db:"employment_type"`
	Industries       string          `db:"industries"`
	ApplicantCount   *int            `db:"applicant_count"`
	ApplicantText    string          `db:"applicant_text"`
	PostedText       string          `db:"posted_text"`
	PostedAtEst      *time.Time      `db:"posted_at_est"`
	ApplyType        string          `db:"apply_type"`
	ApplyURL         string          `db:"apply_url"`
	Closed           bool            `db:"closed"`
}

// InsertObservation appends a job_observation and returns its id.
func (q Queries) InsertObservation(ctx context.Context, o ObservationInsert) (int64, error) {
	findings := o.GuardFindings
	if len(findings) == 0 {
		findings = json.RawMessage(`{}`)
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO job_observation (job_id, fetch_id, derived_by, title, location_text, description_clean,
		   guard_findings, lang, lang_confidence, seniority, employment_type, industries, applicant_count,
		   applicant_text, posted_text, posted_at_est, apply_type, apply_url, closed)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		 RETURNING id`,
		o.JobID, o.FetchID, o.DerivedBy, o.Title, o.LocationText, o.DescriptionClean,
		findings, o.Lang, o.LangConfidence, o.Seniority, o.EmploymentType, o.Industries, o.ApplicantCount,
		o.ApplicantText, o.PostedText, o.PostedAtEst, o.ApplyType, o.ApplyURL, o.Closed).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert observation for job %d: %w", o.JobID, err)
	}
	return id, nil
}

// LatestObservation returns the job's current facts: its latest observation by observed_at,
// the higher id breaking a tie. None is ErrNotFound.
func (q Queries) LatestObservation(ctx context.Context, jobID int64) (Observation, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, job_id, fetch_id, observed_at, derived_by, title, location_text, description_clean,
		        guard_findings, lang, lang_confidence, seniority, employment_type, industries, applicant_count,
		        applicant_text, posted_text, posted_at_est, apply_type, apply_url, closed
		 FROM job_observation WHERE job_id = $1
		 ORDER BY observed_at DESC, id DESC LIMIT 1`, jobID)
	if err != nil {
		return Observation{}, fmt.Errorf("store: latest observation for job %d: %w", jobID, err)
	}
	o, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Observation])
	if errors.Is(err, pgx.ErrNoRows) {
		return Observation{}, fmt.Errorf("store: latest observation for job %d: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return Observation{}, fmt.Errorf("store: latest observation for job %d: %w", jobID, err)
	}
	return o, nil
}

// StatusEvent is one status_event row.
type StatusEvent struct {
	ID     int64     `db:"id"`
	JobID  int64     `db:"job_id"`
	Status string    `db:"status"`
	Reason string    `db:"reason"`
	RuleID *int64    `db:"rule_id"`
	Actor  string    `db:"actor"`
	At     time.Time `db:"at"`
}

// InsertStatusEvent appends a status_event and returns its id. ruleID is nil unless a stored
// rule decided it.
func (q Queries) InsertStatusEvent(ctx context.Context, jobID int64, status, reason, actor string, ruleID *int64) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO status_event (job_id, status, reason, actor, rule_id) VALUES ($1, $2, $3, $4, $5)
		 RETURNING id`, jobID, status, reason, actor, ruleID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert status %s for job %d: %w", status, jobID, err)
	}
	return id, nil
}

// LatestStatus returns the job's current status: its latest status_event by at, the higher
// id breaking a tie. None is ErrNotFound.
func (q Queries) LatestStatus(ctx context.Context, jobID int64) (StatusEvent, error) {
	rows, err := q.db.Query(ctx,
		`SELECT id, job_id, status, reason, rule_id, actor, at
		 FROM status_event WHERE job_id = $1
		 ORDER BY at DESC, id DESC LIMIT 1`, jobID)
	if err != nil {
		return StatusEvent{}, fmt.Errorf("store: latest status for job %d: %w", jobID, err)
	}
	e, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[StatusEvent])
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusEvent{}, fmt.Errorf("store: latest status for job %d: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return StatusEvent{}, fmt.Errorf("store: latest status for job %d: %w", jobID, err)
	}
	return e, nil
}
