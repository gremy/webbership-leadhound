package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// NextJob is a job from a sent brief that is still worth applying to.
type NextJob struct {
	JobID                                                                     int64
	Title, Company, Location, Status, ListingStatus, URL, ApplyURL, ApplyType string
	BriefDay                                                                  time.Time // the brief's date, at midnight UTC
	BriefN                                                                    int       // its number in that brief: BriefLog.JobIDs[BriefN-1]
	Prior                                                                     []PriorJob
}

// PriorJob is another job of the same company whose status is past new. One posting can be
// two jobs, because identity is per source, so this is what shows it was already handled.
type PriorJob struct {
	JobID         int64
	Title, Status string
	Since         time.Time // when the job took its current status
}

// NextJobs returns the jobs of the briefs sent on or after since's calendar date, one row per
// job under its newest brief, newest brief first and then in brief order, at most limit. It
// keeps only a current status of new or shortlisted, an open listing and a latest observation
// not closed; a job with no observation lists with its card's title and empty fields. Prior
// is empty, never nil, when the company has no other job past new.
//
// BriefN stays the number the brief itself gave, whichever of its jobs are left out.
func (q Queries) NextJobs(ctx context.Context, since time.Time, limit int) ([]NextJob, error) {
	// handled is materialized so that job, which has no index on company_id, is read once
	// for all the picked companies rather than once per row.
	rows, err := q.db.Query(ctx,
		`WITH briefed AS (
		   SELECT DISTINCT ON (u.job_id) u.job_id, b.day, u.n
		   FROM brief_log b, unnest(b.job_ids) WITH ORDINALITY AS u(job_id, n)
		   WHERE b.sent_at IS NOT NULL AND b.day >= $1::date
		   ORDER BY u.job_id, b.day DESC, u.n
		 ), picked AS (
		   SELECT j.id, j.company_id, coalesce(o.title, j.title) AS title, c.name AS company,
		          coalesce(o.location_text, '') AS location, coalesce(s.status, 'new') AS status,
		          j.listing_status, j.url, coalesce(o.apply_url, '') AS apply_url,
		          coalesce(o.apply_type, '') AS apply_type, b.day, b.n
		   FROM briefed b
		   JOIN job j ON j.id = b.job_id
		   JOIN company c ON c.id = j.company_id
		   LEFT JOIN LATERAL (SELECT title, location_text, apply_url, apply_type, closed
		                      FROM job_observation WHERE job_id = j.id
		                      ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		   LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                      ORDER BY at DESC, id DESC LIMIT 1) s ON true
		   WHERE j.listing_status = 'open' AND NOT coalesce(o.closed, false)
		     AND coalesce(s.status, 'new') IN ('new', 'shortlisted')
		   ORDER BY b.day DESC, b.n
		   LIMIT $2
		 ), handled AS MATERIALIZED (
		   SELECT j.id, j.company_id, coalesce(o.title, j.title) AS title, s.status, s.at
		   FROM job j
		   JOIN LATERAL (SELECT status, at FROM status_event WHERE job_id = j.id
		                 ORDER BY at DESC, id DESC LIMIT 1) s ON s.status <> 'new'
		   LEFT JOIN LATERAL (SELECT title FROM job_observation WHERE job_id = j.id
		                      ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		   WHERE j.company_id IN (SELECT company_id FROM picked)
		 )
		 SELECT p.id, p.title, p.company, p.location, p.status, p.listing_status, p.url, p.apply_url,
		        p.apply_type, p.day, p.n, h.ids, h.titles, h.statuses, h.ats
		 FROM picked p
		 LEFT JOIN LATERAL (SELECT array_agg(id ORDER BY at DESC, id) AS ids,
		                           array_agg(title ORDER BY at DESC, id) AS titles,
		                           array_agg(status ORDER BY at DESC, id) AS statuses,
		                           array_agg(at ORDER BY at DESC, id) AS ats
		                    FROM handled WHERE company_id = p.company_id AND id <> p.id) h ON true
		 ORDER BY p.day DESC, p.n`, since.Format(time.DateOnly), limit)
	if err != nil {
		return nil, fmt.Errorf("store: next jobs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (NextJob, error) {
		var (
			n              NextJob
			ids            []int64
			titles, states []string
			ats            []time.Time
		)
		err := r.Scan(&n.JobID, &n.Title, &n.Company, &n.Location, &n.Status, &n.ListingStatus, &n.URL, &n.ApplyURL,
			&n.ApplyType, &n.BriefDay, &n.BriefN, &ids, &titles, &states, &ats)
		if err != nil {
			return NextJob{}, err
		}
		n.Prior = make([]PriorJob, len(ids))
		for i := range ids {
			n.Prior[i] = PriorJob{JobID: ids[i], Title: titles[i], Status: states[i], Since: ats[i]}
		}
		return n, nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: next jobs: %w", err)
	}
	return out, nil
}
