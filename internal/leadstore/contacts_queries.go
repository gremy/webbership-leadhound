package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// ProbeForOrg is a site's latest probe that found Drupal, and the registrable domain its
// org would take.
type ProbeForOrg struct {
	ProbeID int64
	Domain  string
}

// DrupalProbesWithoutOrg returns every site's latest probe (site.last_probe_id) that found
// Drupal, where no org points at the site and no org holds the registrable domain of the
// probe's final host (the site's domain when it has none). Oldest probe first. A host with
// no registrable domain is left out.
func (q Queries) DrupalProbesWithoutOrg(ctx context.Context) ([]ProbeForOrg, error) {
	type row struct {
		ProbeID int64  `db:"probe_id"`
		Host    string `db:"host"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT p.id AS probe_id, coalesce(nullif(p.final_host, ''), s.domain) AS host
		 FROM site s JOIN site_probe p ON p.id = s.last_probe_id
		 WHERE p.is_drupal AND NOT EXISTS (SELECT 1 FROM org o WHERE o.site_id = s.id)
		 ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: drupal probes without org: %w", err)
	}
	out := make([]ProbeForOrg, 0, len(rows))
	domains := make([]string, 0, len(rows))
	for _, r := range rows {
		d, err := sitelist.Registrable(r.Host)
		if err != nil {
			continue
		}
		out = append(out, ProbeForOrg{ProbeID: r.ProbeID, Domain: d})
		domains = append(domains, d)
	}
	if len(out) == 0 {
		return nil, nil
	}
	held, err := collect(ctx, q.db, pgx.RowTo[string], `SELECT domain FROM org WHERE domain = ANY($1)`, domains)
	if err != nil {
		return nil, fmt.Errorf("leadstore: drupal probes without org: %w", err)
	}
	taken := map[string]bool{}
	for _, d := range held {
		taken[d] = true
	}
	kept := out[:0]
	for _, p := range out {
		if !taken[p.Domain] {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return kept, nil
}

// Homepage is the fetch an org was found from (org.source_fetch_id).
type Homepage struct {
	FetchID  int64  // 0 when the org has no source fetch
	URL      string // the URL asked for
	FinalURL string // "" when it equals URL
	Body     []byte // empty when the body was kept as size and hash only
}

// OrgHomepage returns org orgID's source fetch: for a site owner, its probe's homepage,
// whose body is kept for a Drupal hit. An org without one gives a zero Homepage; an empty
// Body means the caller fetches the page itself. A missing org is ErrOrgNotFound.
func (q Queries) OrgHomepage(ctx context.Context, orgID int64) (Homepage, error) {
	var (
		fetchID  *int64
		u, final *string
		body     []byte
		hasRow   bool
	)
	err := q.db.QueryRow(ctx,
		`SELECT f.id IS NOT NULL, f.id, f.url, f.final_url, f.body
		 FROM org o LEFT JOIN http_fetch f ON f.id = o.source_fetch_id WHERE o.id = $1`,
		orgID).Scan(&hasRow, &fetchID, &u, &final, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Homepage{}, fmt.Errorf("leadstore: org homepage: org %d: %w", orgID, ErrOrgNotFound)
	}
	if err != nil {
		return Homepage{}, fmt.Errorf("leadstore: org homepage: %w", err)
	}
	if !hasRow {
		return Homepage{}, nil
	}
	h := Homepage{FetchID: *fetchID, Body: body}
	if u != nil {
		h.URL = *u
	}
	if final != nil {
		h.FinalURL = *final
	}
	return h, nil
}

// OrgByID returns org id, or ErrOrgNotFound.
func (q Queries) OrgByID(ctx context.Context, id int64) (Org, error) {
	orgs, err := collect(ctx, q.db, pgx.RowToStructByName[Org],
		`SELECT id, kind, domain, name, country, site_id, source, source_fetch_id, created_at
		 FROM org WHERE id = $1`, id)
	if err != nil {
		return Org{}, fmt.Errorf("leadstore: org by id: %w", err)
	}
	if len(orgs) == 0 {
		return Org{}, fmt.Errorf("leadstore: org by id %d: %w", id, ErrOrgNotFound)
	}
	return orgs[0], nil
}

// SiteIDByDomain returns the id of the site row on domain (matched lower case), or
// ErrSiteNotFound.
func (q Queries) SiteIDByDomain(ctx context.Context, domain string) (int64, error) {
	var id int64
	err := q.db.QueryRow(ctx, `SELECT id FROM site WHERE domain = $1`,
		strings.ToLower(strings.TrimSpace(domain))).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("leadstore: site by domain %q: %w", domain, ErrSiteNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: site by domain: %w", err)
	}
	return id, nil
}

// HasAgencyFinding reports whether org orgID has at least one agency_finding row.
func (q Queries) HasAgencyFinding(ctx context.Context, orgID int64) (bool, error) {
	var ok bool
	if err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agency_finding WHERE org_id = $1)`, orgID).Scan(&ok); err != nil {
		return false, fmt.Errorf("leadstore: has agency finding: %w", err)
	}
	return ok, nil
}

// ExportRow is one org with a route, for the hand-written outreach list.
type ExportRow struct {
	Domain  string `db:"domain"`
	Kind    string `db:"kind"`
	Name    string `db:"name"`
	Country string `db:"country"`
	// The finding: a site owner's latest probe (major, exact version), or an agency's first
	// agency_finding (phrase and its page).
	Major        int    `db:"major"`
	ExactVersion string `db:"exact_version"`
	Phrase       string `db:"phrase"`
	FindingURL   string `db:"finding_url"`
	// The best route: an email with MX first, then a form, then an email without MX.
	RouteKind string `db:"route_kind"`
	Route     string `db:"route"`
	MXOK      bool   `db:"mx_ok"`
}

// ExportRoutes returns up to limit orgs that have a route (limit <= 0: all of them), by
// their site's list rank (unranked last), then org id. Selftest orgs and orgs on the
// reserved TLDs .example, .invalid, .test and .localhost are left out.
func (q Queries) ExportRoutes(ctx context.Context, limit int) ([]ExportRow, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[ExportRow],
		`SELECT o.domain, o.kind, o.name, o.country,
		   coalesce(p.major, 0) AS major, coalesce(p.exact_version, '') AS exact_version,
		   coalesce(af.phrase, '') AS phrase, coalesce(af.url, '') AS finding_url,
		   r.kind AS route_kind, r.value AS route, r.mx_ok
		 FROM org o
		 LEFT JOIN site s ON s.id = o.site_id
		 LEFT JOIN site_probe p ON p.id = s.last_probe_id AND o.kind = 'site_owner' AND p.is_drupal
		 LEFT JOIN LATERAL (
		   SELECT phrase, url FROM agency_finding f WHERE f.org_id = o.id ORDER BY f.id LIMIT 1
		 ) af ON o.kind = 'agency'
		 JOIN LATERAL (
		   SELECT kind, value, mx_ok FROM contact_route c WHERE c.org_id = o.id
		   ORDER BY CASE WHEN c.kind = 'email' AND c.mx_ok THEN 0 WHEN c.kind = 'form' THEN 1 ELSE 2 END, c.id
		   LIMIT 1
		 ) r ON true
		 WHERE o.kind <> 'selftest'
		   AND o.domain !~ '\.(example|invalid|test|localhost)$'
		 ORDER BY s.list_rank NULLS LAST, o.id
		 LIMIT $1`, lim)
	if err != nil {
		return nil, fmt.Errorf("leadstore: export routes: %w", err)
	}
	return rows, nil
}
