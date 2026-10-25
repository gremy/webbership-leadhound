package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Job statuses, as status_event.status stores them.
var JobStatuses = []string{"new", "shortlisted", "applied", "interviewing", "offer", "rejected", "skipped", "withdrawn"}

// Listing states, as job.listing_status stores them.
const (
	ListingOpen        = "open"
	ListingExpired     = "expired"
	ListingUnreachable = "unreachable"
)

// ErrBadStatus is returned for a status or listing state the schema does not allow.
var ErrBadStatus = errors.New("store: unknown status")

// SetListingStatus sets the job's listing_status and stamps status_checked_at. An unknown
// job is ErrNotFound.
func (q Queries) SetListingStatus(ctx context.Context, jobID int64, status string) error {
	if !slices.Contains([]string{ListingOpen, ListingExpired, ListingUnreachable}, status) {
		return fmt.Errorf("store: set listing status %q for job %d: %w", status, jobID, ErrBadStatus)
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE job SET listing_status = $2, status_checked_at = now() WHERE id = $1`, jobID, status)
	if err != nil {
		return fmt.Errorf("store: set listing status for job %d: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: set listing status for job %d: %w", jobID, ErrNotFound)
	}
	return nil
}

// RecheckTarget is one job due a recheck.
type RecheckTarget struct {
	JobID         int64
	SourceJobID   string
	Company       string
	ListingStatus string
}

// RecheckTargets returns the jobs among jobIDs, from source, that are due a recheck, in
// the order of jobIDs: an unreachable listing always, and an open one whose last check is
// older than staleBefore. The last check is the later of its current observation's fetch
// and status_checked_at. An expired listing is never due.
func (q Queries) RecheckTargets(ctx context.Context, source string, jobIDs []int64, staleBefore time.Time) ([]RecheckTarget, error) {
	if len(jobIDs) == 0 {
		return []RecheckTarget{}, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT j.id, j.source_job_id, c.name, j.listing_status
		 FROM unnest($1::bigint[]) WITH ORDINALITY AS u(id, ord)
		 JOIN job j ON j.id = u.id
		 JOIN company c ON c.id = j.company_id
		 LEFT JOIN LATERAL (SELECT f.fetched_at FROM job_observation o JOIN http_fetch f ON f.id = o.fetch_id
		                    WHERE o.job_id = j.id ORDER BY o.observed_at DESC, o.id DESC LIMIT 1) lf ON true
		 WHERE j.source = $2
		   AND (j.listing_status = 'unreachable'
		        OR (j.listing_status = 'open'
		            AND coalesce(greatest(lf.fetched_at, j.status_checked_at), '-infinity') < $3))
		 ORDER BY u.ord`, jobIDs, source, staleBefore)
	if err != nil {
		return nil, fmt.Errorf("store: recheck targets: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RecheckTarget, error) {
		var t RecheckTarget
		err := r.Scan(&t.JobID, &t.SourceJobID, &t.Company, &t.ListingStatus)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: recheck targets: %w", err)
	}
	return out, nil
}

// CurrentStatus is the job's latest status, or "new" when it has no status event yet.
func (q Queries) CurrentStatus(ctx context.Context, jobID int64) (string, error) {
	e, err := q.LatestStatus(ctx, jobID)
	if errors.Is(err, ErrNotFound) {
		return "new", nil
	}
	if err != nil {
		return "", err
	}
	return e.Status, nil
}

// ShortlistNew writes status_event shortlisted/system with reason for each job in jobIDs
// whose current status is new, and returns how many it wrote. Any other status is left
// as it is.
func (q Queries) ShortlistNew(ctx context.Context, jobIDs []int64, reason string) (int, error) {
	if len(jobIDs) == 0 {
		return 0, nil
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO status_event (job_id, status, reason, actor)
		 SELECT j.id, 'shortlisted', $2, 'system' FROM job j
		 LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                    ORDER BY at DESC, id DESC LIMIT 1) s ON true
		 WHERE j.id = ANY($1) AND coalesce(s.status, 'new') = 'new'
		 ORDER BY j.id`, jobIDs, reason)
	if err != nil {
		return 0, fmt.Errorf("store: shortlist: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// JobSummary is a job as the tracker names it.
type JobSummary struct {
	JobID         int64
	Title         string // the current observation's title, else the card's
	Company       string
	Status        string // the current status; "new" when there is none
	ListingStatus string
	URL           string
}

// JobSummaries returns each known job in jobIDs, keyed by id; unknown ids are absent.
func (q Queries) JobSummaries(ctx context.Context, jobIDs []int64) (map[int64]JobSummary, error) {
	rows, err := q.db.Query(ctx,
		`SELECT j.id, coalesce(o.title, j.title), c.name, coalesce(s.status, 'new'), j.listing_status, j.url
		 FROM job j
		 JOIN company c ON c.id = j.company_id
		 LEFT JOIN LATERAL (SELECT title FROM job_observation WHERE job_id = j.id
		                    ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                    ORDER BY at DESC, id DESC LIMIT 1) s ON true
		 WHERE j.id = ANY($1)`, jobIDs)
	if err != nil {
		return nil, fmt.Errorf("store: job summaries: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (JobSummary, error) {
		var s JobSummary
		err := r.Scan(&s.JobID, &s.Title, &s.Company, &s.Status, &s.ListingStatus, &s.URL)
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: job summaries: %w", err)
	}
	out := make(map[int64]JobSummary, len(list))
	for _, s := range list {
		out[s.JobID] = s
	}
	return out, nil
}

// JobSummaryByID is JobSummaries for one job; an unknown id is ErrNotFound.
func (q Queries) JobSummaryByID(ctx context.Context, jobID int64) (JobSummary, error) {
	m, err := q.JobSummaries(ctx, []int64{jobID})
	if err != nil {
		return JobSummary{}, err
	}
	s, ok := m[jobID]
	if !ok {
		return JobSummary{}, fmt.Errorf("store: job %d: %w", jobID, ErrNotFound)
	}
	return s, nil
}

// TrackRow is one line of the tracker: a job and its current status.
type TrackRow struct {
	JobID   int64
	Title   string
	Company string
	Status  string
	Since   time.Time
	Reason  string
}

// TrackList returns the jobs with a status event and their current status (the latest event
// over all of a job's events), latest change first, with total, how many jobs match before
// the cap. status "" leaves out the jobs still new, "all" keeps every one, and any status in
// JobStatuses keeps only that one. limit caps the rows returned; 0 returns every one.
//
// Every job gets its new event when it is first stored, so "all" and "new" match nearly
// every job: the console caps them. The other filters match only jobs that have an event
// other than new, which status_event_active_idx finds without reading the new events of
// every other job.
func (q Queries) TrackList(ctx context.Context, status string, limit int) ([]TrackRow, int, error) {
	if status != "" && status != "all" && !slices.Contains(JobStatuses, status) {
		return nil, 0, fmt.Errorf("store: track list %q: %w", status, ErrBadStatus)
	}
	// candidates narrows the jobs whose latest event is read; it never decides the status.
	candidates := `true`
	if status != "all" && status != "new" {
		candidates = `e.job_id IN (SELECT job_id FROM status_event WHERE status <> 'new')`
	}
	var capped any // NULL is LIMIT ALL
	if limit > 0 {
		capped = limit
	}
	rows, err := q.db.Query(ctx,
		`WITH latest AS (
		   SELECT DISTINCT ON (e.job_id) e.job_id, e.status, e.at, e.reason
		   FROM status_event e
		   WHERE `+candidates+`
		   ORDER BY e.job_id, e.at DESC, e.id DESC
		 ), picked AS (
		   SELECT job_id, status, at, reason, count(*) OVER () AS total
		   FROM latest
		   WHERE ($1 = 'all') OR ($1 = '' AND status <> 'new') OR status = $1
		   ORDER BY at DESC, job_id
		   LIMIT $2
		 )
		 SELECT p.job_id, coalesce(o.title, j.title), c.name, p.status, p.at, p.reason, p.total
		 FROM picked p
		 JOIN job j ON j.id = p.job_id
		 JOIN company c ON c.id = j.company_id
		 LEFT JOIN LATERAL (SELECT title FROM job_observation WHERE job_id = j.id
		                    ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 ORDER BY p.at DESC, p.job_id`, status, capped)
	if err != nil {
		return nil, 0, fmt.Errorf("store: track list: %w", err)
	}
	total := 0
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (TrackRow, error) {
		var t TrackRow
		err := r.Scan(&t.JobID, &t.Title, &t.Company, &t.Status, &t.Since, &t.Reason, &total)
		return t, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("store: track list: %w", err)
	}
	return out, total, nil
}

// CountJobsFirstSeen counts the jobs from source first seen on the calendar day of day in
// loc.
func (q Queries) CountJobsFirstSeen(ctx context.Context, source string, loc *time.Location, day time.Time) (int, error) {
	if loc == nil {
		return 0, errors.New("store: count jobs first seen: nil location")
	}
	start, end := dayBounds(loc, day)
	var n int
	err := q.db.QueryRow(ctx,
		`SELECT count(*) FROM job WHERE source = $1 AND first_seen_at >= $2 AND first_seen_at < $3`,
		source, start, end).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count jobs first seen %s: %w", source, err)
	}
	return n, nil
}
