package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// NextJob is a job from a sent brief that is still worth applying to.
type NextJob struct {
	JobID                                                                     int64
	Title, Company, Location, Status, ListingStatus, URL, ApplyURL, ApplyType string
	BriefDay                                                                  time.Time // the brief's date, at midnight UTC; zero from NeverBriefedJobs
	BriefN                                                                    int       // its number in that brief: BriefLog.JobIDs[BriefN-1]; 0 from NeverBriefedJobs
	Prior                                                                     []PriorJob
	Description                                                               string // the latest observation's text, only when the read asked for it
	LatestActor                                                               string // who wrote the latest status event; system when there is none
	StoredRuleReason                                                          string // from NeverBriefedJobs: why the current observation failed the rule gate; empty when it did not
}

// PriorJob is another job of the same company whose status is past new. One posting can be
// two jobs, because identity is per source, so this is what shows it was already handled.
type PriorJob struct {
	JobID         int64
	Title, Status string
	Since         time.Time // when the job took its current status
}

// What both reads share of picked: the job's latest observation and latest status, narrowed to a
// current status of new or shortlisted, an open listing and a latest observation not closed. $2
// chooses whether the description is read, so an unread one is never fetched from its TOAST table.
const (
	nextJobPickedColumns = `j.id, j.company_id, coalesce(o.title, j.title) AS title, c.name AS company,
		          coalesce(o.location_text, '') AS location, coalesce(s.status, 'new') AS status,
		          coalesce(s.actor, 'system') AS actor, j.listing_status, j.url,
		          coalesce(o.apply_url, '') AS apply_url, coalesce(o.apply_type, '') AS apply_type,
		          CASE WHEN $2::boolean THEN coalesce(o.description_clean, '') ELSE '' END AS description`
	nextJobPickedJoins = `JOIN company c ON c.id = j.company_id
		   LEFT JOIN LATERAL (SELECT id, title, location_text, apply_url, apply_type, closed, description_clean
		                      FROM job_observation WHERE job_id = j.id
		                      ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		   LEFT JOIN LATERAL (SELECT status, actor FROM status_event WHERE job_id = j.id
		                      ORDER BY at DESC, id DESC LIMIT 1) s ON true
		   WHERE j.listing_status = 'open' AND NOT coalesce(o.closed, false)
		     AND coalesce(s.status, 'new') IN ('new', 'shortlisted')`
)

// nextJobRows ends both reads: picked's rows with each one's Prior, for the companies priorFrom
// selects from picked. handled is materialized so that job, which has no index on company_id, is
// read once for all those companies rather than once per row.
func nextJobRows(priorFrom string) string {
	return `, handled AS MATERIALIZED (
		   SELECT j.id, j.company_id, coalesce(o.title, j.title) AS title, s.status, s.at
		   FROM job j
		   JOIN LATERAL (SELECT status, at FROM status_event WHERE job_id = j.id
		                 ORDER BY at DESC, id DESC LIMIT 1) s ON s.status <> 'new'
		   LEFT JOIN LATERAL (SELECT title FROM job_observation WHERE job_id = j.id
		                      ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		   WHERE j.company_id IN (` + priorFrom + `)
		 )
		 SELECT p.id, p.title, p.company, p.location, p.status, p.listing_status, p.url, p.apply_url,
		        p.apply_type, p.day, p.n, p.description, p.actor, p.stored_rule_reason, h.ids, h.titles, h.statuses, h.ats
		 FROM picked p
		 LEFT JOIN LATERAL (SELECT array_agg(id ORDER BY at DESC, id) AS ids,
		                           array_agg(title ORDER BY at DESC, id) AS titles,
		                           array_agg(status ORDER BY at DESC, id) AS statuses,
		                           array_agg(at ORDER BY at DESC, id) AS ats
		                    FROM handled WHERE company_id = p.company_id AND id <> p.id) h ON true`
}

// scanNextJob reads one row of nextJobRows.
func scanNextJob(r pgx.CollectableRow) (NextJob, error) {
	var (
		n              NextJob
		day            *time.Time
		ids            []int64
		titles, states []string
		ats            []time.Time
	)
	err := r.Scan(&n.JobID, &n.Title, &n.Company, &n.Location, &n.Status, &n.ListingStatus, &n.URL, &n.ApplyURL,
		&n.ApplyType, &day, &n.BriefN, &n.Description, &n.LatestActor, &n.StoredRuleReason, &ids, &titles, &states, &ats)
	if err != nil {
		return NextJob{}, err
	}
	if day != nil {
		n.BriefDay = *day
	}
	n.Prior = make([]PriorJob, len(ids))
	for i := range ids {
		n.Prior[i] = PriorJob{JobID: ids[i], Title: titles[i], Status: states[i], Since: ats[i]}
	}
	return n, nil
}

// NextJobs returns the jobs of the briefs sent on or after since's calendar date, one row per
// job under its newest brief, newest brief first and then in brief order. It keeps only a
// current status of new or shortlisted, an open listing and a latest observation not closed; a
// job with no observation lists with its card's title and empty fields. Prior is empty, never
// nil, when the company has no other job past new. Description is read only with
// needDescription.
//
// BriefN stays the number the brief itself gave, whichever of its jobs are left out. There is no
// limit here: the caller filters by the skip rules, which this package cannot read, before it
// cuts the page.
func (q Queries) NextJobs(ctx context.Context, since time.Time, needDescription bool) ([]NextJob, error) {
	rows, err := q.db.Query(ctx,
		`WITH briefed AS (
		   SELECT DISTINCT ON (u.job_id) u.job_id, b.day, u.n
		   FROM brief_log b, unnest(b.job_ids) WITH ORDINALITY AS u(job_id, n)
		   WHERE b.sent_at IS NOT NULL AND b.day >= $1::date
		   ORDER BY u.job_id, b.day DESC, u.n
		 ), picked AS (
		   SELECT `+nextJobPickedColumns+`, b.day, b.n, '' AS stored_rule_reason
		   FROM briefed b
		   JOIN job j ON j.id = b.job_id
		   `+nextJobPickedJoins+`
		 )`+nextJobRows(`SELECT company_id FROM picked`)+`
		 ORDER BY p.day DESC, p.n`, since.Format(time.DateOnly), needDescription)
	if err != nil {
		return nil, fmt.Errorf("store: next jobs: %w", err)
	}
	out, err := pgx.CollectRows(rows, scanNextJob)
	if err != nil {
		return nil, fmt.Errorf("store: next jobs: %w", err)
	}
	return out, nil
}

// NeverBriefedJobs streams the jobs a skip rule may be keeping from a brief and returns those
// keep says yes to, in id order: in no brief sent on or after briefedSince's calendar date, under
// NextJobs's status and listing limits, first seen at or after firstSeen unless their latest
// actor is not system, with BriefDay zero and BriefN 0.
//
// StoredRuleReason is why the current observation failed the rule gate, which the ranker goes on
// skipping after the live match has stopped: when the trim blanks the description a rule read,
// or the rule is disabled. It is the rule's reason, else the gate's, else a plain word, so it is
// empty only when there is no such fail. With liveRules false only jobs with a rule-gate fail on
// some observation are read, because nothing else can list.
//
// A job whose latest observation fails only other gates is left out: those gates keep it from
// every brief whatever the rules say, and after a wide-net night such jobs are nine in ten of the
// table. Prior is read only for the companies of jobs whose latest actor is not system, the only
// kind a caller lists as a job. keep is called once per row, so memory follows what it keeps.
func (q Queries) NeverBriefedJobs(ctx context.Context, briefedSince, firstSeen time.Time, needDescription, liveRules bool, keep func(NextJob) bool) ([]NextJob, error) {
	rows, err := q.db.Query(ctx,
		`WITH briefed AS (
		   SELECT DISTINCT u.job_id
		   FROM brief_log b, unnest(b.job_ids) AS u(job_id)
		   WHERE b.sent_at IS NOT NULL AND b.day >= $1::date
		 ), picked AS (
		   SELECT `+nextJobPickedColumns+`, NULL::date AS day, 0::bigint AS n,
		          coalesce((SELECT coalesce(nullif(r.reason, ''), nullif(g.reason, ''), 'a stored skip rule')
		                    FROM gate_result g LEFT JOIN rule r ON r.id = g.rule_id
		                    WHERE g.observation_id = o.id AND g.gate = 'rule' AND g.verdict = 'fail'
		                    ORDER BY g.id LIMIT 1), '') AS stored_rule_reason
		   FROM job j
		   `+nextJobPickedJoins+`
		     AND ($4::boolean OR j.id IN (SELECT job_id FROM gate_result WHERE gate = 'rule' AND verdict = 'fail'))
		     AND (j.first_seen_at >= $3 OR coalesce(s.actor, 'system') <> 'system')
		     AND NOT EXISTS (SELECT 1 FROM briefed b WHERE b.job_id = j.id)
		     AND (NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.verdict = 'fail')
		          OR EXISTS (SELECT 1 FROM gate_result g
		                     WHERE g.observation_id = o.id AND g.verdict = 'fail' AND g.gate = 'rule'))
		 )`+nextJobRows(`SELECT company_id FROM picked WHERE actor <> 'system'`),
		briefedSince.Format(time.DateOnly), needDescription, firstSeen, liveRules)
	if err != nil {
		return nil, fmt.Errorf("store: never briefed jobs: %w", err)
	}
	defer rows.Close()
	out := []NextJob{}
	for rows.Next() {
		n, err := scanNextJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: never briefed jobs: %w", err)
		}
		if keep(n) {
			out = append(out, n)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: never briefed jobs: %w", err)
	}
	slices.SortFunc(out, func(a, b NextJob) int { return cmp.Compare(a.JobID, b.JobID) })
	return out, nil
}
