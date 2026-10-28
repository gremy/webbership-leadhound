package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/store"
)

// The origins a site row can have (site_origin_check).
const (
	OriginMajestic    = "majestic"
	OriginTranco      = "tranco"
	OriginHTTPArchive = "httparchive"
	OriginAgency      = "agency"
	OriginManual      = "manual"
	OriginPlace       = "place" // added by a place import (00021); probed with cafe@1
)

// The states a site row can be in (site_state_check).
const (
	SiteNew     = "new"
	SiteProbed  = "probed"
	SiteRetired = "retired"
)

// The outcomes of one probe (site_probe_outcome_check).
const (
	OutcomeOK               = "ok"
	OutcomeBotCheck         = "bot_check"
	OutcomeRobotsDisallowed = "robots_disallowed"
	OutcomeNotHTML          = "not_html"
	OutcomeHTTPError        = "http_error"
	OutcomeTransportError   = "transport_error"
)

// SiteOrigins are the valid site origins.
var SiteOrigins = []string{OriginMajestic, OriginTranco, OriginHTTPArchive, OriginAgency, OriginManual}

var (
	// ErrUnknownOrigin is returned by ImportSites for an origin outside SiteOrigins.
	ErrUnknownOrigin = errors.New("leadstore: unknown site origin")
	// ErrEmptyDomain is returned by ImportSites for a row whose domain is blank.
	ErrEmptyDomain = errors.New("leadstore: empty site domain")
	// ErrSiteNotFound is returned by RecordProbe when no site row has that id.
	ErrSiteNotFound = errors.New("leadstore: site not found")
)

// siteImportChunk is how many rows one INSERT carries.
const siteImportChunk = 5000

// SiteImport is one site to add to the site list.
type SiteImport struct {
	Domain  string // registrable domain; stored trimmed and lower case
	Country string // ISO 3166-1 alpha-2, or '' when generic
	Rank    *int   // the list rank, nil when unknown
}

// ImportTally is what one ImportSites did: rows added, and rows already known (repeats in
// the input and rows whose rank was filled included).
type ImportTally struct {
	Added int
	Known int
}

// ImportSites adds rows to the site list with origin; countries are stored upper case. A
// site already there keeps its origin, country and state. It fills a NULL list_rank from
// the row's rank, the one column besides the state pointers that it UPDATEs. Domains
// repeated in the input count once, as known; the first non-nil rank among the repeats is
// the one used.
func (q Queries) ImportSites(ctx context.Context, origin string, rows []SiteImport) (ImportTally, error) {
	var t ImportTally
	if !slices.Contains(SiteOrigins, origin) {
		return t, fmt.Errorf("%w: %q", ErrUnknownOrigin, origin)
	}
	index := map[string]int{}
	var domains, countries []string
	var ranks []*int
	for _, r := range rows {
		d := strings.ToLower(strings.TrimSpace(r.Domain))
		if d == "" {
			return t, ErrEmptyDomain
		}
		if i, ok := index[d]; ok {
			t.Known++
			if ranks[i] == nil {
				ranks[i] = r.Rank
			}
			continue
		}
		index[d] = len(domains)
		domains = append(domains, d)
		countries = append(countries, strings.ToUpper(strings.TrimSpace(r.Country)))
		ranks = append(ranks, r.Rank)
	}
	for start := 0; start < len(domains); start += siteImportChunk {
		end := min(start+siteImportChunk, len(domains))
		n, err := q.importSiteChunk(ctx, origin, domains[start:end], countries[start:end], ranks[start:end])
		if err != nil {
			return t, err
		}
		t.Added += n
		t.Known += end - start - n
	}
	return t, nil
}

func (q Queries) importSiteChunk(ctx context.Context, origin string, domains, countries []string, ranks []*int) (int, error) {
	// A conflicting row is updated only to fill a NULL rank; any other existing row is left
	// untouched and not returned. xmax = 0 marks a row this statement inserted.
	const sql = `INSERT INTO site (domain, country, origin, list_rank)
		SELECT d, c, $4, r FROM unnest($1::text[], $2::text[], $3::int[]) AS t(d, c, r)
		ON CONFLICT (domain) DO UPDATE SET list_rank = EXCLUDED.list_rank
		  WHERE site.list_rank IS NULL AND EXCLUDED.list_rank IS NOT NULL
		RETURNING xmax = 0 AS inserted`
	inserted, err := collect(ctx, q.db, pgx.RowTo[bool], sql, domains, countries, ranks, origin)
	if err != nil {
		return 0, fmt.Errorf("leadstore: import sites: %w", err)
	}
	added := 0
	for _, ins := range inserted {
		if ins {
			added++
		}
	}
	return added, nil
}

// collect runs sql and collects every row with fn.
func collect[T any](ctx context.Context, db store.DBTX, fn pgx.RowToFunc[T], sql string, args ...any) ([]T, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, fn)
}

// Site is one site row.
type Site struct {
	ID          int64     `db:"id"`
	Domain      string    `db:"domain"`
	Country     string    `db:"country"`
	Origin      string    `db:"origin"`
	ListRank    *int      `db:"list_rank"`
	AddedAt     time.Time `db:"added_at"`
	State       string    `db:"state"`
	LastProbeID *int64    `db:"last_probe_id"`
}

// DueSites returns up to limit sites to probe (limit <= 0: all of them): every new site,
// and every probed site whose last probe is older than reprobeAfter at now. Retired sites
// are never due. They come by list rank, unranked last, then by id.
func (q Queries) DueSites(ctx context.Context, limit int, reprobeAfter time.Duration, now time.Time) ([]Site, error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	const sql = `SELECT s.id, s.domain, s.country, s.origin, s.list_rank, s.added_at, s.state, s.last_probe_id
		FROM site s LEFT JOIN site_probe p ON p.id = s.last_probe_id
		WHERE s.state = 'new' OR (s.state = 'probed' AND p.created_at < $1)
		ORDER BY s.list_rank NULLS LAST, s.id
		LIMIT $2`
	sites, err := collect(ctx, q.db, pgx.RowToStructByName[Site], sql, now.Add(-reprobeAfter), lim)
	if err != nil {
		return nil, fmt.Errorf("leadstore: due sites: %w", err)
	}
	return sites, nil
}

// Signal is one piece of evidence a probe saw, and the fetch it came from.
type Signal struct {
	Signal  string `json:"signal"`
	Value   string `json:"value"`
	FetchID int64  `json:"fetch_id"`
}

// ProbeRow is one site_probe row to write.
type ProbeRow struct {
	Outcome      string
	FinalHost    string
	IsDrupal     bool
	Major        int // 0 = unknown or not Drupal
	ExactVersion string
	Ambiguous    string // e.g. "10-or-11"
	Signals      []Signal
	FetchIDs     []int64
	SiteName     string
	DerivedBy    string // e.g. "drupal@1"
}

// RecordProbe writes one probe of siteID for runID and points the site at it (last_probe_id,
// and state probed unless the site is retired). It returns the probe's id. The two
// statements run on the caller's DBTX, so a caller that needs them atomic passes a
// transaction.
func (q Queries) RecordProbe(ctx context.Context, siteID, runID int64, p ProbeRow) (int64, error) {
	signals := p.Signals
	if signals == nil {
		signals = []Signal{}
	}
	sig, err := json.Marshal(signals)
	if err != nil {
		return 0, fmt.Errorf("leadstore: record probe: signals: %w", err)
	}
	fetchIDs := p.FetchIDs
	if fetchIDs == nil {
		fetchIDs = []int64{} // NOT NULL: a nil slice would be sent as NULL
	}
	var id int64
	err = q.db.QueryRow(ctx,
		`INSERT INTO site_probe (site_id, run_id, outcome, final_host, is_drupal, major, exact_version, ambiguous, signals, fetch_ids, site_name, derived_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12) RETURNING id`,
		siteID, runID, p.Outcome, p.FinalHost, p.IsDrupal, p.Major, p.ExactVersion, p.Ambiguous, string(sig), fetchIDs, p.SiteName, p.DerivedBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("leadstore: record probe: %w", err)
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE site SET last_probe_id = $1, state = CASE WHEN state = 'retired' THEN state ELSE 'probed' END WHERE id = $2`,
		id, siteID)
	if err != nil {
		return 0, fmt.Errorf("leadstore: record probe: point site: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return 0, fmt.Errorf("leadstore: record probe: site %d: %w", siteID, ErrSiteNotFound)
	}
	return id, nil
}

// InsertFetchHeader keeps h, the allowlisted response headers of fetch fetchID, as jsonb
// with lower-case names ({"x-generator": ["…"]}). A fetch that already has its headers
// keeps the first ones.
func (q Queries) InsertFetchHeader(ctx context.Context, fetchID int64, h http.Header) error {
	lower := make(map[string][]string, len(h))
	for k, v := range h {
		k = strings.ToLower(k)
		lower[k] = append(lower[k], v...)
	}
	b, err := json.Marshal(lower)
	if err != nil {
		return fmt.Errorf("leadstore: insert fetch header: %w", err)
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO fetch_header (fetch_id, headers) VALUES ($1, $2::jsonb) ON CONFLICT (fetch_id) DO NOTHING`,
		fetchID, string(b)); err != nil {
		return fmt.Errorf("leadstore: insert fetch header: %w", err)
	}
	return nil
}

// ProbeCounts counts the probes written in a window.
type ProbeCounts struct {
	Total     int
	Drupal    int            // probes that found Drupal
	ByOutcome map[string]int // outcome → probes
	ByMajor   map[int]int    // Drupal major → probes, Drupal probes only (0 = major unknown)
}

// ProbeSummary counts the probes created at or after since, by outcome and, for Drupal
// sites, by major version.
func (q Queries) ProbeSummary(ctx context.Context, since time.Time) (ProbeCounts, error) {
	type group struct {
		Outcome  string `db:"outcome"`
		IsDrupal bool   `db:"is_drupal"`
		Major    int    `db:"major"`
		N        int    `db:"n"`
	}
	groups, err := collect(ctx, q.db, pgx.RowToStructByName[group],
		`SELECT outcome, is_drupal, major, count(*)::int AS n FROM site_probe
		 WHERE created_at >= $1 GROUP BY outcome, is_drupal, major`, since)
	if err != nil {
		return ProbeCounts{}, fmt.Errorf("leadstore: probe summary: %w", err)
	}
	c := ProbeCounts{ByOutcome: map[string]int{}, ByMajor: map[int]int{}}
	for _, g := range groups {
		c.Total += g.N
		c.ByOutcome[g.Outcome] += g.N
		if g.IsDrupal {
			c.Drupal += g.N
			c.ByMajor[g.Major] += g.N
		}
	}
	return c, nil
}

// EvalRow is the latest probe of one site, for comparing the detector with a hand-checked
// list.
type EvalRow struct {
	Domain       string   `db:"domain"`
	Outcome      string   `db:"outcome"`
	IsDrupal     bool     `db:"is_drupal"`
	Major        int      `db:"major"`
	ExactVersion string   `db:"exact_version"`
	Ambiguous    string   `db:"ambiguous"`
	Signals      []Signal `db:"signals"`
}

// EvalRows returns the latest probe of each listed domain (matched lower case), ordered by
// domain. A domain with no site row or no probe is left out.
func (q Queries) EvalRows(ctx context.Context, domains []string) ([]EvalRow, error) {
	lower := make([]string, len(domains))
	for i, d := range domains {
		lower[i] = strings.ToLower(strings.TrimSpace(d))
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[EvalRow],
		`SELECT DISTINCT ON (s.domain) s.domain, p.outcome, p.is_drupal, p.major, p.exact_version, p.ambiguous, p.signals
		 FROM site s JOIN site_probe p ON p.site_id = s.id
		 WHERE s.domain = ANY($1)
		 ORDER BY s.domain, p.created_at DESC, p.id DESC`, lower)
	if err != nil {
		return nil, fmt.Errorf("leadstore: eval rows: %w", err)
	}
	return rows, nil
}
