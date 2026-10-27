package leadstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RateLead is one open lead as the relevance question reads it (leadrate.Input), with its
// latest rating's input hash. It holds no email address and no phone number: only which
// channels and facts the org has.
type RateLead struct {
	LeadID       int64  `db:"lead_id"`
	OrgID        int64  `db:"org_id"`
	Domain       string `db:"domain"`
	Kind         string `db:"kind"`
	Country      string `db:"country"`
	Major        int    `db:"major"`         // the finding's major, else the probe's; 0 when unknown
	ExactVersion string `db:"exact_version"` // the finding's exact version, else the probe's
	ListRank     *int   `db:"list_rank"`
	SiteName     string `db:"site_name"`   // the probe's site name, else the org's name; "" when neither
	Description  string `db:"description"` // the org's first description fact; "" when none
	HasEmail     bool   `db:"has_email"`
	HasForm      bool   `db:"has_form"`
	HasPhone     bool   `db:"has_phone"`
	SocialLinks  int    `db:"social_links"`
	Starred      bool   `db:"starred"`
	// LatestSHA is the input hash of the lead's latest rating; nil when it was never rated.
	LatestSHA []byte `db:"latest_sha"`
}

// rateLeadSQL is every lead but the selftest ones as RateLead's columns, over listRowsSQL
// ($1 the profile, $2 SkipRankOver) as m.
const rateLeadSQL = `SELECT m.lead_id, m.org_id, m.domain, m.kind, m.country,
	   CASE WHEN m.f_major > 0 THEN m.f_major ELSE m.probe_major END AS major,
	   coalesce(nullif(m.f_exact_version, ''), m.probe_exact) AS exact_version,
	   m.list_rank,
	   coalesce(nullif(p.site_name, ''), o.name, '') AS site_name,
	   coalesce(d.value, '') AS description,
	   m.has_email, m.has_form, m.has_phone, sl.n AS social_links, m.starred, h.sha AS latest_sha
	 FROM (` + listRowsSQL + `) m
	 JOIN org o ON o.id = m.org_id
	 LEFT JOIN site_probe p ON p.id = m.probe_id
	 LEFT JOIN LATERAL (
	   SELECT x.value FROM org_fact x WHERE x.org_id = m.org_id AND x.kind = 'description' ORDER BY x.id LIMIT 1
	 ) d ON true
	 CROSS JOIN LATERAL (
	   SELECT count(*)::int AS n FROM org_fact x WHERE x.org_id = m.org_id AND x.kind = 'social'
	 ) sl
	 LEFT JOIN LATERAL (
	   SELECT r.input_sha256 AS sha FROM lead_relevance r WHERE r.lead_id = m.lead_id ORDER BY r.id DESC LIMIT 1
	 ) h ON true`

// LeadsToRate returns every open lead (state new; never a selftest one, as the list shows
// them) in the order rating takes them (D8): the starred leads first, by the operator's
// position (then id), then the rest in the queue's order (CandidateLeads): by their latest
// relevance (unrated last) when c.SortByRelevance, then by points under c.ProfileJSON, then
// by their site's list rank (unranked last), then by id. Every open lead is returned, not
// only those a batch may draft, so the list's leads with no channel get a number too.
func (q Queries) LeadsToRate(ctx context.Context, c ListContext) ([]RateLead, error) {
	order := "m.points DESC, m.list_rank NULLS LAST, m.lead_id"
	if c.SortByRelevance {
		order = "m.relevance DESC NULLS LAST, " + order
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[RateLead],
		rateLeadSQL+`
		 WHERE m.state = 'new' AND ($3::bigint = 0 OR m.campaign_id = $3::bigint)
		 ORDER BY m.starred DESC, m.position, CASE WHEN m.starred THEN m.lead_id END, `+order,
		profileOrEmpty(c.ProfileJSON), c.SkipRankOver, c.CampaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: leads to rate: %w", err)
	}
	return rows, nil
}

// RateLeadByID returns lead leadID as the relevance question reads it, whatever its state,
// read exactly as LeadsToRate reads an open lead; a selftest lead or a missing one is
// ErrLeadNotFound.
func (q Queries) RateLeadByID(ctx context.Context, leadID int64) (RateLead, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[RateLead],
		rateLeadSQL+` WHERE m.lead_id = $3`, "{}", 0, leadID)
	if err != nil {
		return RateLead{}, fmt.Errorf("leadstore: rate lead: %w", err)
	}
	if len(rows) == 0 {
		return RateLead{}, fmt.Errorf("leadstore: rate lead %d: %w", leadID, ErrLeadNotFound)
	}
	return rows[0], nil
}
