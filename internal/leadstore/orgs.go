package leadstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// The kinds an org can be (org_kind_check).
const (
	OrgSiteOwner = "site_owner"
	OrgAgency    = "agency"
	OrgSelftest  = "selftest"
)

// The sources an org can come from (org_source_check).
const (
	OrgSourceProbe    = "probe"
	OrgSourceAgencies = "agencies-file"
	OrgSourceSelftest = "selftest"
)

var (
	// ErrProbeNotFound is returned by UpsertSiteOrg when no site_probe row has that id.
	ErrProbeNotFound = errors.New("leadstore: probe not found")
	// ErrNotDrupalProbe is returned by UpsertSiteOrg for a probe that did not find Drupal.
	ErrNotDrupalProbe = errors.New("leadstore: probe did not find drupal")
	// ErrNoDomain is returned when no registrable domain can be read for an org.
	ErrNoDomain = errors.New("leadstore: no registrable domain")
	// ErrOrgNotFound is returned by OrgSetSite when no org row has that id.
	ErrOrgNotFound = errors.New("leadstore: org not found")
	// ErrOrgSiteSet is returned by OrgSetSite when the org already points at another site.
	ErrOrgSiteSet = errors.New("leadstore: org already has a site")
)

// Org is one org row.
type Org struct {
	ID            int64     `db:"id"`
	Kind          string    `db:"kind"`
	Domain        string    `db:"domain"`
	Name          string    `db:"name"`
	Country       string    `db:"country"`
	SiteID        *int64    `db:"site_id"`
	Source        string    `db:"source"`
	SourceFetchID *int64    `db:"source_fetch_id"`
	CreatedAt     time.Time `db:"created_at"`
}

// insertOrg adds o unless an org already holds its domain. It returns the id of the org on
// that domain, its kind, and whether this call created it.
func (q Queries) insertOrg(ctx context.Context, o Org) (id int64, kind string, created bool, err error) {
	err = q.db.QueryRow(ctx,
		`INSERT INTO org (kind, domain, name, country, site_id, source, source_fetch_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (domain) DO NOTHING RETURNING id, kind`,
		o.Kind, o.Domain, o.Name, o.Country, o.SiteID, o.Source, o.SourceFetchID).Scan(&id, &kind)
	if err == nil {
		return id, kind, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, "", false, err
	}
	if err := q.db.QueryRow(ctx, `SELECT id, kind FROM org WHERE domain = $1`, o.Domain).Scan(&id, &kind); err != nil {
		return 0, "", false, err
	}
	return id, kind, false, nil
}

// UpsertSiteOrg makes sure a site_owner org exists for the Drupal probe probeID: one org
// per registrable domain of the probe's final host (the site's domain when the probe has
// none). The org takes the probe's site_name, the site's country and id, and the probe's
// homepage fetch (its newest site fetch). An org already on that domain, of any kind, is
// left as it is. It returns the org's id and whether this call created it.
func (q Queries) UpsertSiteOrg(ctx context.Context, probeID int64) (orgID int64, created bool, err error) {
	var (
		isDrupal            bool
		finalHost, siteName string
		siteDomain, country string
		siteID              int64
		homepageID          *int64
	)
	err = q.db.QueryRow(ctx,
		`SELECT p.is_drupal, p.final_host, p.site_name, s.id, s.domain, s.country,
		   (SELECT max(f.id) FROM http_fetch f WHERE f.id = ANY(p.fetch_ids) AND f.kind = 'site')
		 FROM site_probe p JOIN site s ON s.id = p.site_id WHERE p.id = $1`,
		probeID).Scan(&isDrupal, &finalHost, &siteName, &siteID, &siteDomain, &country, &homepageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("leadstore: upsert site org: probe %d: %w", probeID, ErrProbeNotFound)
	}
	if err != nil {
		return 0, false, fmt.Errorf("leadstore: upsert site org: %w", err)
	}
	if !isDrupal {
		return 0, false, fmt.Errorf("leadstore: upsert site org: probe %d: %w", probeID, ErrNotDrupalProbe)
	}
	host := finalHost
	if host == "" {
		host = siteDomain
	}
	domain, err := sitelist.Registrable(host)
	if err != nil {
		return 0, false, fmt.Errorf("leadstore: upsert site org: probe %d: %w: %w", probeID, ErrNoDomain, err)
	}
	id, _, created, err := q.insertOrg(ctx, Org{
		Kind: OrgSiteOwner, Domain: domain, Name: strings.TrimSpace(siteName), Country: country,
		SiteID: &siteID, Source: OrgSourceProbe, SourceFetchID: homepageID,
	})
	if err != nil {
		return 0, false, fmt.Errorf("leadstore: upsert site org: %w", err)
	}
	return id, created, nil
}

// AgencyImport is one agency from the agencies file.
type AgencyImport struct {
	Name    string
	Website string // the agency's site URL
	Country string // ISO 3166-1 alpha-2, or ''
	Domain  string // registrable domain; read from Website when blank
}

// AgencyUpsert is what UpsertAgency did.
type AgencyUpsert struct {
	OrgID   int64
	Created bool
	// Clash is true when the domain already belongs to an org of another kind, which is
	// left untouched; ExistingKind names that kind.
	Clash        bool
	ExistingKind string
}

// UpsertAgency makes sure an agency org exists for a, with fetchID as its source fetch
// (nil when none). There is one org per domain and the first kind wins: an org of another
// kind already on the domain is left untouched and reported as a clash, and an agency org
// already there is left as it is.
func (q Queries) UpsertAgency(ctx context.Context, a AgencyImport, fetchID *int64) (AgencyUpsert, error) {
	domain, err := agencyDomain(a)
	if err != nil {
		return AgencyUpsert{}, fmt.Errorf("leadstore: upsert agency %q: %w", a.Name, err)
	}
	id, kind, created, err := q.insertOrg(ctx, Org{
		Kind: OrgAgency, Domain: domain, Name: strings.TrimSpace(a.Name),
		Country: strings.ToUpper(strings.TrimSpace(a.Country)), Source: OrgSourceAgencies, SourceFetchID: fetchID,
	})
	if err != nil {
		return AgencyUpsert{}, fmt.Errorf("leadstore: upsert agency: %w", err)
	}
	r := AgencyUpsert{OrgID: id, Created: created}
	if kind != OrgAgency {
		r.Clash, r.ExistingKind = true, kind
	}
	return r, nil
}

// agencyDomain is a's registrable domain: its Domain, else its Website's host.
func agencyDomain(a AgencyImport) (string, error) {
	host := strings.TrimSpace(a.Domain)
	if host == "" {
		u, err := url.Parse(strings.TrimSpace(a.Website))
		if err != nil || u.Hostname() == "" {
			return "", ErrNoDomain
		}
		host = u.Hostname()
	}
	d, err := sitelist.Registrable(host)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoDomain, err)
	}
	return d, nil
}

// OrgSetSite points org orgID at site siteID, the org's one UPDATEd column. Setting the
// site it already has is a no-op; an org pointing at another site is refused.
func (q Queries) OrgSetSite(ctx context.Context, orgID, siteID int64) error {
	tag, err := q.db.Exec(ctx,
		`UPDATE org SET site_id = $2 WHERE id = $1 AND (site_id IS NULL OR site_id = $2)`, orgID, siteID)
	if err != nil {
		return fmt.Errorf("leadstore: org set site: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM org WHERE id = $1)`, orgID).Scan(&exists); err != nil {
		return fmt.Errorf("leadstore: org set site: %w", err)
	}
	if !exists {
		return fmt.Errorf("leadstore: org set site: org %d: %w", orgID, ErrOrgNotFound)
	}
	return fmt.Errorf("leadstore: org set site: org %d: %w", orgID, ErrOrgSiteSet)
}

// OrgByDomain returns the org on domain (matched lower case), or ErrOrgNotFound.
func (q Queries) OrgByDomain(ctx context.Context, domain string) (Org, error) {
	orgs, err := collect(ctx, q.db, pgx.RowToStructByName[Org],
		`SELECT id, kind, domain, name, country, site_id, source, source_fetch_id, created_at
		 FROM org WHERE domain = $1`, strings.ToLower(strings.TrimSpace(domain)))
	if err != nil {
		return Org{}, fmt.Errorf("leadstore: org by domain: %w", err)
	}
	if len(orgs) == 0 {
		return Org{}, fmt.Errorf("leadstore: org by domain %q: %w", domain, ErrOrgNotFound)
	}
	return orgs[0], nil
}
