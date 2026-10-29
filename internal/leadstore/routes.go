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

// The kinds a contact route can be (contact_route_kind_check).
const (
	RouteEmail = "email"
	RouteForm  = "form"
)

// MaxPhraseRunes is the longest agency_finding phrase (agency_finding_phrase_check).
const MaxPhraseRunes = 120

// RouteRetryAfter is how long an org searched for routes waits before OrgsNeedingRoutes
// returns it again.
const RouteRetryAfter = 30 * 24 * time.Hour

var (
	// ErrUnknownRouteKind is returned by InsertRoute for a kind other than email or form.
	ErrUnknownRouteKind = errors.New("leadstore: unknown route kind")
	// ErrEmptyRoute is returned by InsertRoute for a blank value.
	ErrEmptyRoute = errors.New("leadstore: empty route value")
	// ErrPhraseTooLong is returned by InsertAgencyFinding for a phrase over MaxPhraseRunes.
	ErrPhraseTooLong = errors.New("leadstore: agency finding phrase too long")
)

// RouteRow is one contact_route row to write.
type RouteRow struct {
	OrgID     int64
	Kind      string // RouteEmail or RouteForm
	Value     string // the address (stored lower case) or the form page URL
	FetchID   int64  // the page it was seen on
	MXOK      bool
	DerivedBy string // e.g. "contacts@1"
}

// InsertRoute writes r. A route the org already has (same kind and value) is kept as it
// was; inserted reports whether this call added the row.
func (q Queries) InsertRoute(ctx context.Context, r RouteRow) (inserted bool, err error) {
	value := strings.TrimSpace(r.Value)
	switch r.Kind {
	case RouteEmail:
		value = strings.ToLower(value)
	case RouteForm:
	default:
		return false, fmt.Errorf("leadstore: insert route: %w: %q", ErrUnknownRouteKind, r.Kind)
	}
	if value == "" {
		return false, fmt.Errorf("leadstore: insert route: %w", ErrEmptyRoute)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO contact_route (org_id, kind, value, fetch_id, mx_ok, derived_by)
		 VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (org_id, kind, value) DO NOTHING`,
		r.OrgID, r.Kind, value, r.FetchID, r.MXOK, r.DerivedBy)
	if err != nil {
		return false, fmt.Errorf("leadstore: insert route: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Route is one contact_route row.
type Route struct {
	ID        int64     `db:"id"`
	OrgID     int64     `db:"org_id"`
	Kind      string    `db:"kind"`
	Value     string    `db:"value"`
	FetchID   int64     `db:"fetch_id"` // 0 for a route read from a place (00022)
	MXOK      bool      `db:"mx_ok"`
	DerivedBy string    `db:"derived_by"`
	FoundAt   time.Time `db:"found_at"`
}

// RoutesFor returns the routes of org orgID, emails first, then in the order found.
func (q Queries) RoutesFor(ctx context.Context, orgID int64) ([]Route, error) {
	routes, err := collect(ctx, q.db, pgx.RowToStructByName[Route],
		`SELECT id, org_id, kind, value, coalesce(fetch_id, 0) AS fetch_id, mx_ok, derived_by, found_at
		 FROM contact_route WHERE org_id = $1 ORDER BY kind, id`, orgID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: routes for org %d: %w", orgID, err)
	}
	return routes, nil
}

// AgencyFindingRow is one agency_finding row to write.
type AgencyFindingRow struct {
	OrgID     int64
	URL       string // the agency's page that sells upgrades
	Phrase    string // the matched words, at most MaxPhraseRunes
	FetchID   int64
	DerivedBy string
}

// InsertAgencyFinding writes f and returns its id.
func (q Queries) InsertAgencyFinding(ctx context.Context, f AgencyFindingRow) (int64, error) {
	if n := utf8.RuneCountInString(f.Phrase); n > MaxPhraseRunes {
		return 0, fmt.Errorf("leadstore: insert agency finding: %w: %d runes", ErrPhraseTooLong, n)
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO agency_finding (org_id, url, phrase, fetch_id, derived_by)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		f.OrgID, f.URL, f.Phrase, f.FetchID, f.DerivedBy).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: insert agency finding: %w", err)
	}
	return id, nil
}

// RecordContactAttempt notes one route search for org orgID in run runID that found
// routesFound routes, none included, so OrgsNeedingRoutes skips the org for
// RouteRetryAfter.
func (q Queries) RecordContactAttempt(ctx context.Context, orgID, runID int64, routesFound int) error {
	if _, err := q.db.Exec(ctx,
		`INSERT INTO contact_attempt (org_id, run_id, routes_found) VALUES ($1, $2, $3)`,
		orgID, runID, routesFound); err != nil {
		return fmt.Errorf("leadstore: record contact attempt: %w", err)
	}
	return nil
}

// OrgsNeedingRoutes returns up to limit orgs (limit <= 0: all of them) with no route search
// and no route found in the RouteRetryAfter before now, oldest org first. A route read from a
// place's map listing (00022) does not count: the org's own pages are still searched. Selftest orgs are
// never returned, nor an org whose domain is suppressed (orgDomainSuppressedSQL).
func (q Queries) OrgsNeedingRoutes(ctx context.Context, limit int, now time.Time) ([]Org, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	orgs, err := collect(ctx, q.db, pgx.RowToStructByName[Org],
		`SELECT o.id, o.kind, o.domain, o.name, o.country, o.region, o.site_id, o.source, o.source_fetch_id, o.created_at
		 FROM org o
		 WHERE o.kind <> 'selftest'
		   AND NOT (o.source = 'place' AND o.site_id IS NULL)
		   AND NOT EXISTS (SELECT 1 FROM contact_attempt a WHERE a.org_id = o.id AND a.attempted_at > $1)
		   AND NOT EXISTS (SELECT 1 FROM contact_route r WHERE r.org_id = o.id AND r.found_at > $1 AND r.place_id IS NULL)
		   AND NOT `+orgDomainSuppressedSQL("o")+`
		 ORDER BY o.id
		 LIMIT $2`, now.Add(-RouteRetryAfter), lim)
	if err != nil {
		return nil, fmt.Errorf("leadstore: orgs needing routes: %w", err)
	}
	return orgs, nil
}
