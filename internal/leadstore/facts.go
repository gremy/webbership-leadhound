package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// The kinds an org fact can be (org_fact_kind_check, migration 00012).
const (
	FactPhone       = "phone"
	FactDescription = "description"
	FactSocial      = "social"
)

// MaxFactRunes is the longest fact value and normalized value (org_fact_value_check).
const MaxFactRunes = 300

// EnrichRetryAfter is how long an org enriched once waits before OrgsNeedingEnrich returns it
// again.
const EnrichRetryAfter = 30 * 24 * time.Hour

var (
	// ErrUnknownFactKind is returned by InsertFact for a kind other than phone, description
	// or social.
	ErrUnknownFactKind = errors.New("leadstore: unknown fact kind")
	// ErrEmptyFact is returned by InsertFact for a blank value or normalized value.
	ErrEmptyFact = errors.New("leadstore: empty fact value")
	// ErrFactTooLong is returned by InsertFact for a value over MaxFactRunes.
	ErrFactTooLong = errors.New("leadstore: fact value too long")
)

// FactRow is one org_fact row to write.
type FactRow struct {
	OrgID      int64
	Kind       string // FactPhone, FactDescription or FactSocial
	Value      string // as shown on the page, cleaned
	Normalized string // the comparison key: a phone's + and digits, a profile URL, a description itself
	FetchID    int64  // the page it was read from
	DerivedBy  string // e.g. "enrich@1"
}

// Fact is one stored org fact.
type Fact struct {
	ID         int64     `db:"id"`
	OrgID      int64     `db:"org_id"`
	Kind       string    `db:"kind"`
	Value      string    `db:"value"`
	Normalized string    `db:"normalized"`
	FetchID    int64     `db:"fetch_id"`
	DerivedBy  string    `db:"derived_by"`
	FoundAt    time.Time `db:"found_at"`
}

// InsertFact writes f. A fact the org already has (same kind and normalized value) is kept as
// it was; inserted reports whether this call added the row. A phone whose normalized value is
// not + and 8 to 15 digits is refused by org_fact_phone_check.
func (q Queries) InsertFact(ctx context.Context, f FactRow) (inserted bool, err error) {
	switch f.Kind {
	case FactPhone, FactDescription, FactSocial:
	default:
		return false, fmt.Errorf("leadstore: insert fact: %w: %q", ErrUnknownFactKind, f.Kind)
	}
	value, norm := strings.TrimSpace(f.Value), strings.TrimSpace(f.Normalized)
	if value == "" || norm == "" {
		return false, fmt.Errorf("leadstore: insert fact: %w", ErrEmptyFact)
	}
	if utf8.RuneCountInString(value) > MaxFactRunes || utf8.RuneCountInString(norm) > MaxFactRunes {
		return false, fmt.Errorf("leadstore: insert fact: %w", ErrFactTooLong)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, derived_by)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (org_id, kind, normalized) DO NOTHING`,
		f.OrgID, f.Kind, value, norm, f.FetchID, f.DerivedBy)
	if err != nil {
		return false, fmt.Errorf("leadstore: insert fact: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// OrgFacts returns every fact of the orgs orgIDs, by org, then kind, then the order they
// were found in.
func (q Queries) OrgFacts(ctx context.Context, orgIDs []int64) ([]Fact, error) {
	if len(orgIDs) == 0 {
		return nil, nil
	}
	facts, err := collect(ctx, q.db, pgx.RowToStructByName[Fact],
		`SELECT id, org_id, kind, value, normalized, fetch_id, derived_by, found_at
		 FROM org_fact WHERE org_id = ANY($1) ORDER BY org_id, kind, id`, orgIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: org facts: %w", err)
	}
	return facts, nil
}

// RecordEnrichAttempt notes one enrichment of org orgID in run runID that found factsFound
// facts, none included, so OrgsNeedingEnrich skips the org for EnrichRetryAfter.
func (q Queries) RecordEnrichAttempt(ctx context.Context, orgID, runID int64, factsFound int) error {
	if _, err := q.db.Exec(ctx,
		`INSERT INTO enrich_attempt (org_id, run_id, facts_found) VALUES ($1, $2, $3)`,
		orgID, runID, factsFound); err != nil {
		return fmt.Errorf("leadstore: record enrich attempt: %w", err)
	}
	return nil
}

// OrgsNeedingEnrich returns up to limit orgs (limit <= 0: all of them), each once however many
// campaigns it is a lead in, that have a lead and
// no enrichment in the EnrichRetryAfter before now, the newest lead first. Selftest orgs and
// selftest leads are never returned, nor an org whose domain is suppressed (a forgotten or
// opted-out org is never read again: orgDomainSuppressedSQL).
func (q Queries) OrgsNeedingEnrich(ctx context.Context, limit int, now time.Time) ([]Org, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	orgs, err := collect(ctx, q.db, pgx.RowToStructByName[Org],
		`SELECT o.id, o.kind, o.domain, o.name, o.country, o.region, o.site_id, o.source, o.source_fetch_id, o.created_at
		 FROM (SELECT org_id, max(id) AS id FROM lead WHERE kind <> 'selftest' GROUP BY org_id) l
		 JOIN org o ON o.id = l.org_id
		 WHERE o.kind <> 'selftest'
		   AND NOT EXISTS (SELECT 1 FROM enrich_attempt a WHERE a.org_id = o.id AND a.attempted_at > $1)
		   AND NOT `+orgDomainSuppressedSQL("o")+`
		 ORDER BY l.id DESC
		 LIMIT $2`, now.Add(-EnrichRetryAfter), lim)
	if err != nil {
		return nil, fmt.Errorf("leadstore: orgs needing enrich: %w", err)
	}
	return orgs, nil
}

// EnrichBody is one kept page of an org that enrichment reads.
type EnrichBody struct {
	FetchID int64  `db:"fetch_id"`
	URL     string `db:"url"`     // the page's address after redirects (final_url, else the URL asked for)
	Body    []byte `db:"body"`    // as it arrived; never empty
	Contact bool   `db:"contact"` // a contact page: a route was read from it
}

// EnrichBodies returns the kept page bodies of org orgID that enrichment reads, each once:
// the org's homepage (org.source_fetch_id), the homepage fetches of its lead's probe and of
// its site's latest probe, and every page a contact route was read from. A fetch kept as
// size and hash only (an empty body) is left out. Homepages come first, then the contact
// pages, each by fetch id. A missing org is ErrOrgNotFound.
func (q Queries) EnrichBodies(ctx context.Context, orgID int64) ([]EnrichBody, error) {
	var ok bool
	if err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org WHERE id = $1)`, orgID).Scan(&ok); err != nil {
		return nil, fmt.Errorf("leadstore: enrich bodies: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("leadstore: enrich bodies: org %d: %w", orgID, ErrOrgNotFound)
	}
	bodies, err := collect(ctx, q.db, pgx.RowToStructByName[EnrichBody],
		`WITH o AS (SELECT id, source_fetch_id, site_id FROM org WHERE id = $1),
		 pages AS (
		   SELECT o.source_fetch_id AS fetch_id, false AS contact FROM o WHERE o.source_fetch_id IS NOT NULL
		   UNION ALL
		   SELECT x.fetch_id, false FROM o JOIN lead l ON l.org_id = o.id JOIN site_probe p ON p.id = l.probe_id
		     CROSS JOIN LATERAL unnest(p.fetch_ids) AS x(fetch_id)
		   UNION ALL
		   SELECT x.fetch_id, false FROM o JOIN site s ON s.id = o.site_id JOIN site_probe p ON p.id = s.last_probe_id
		     CROSS JOIN LATERAL unnest(p.fetch_ids) AS x(fetch_id)
		   UNION ALL
		   SELECT r.fetch_id, true FROM contact_route r JOIN o ON r.org_id = o.id
		 )
		 SELECT f.id AS fetch_id, coalesce(nullif(f.final_url, ''), f.url) AS url, f.body, bool_or(pg.contact) AS contact
		 FROM pages pg JOIN http_fetch f ON f.id = pg.fetch_id
		 WHERE f.kind = 'site' AND length(f.body) > 0
		 GROUP BY f.id
		 ORDER BY bool_or(pg.contact), f.id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: enrich bodies: %w", err)
	}
	return bodies, nil
}
