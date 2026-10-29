package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/places"
)

// FindingSourcePlace is the finding source of a café with no website of its own: the place
// row itself (00021).
const FindingSourcePlace = "place"

// PlaceOrgPrefix starts the domain of an org made for a place with no website of its own
// ("overture:<source_id>"): such an org has no site, is never searched for routes, and its
// lead is reached on no channel until the operator adds one.
const PlaceOrgPrefix = "overture:"

var (
	// ErrBadRelease is returned by ImportPlaces for a release outside place_import_release_check.
	ErrBadRelease = errors.New("leadstore: release must be 1 to 40 letters, digits, '.', '_' or '-'")
	// ErrBadPlaceFile is returned by ImportPlaces for a file name that is not a plain base name.
	ErrBadPlaceFile = errors.New("leadstore: place file name must be a base name of at most 200 characters")
)

var (
	releaseRe   = regexp.MustCompile(`^[0-9A-Za-z._-]{1,40}$`)
	regionISORe = regexp.MustCompile(`^[A-Z]{2}-[A-Z0-9]{1,3}$`)
)

// PlaceImport is one import of a places file into a campaign.
type PlaceImport struct {
	CampaignID int64
	Release    string // Overture's release, e.g. 2026-10-21.0
	FileName   string // the file's base name; a path is cut to its base name
	Stats      places.Stats
	Actor      string // ActorGremy is stored as operator, ActorSystem as system
}

// PlaceRow is one kept place and its chain verdict ("" rule: an independent café).
type PlaceRow struct {
	Place       places.Place
	ChainRule   string // cafe.ChainBy*; "" when not a chain
	ChainDetail string
}

// PlaceImported is what ImportPlaces did.
type PlaceImported struct {
	ImportID int64
	Stored   int // place rows written
	Already  int // places this release already held (left as they were)
	Chains   int // of the stored, chain places (no org, no lead)
	Orgs     int // orgs made
	Sites    int // sites made (origin place), probed by cafe@1
	// Suppressed are the places not stored at all because an active domain suppression
	// covers their own website's registrable domain (`leads forget`, an opt-out): nothing is
	// gathered about such an org again, its place's phones and emails included. place_import
	// has no column for it: the run's note and the command's output carry it.
	Suppressed int
	// SuppressedNoSite are those of Suppressed with no website of their own: their org
	// ("overture:<source_id>") was suppressed, by `leads forget`.
	SuppressedNoSite int
}

// ImportPlaces stores one import of rows for im.CampaignID: for each place the release does
// not hold yet, an independent café gets its site (origin place, from its own website's
// registrable domain) and its org (source place, kind site_owner, with the place's
// country and ISO 3166-2 region), a chain gets neither; then the place_import row with
// im.Stats and these counts, then one place row each. A site or org already on that
// domain is reused as it is (its origin, source and region stay), so two places on one
// website share one org. A place whose own website's registrable domain an active domain
// suppression covers (any account's, as the collectors read it: orgDomainSuppressedSQL) is
// not stored at all, so a new release never brings back a forgotten org's place, phones or
// emails; it is counted in Suppressed. Run it in a transaction (leadsInTx) so an import
// lands whole.
func (q Queries) ImportPlaces(ctx context.Context, im PlaceImport, rows []PlaceRow) (PlaceImported, error) {
	var res PlaceImported
	if !releaseRe.MatchString(im.Release) {
		return res, ErrBadRelease
	}
	name := filepath.Base(filepath.ToSlash(im.FileName))
	if im.FileName == "" {
		name = ""
	}
	if len([]rune(name)) > 200 || strings.ContainsAny(name, "\r\n/\\") {
		return res, ErrBadPlaceFile
	}
	actor := "operator"
	if im.Actor == ActorSystem {
		actor = "system"
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.Place.SourceID
	}
	held, err := collect(ctx, q.db, pgx.RowTo[string],
		`SELECT source_id FROM place WHERE source = $1 AND release = $2 AND source_id = ANY($3)`,
		places.Source, im.Release, ids)
	if err != nil {
		return res, fmt.Errorf("leadstore: import places: %w", err)
	}
	have := make(map[string]bool, len(held))
	for _, id := range held {
		have[id] = true
	}
	suppressed, err := q.SuppressedPlaces(ctx, placesOf(rows))
	if err != nil {
		return res, fmt.Errorf("leadstore: import places: %w", err)
	}
	orgOf := make([]*int64, len(rows))
	todo := make([]bool, len(rows))
	for i, r := range rows {
		if have[r.Place.SourceID] {
			res.Already++
			continue
		}
		have[r.Place.SourceID] = true // a repeat in the file counts once
		if suppressed[r.Place.SourceID] {
			res.Suppressed++
			if strings.HasPrefix(placeOrgDomain(r.Place), PlaceOrgPrefix) {
				res.SuppressedNoSite++
			}
			continue
		}
		todo[i] = true
		if r.ChainRule != "" {
			res.Chains++
			continue
		}
		id, orgMade, siteMade, err := q.placeOrg(ctx, r.Place)
		if err != nil {
			return res, fmt.Errorf("leadstore: import places: %s: %w", r.Place.SourceID, err)
		}
		orgOf[i] = &id
		if orgMade {
			res.Orgs++
		}
		if siteMade {
			res.Sites++
		}
	}
	for _, t := range todo {
		if t {
			res.Stored++
		}
	}
	st := im.Stats
	malformed := st.MalformedLines
	if malformed == nil {
		malformed = []int{}
	}
	err = q.db.QueryRow(ctx,
		`INSERT INTO place_import (source, release, campaign_id, file_name, lines, kept, blank, malformed,
		   not_cafe, low_confidence, other_country, no_name, malformed_lines, stored, already, chains, orgs, sites, actor)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19) RETURNING id`,
		places.Source, im.Release, orCampaign(im.CampaignID), name, st.Lines, st.Kept, st.Blank, st.Malformed,
		st.NotCafe, st.LowConfidence, st.OtherCountry, st.NoName, malformed,
		res.Stored, res.Already, res.Chains, res.Orgs, res.Sites, actor).Scan(&res.ImportID)
	if err != nil {
		return res, fmt.Errorf("leadstore: import places: log: %w", err)
	}
	for i, r := range rows {
		if !todo[i] {
			continue
		}
		if err := q.insertPlace(ctx, res.ImportID, im.Release, r, orgOf[i]); err != nil {
			return res, fmt.Errorf("leadstore: import places: %s: %w", r.Place.SourceID, err)
		}
	}
	return res, nil
}

// SuppressedPlaces returns the source ids of ps that an active domain suppression of any
// account covers (orgDomainSuppressedSQL): their org's domain now (placeOrgDomain: an own
// website's registrable domain, or the "overture:<source_id>" org of a café with no website
// that `leads forget` suppresses), or the org any stored place with the same source id points
// at. So a café forgotten with no website stays forgotten when a later release lists a
// website for it, and the other way round. Nothing is gathered about such a place again.
func (q Queries) SuppressedPlaces(ctx context.Context, ps []places.Place) (map[string]bool, error) {
	ids, domains := make([]string, 0, len(ps)), make([]string, 0, len(ps))
	for _, p := range ps {
		ids, domains = append(ids, p.SourceID), append(domains, placeOrgDomain(p))
	}
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	hit, err := collect(ctx, q.db, pgx.RowTo[string],
		`SELECT DISTINCT x.source_id FROM unnest($1::text[], $2::text[]) AS x(source_id, domain)
		 WHERE `+orgDomainSuppressedSQL("x")+`
		    OR EXISTS (SELECT 1 FROM place sp JOIN org o ON o.id = sp.org_id
		      WHERE sp.source = $3 AND sp.source_id = x.source_id AND `+orgDomainSuppressedSQL("o")+`)`,
		ids, domains, places.Source)
	if err != nil {
		return nil, fmt.Errorf("leadstore: suppressed places: %w", err)
	}
	for _, id := range hit {
		out[id] = true
	}
	return out, nil
}

// placesOf is the places of rows.
func placesOf(rows []PlaceRow) []places.Place {
	out := make([]places.Place, len(rows))
	for i, r := range rows {
		out[i] = r.Place
	}
	return out
}

// ReleasePlaces returns every place stored for release (any campaign's import, chains
// included) with what the chain check reads of it: its source id, name, websites and brand.
// The chain index of an import and of a café build counts the whole release, so a chain
// split across export files is still a chain.
func (q Queries) ReleasePlaces(ctx context.Context, release string) ([]places.Place, error) {
	type row struct {
		SourceID      string   `db:"source_id"`
		Name          string   `db:"name"`
		Websites      []string `db:"websites"`
		Brand         string   `db:"brand"`
		BrandWikidata string   `db:"brand_wikidata"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT source_id, name, websites, brand, brand_wikidata FROM place
		 WHERE source = $1 AND release = $2 ORDER BY id`, places.Source, release)
	if err != nil {
		return nil, fmt.Errorf("leadstore: release places: %w", err)
	}
	out := make([]places.Place, len(rows))
	for i, r := range rows {
		out[i] = places.Place{SourceID: r.SourceID, Name: r.Name, Websites: r.Websites, Brand: r.Brand, BrandWikidata: r.BrandWikidata}
	}
	return out, nil
}

// placeOrg returns the org of an independent place, making its site and org when needed.
func (q Queries) placeOrg(ctx context.Context, p places.Place) (orgID int64, orgMade, siteMade bool, err error) {
	country := p.Address.Country
	region := p.Address.RegionISO()
	if !regionISORe.MatchString(region) {
		region = ""
	}
	domain := placeOrgDomain(p)
	var siteID *int64
	if !strings.HasPrefix(domain, PlaceOrgPrefix) {
		var id int64
		err = q.db.QueryRow(ctx,
			`INSERT INTO site (domain, country, origin) VALUES ($1, $2, 'place')
			 ON CONFLICT (domain) DO NOTHING RETURNING id`, domain, country).Scan(&id)
		switch {
		case err == nil:
			siteMade = true
		case errors.Is(err, pgx.ErrNoRows):
			if err = q.db.QueryRow(ctx, `SELECT id FROM site WHERE domain = $1`, domain).Scan(&id); err != nil {
				return 0, false, false, err
			}
		default:
			return 0, false, false, err
		}
		siteID = &id
	}
	err = q.db.QueryRow(ctx,
		`INSERT INTO org (kind, domain, name, country, region, site_id, source)
		 VALUES ('site_owner', $1, $2, $3, $4, $5, 'place')
		 ON CONFLICT (domain) DO NOTHING RETURNING id`, domain, p.Name, country, region, siteID).Scan(&orgID)
	switch {
	case err == nil:
		return orgID, true, siteMade, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, false, false, err
	}
	if err := q.db.QueryRow(ctx, `SELECT id FROM org WHERE domain = $1`, domain).Scan(&orgID); err != nil {
		return 0, false, false, err
	}
	return orgID, false, siteMade, nil
}

// placeOrgDomain is the domain of p's org: its own website's registrable domain, or, with no
// website of its own, PlaceOrgPrefix and its source id.
func placeOrgDomain(p places.Place) string {
	if d := places.DomainKey(p.OwnWebsite()); d != "" {
		return d
	}
	return PlaceOrgPrefix + strings.ToLower(p.SourceID)
}

func (q Queries) insertPlace(ctx context.Context, importID int64, release string, r PlaceRow, orgID *int64) error {
	p := r.Place
	js := func(v []string) string {
		if v == nil {
			v = []string{}
		}
		b, _ := json.Marshal(v) // a []string always marshals
		return string(b)
	}
	type src struct {
		Dataset  string `json:"dataset"`
		RecordID string `json:"record_id"`
	}
	sources := make([]src, 0, len(p.Sources))
	for _, s := range p.Sources {
		sources = append(sources, src(s))
	}
	sb, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	var lon, lat *float64
	if p.HasPoint {
		lon, lat = &p.Lon, &p.Lat
	}
	region := p.Address.RegionISO()
	if !regionISORe.MatchString(region) {
		region = ""
	}
	_, err = q.db.Exec(ctx,
		`INSERT INTO place (import_id, source, source_id, release, name, category, alternates, country, region,
		   region_iso, locality, postcode, freeform, lon, lat, websites, phones, socials, emails, brand,
		   brand_wikidata, sources, confidence, chain_rule, chain_detail, org_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb, $17::jsonb,
		   $18::jsonb, $19::jsonb, $20, $21, $22::jsonb, $23, $24, $25, $26)
		 ON CONFLICT (source, source_id, release) DO NOTHING`,
		importID, places.Source, p.SourceID, release, p.Name, p.Category, js(p.Alternates), p.Address.Country,
		p.Address.Region, region, p.Address.Locality, p.Address.Postcode, p.Address.Freeform, lon, lat,
		js(p.Websites), js(p.Phones), js(p.Socials), js(p.Emails), p.Brand, p.BrandWikidata, string(sb),
		p.Confidence, r.ChainRule, r.ChainDetail, orgID)
	return err
}

// CafeBuildOrg is one org a café campaign's build classifies: an org a place of the
// campaign's imports points at, with its latest place, its site's latest probe and its lead
// in the campaign.
type CafeBuildOrg struct {
	OrgID   int64  `db:"org_id"`
	Kind    string `db:"kind"`
	Domain  string `db:"domain"`
	Country string `db:"country"`
	SiteID  *int64 `db:"site_id"`
	// PlaceID is the org's latest place row in the campaign's imports: of its own places, or
	// a later row of one of them stored as a chain (a chain row points at no org).
	PlaceID          int64     `db:"place_id"`
	PlaceRelease     string    `db:"place_release"`
	PlaceSourceID    string    `db:"place_source_id"`
	PlaceChainRule   string    `db:"place_chain_rule"` // "" when that row is not a chain
	PlaceChainDetail string    `db:"place_chain_detail"`
	ProbeID          *int64    `db:"probe_id"`
	ProbeOutcome     string    `db:"probe_outcome"` // "" without a probe
	DerivedBy        string    `db:"derived_by"`    // the probe's detector; "" without a probe
	Signals          []Signal  `db:"signals"`
	ProbedAt         time.Time `db:"probed_at"` // zero without a probe
	LeadID           *int64    `db:"lead_id"`
	LeadState        string    `db:"lead_state"`
	LeadKind         string    `db:"lead_kind"`
	LeadChannel      string    `db:"lead_channel"`
}

// CafeOrgsForBuild returns every org that a place imported into campaign campaignID points
// at (chains point at none), by org id, with its latest place row in the campaign's imports:
// the latest of its own place rows and of the chain rows a later import stored for one of
// those places (same source id). A chain there (PlaceChainRule) makes no lead; the build also
// checks that row against its whole release's chain index (a chain split across files).
func (q Queries) CafeOrgsForBuild(ctx context.Context, campaignID int64) ([]CafeBuildOrg, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[CafeBuildOrg],
		`SELECT o.id AS org_id, o.kind, o.domain, o.country, o.site_id, pl.id AS place_id,
		   pl.release AS place_release, pl.source_id AS place_source_id,
		   pl.chain_rule AS place_chain_rule, pl.chain_detail AS place_chain_detail,
		   p.id AS probe_id, coalesce(p.outcome, '') AS probe_outcome, coalesce(p.derived_by, '') AS derived_by,
		   coalesce(p.signals, '[]'::jsonb) AS signals, coalesce(p.created_at, 'epoch'::timestamptz) AS probed_at,
		   l.id AS lead_id, coalesce(l.state, '') AS lead_state, coalesce(l.kind, '') AS lead_kind,
		   coalesce(l.channel, '') AS lead_channel
		 FROM org o
		 JOIN LATERAL (
		   SELECT x.id, x.release, x.source_id, x.chain_rule, x.chain_detail
		   FROM place x JOIN place_import i ON i.id = x.import_id
		   WHERE i.campaign_id = $1 AND (x.org_id = o.id OR x.org_id IS NULL)
		     AND x.source = $2 AND x.source_id IN (
		       SELECT y.source_id FROM place y JOIN place_import yi ON yi.id = y.import_id
		       WHERE y.org_id = o.id AND yi.campaign_id = $1)
		   ORDER BY x.id DESC LIMIT 1
		 ) pl ON true
		 LEFT JOIN site s ON s.id = o.site_id
		 LEFT JOIN site_probe p ON p.id = s.last_probe_id
		 LEFT JOIN lead l ON l.org_id = o.id AND l.campaign_id = $1
		 WHERE o.kind <> 'selftest'
		 ORDER BY o.id`, orCampaign(campaignID), places.Source)
	if err != nil {
		return nil, fmt.Errorf("leadstore: café orgs for build: %w", err)
	}
	for i := range rows {
		if rows[i].ProbeID == nil {
			rows[i].ProbedAt = time.Time{}
		}
	}
	return rows, nil
}

// CafeFinding is the finding a café lead rests on: its kind, the Decision's plain reason and
// evidence, and the probe it was read from (nil for a café with no website: the place).
type CafeFinding struct {
	OrgID    int64
	Kind     string
	ProbeID  *int64 // the probe whose signals Classify read; nil: the place itself
	PlaceID  int64
	Detector string          // cafe@1
	Subject  map[string]any  // {reason, language, language_via, language_note, website}
	Evidence []FindingDetail // one per signal
}

// FindingDetail is one piece of a finding's evidence ({name, value, fetch_id}).
type FindingDetail struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	FetchID int64  `json:"fetch_id,omitempty"`
}

// EnsureCafeFinding returns the finding of f's source row (its probe, else its place),
// inserting it with f's kind, subject and evidence when none exists. UpsertLead and
// RefreshLead find it by the same source row, so a café lead points at it.
func (q Queries) EnsureCafeFinding(ctx context.Context, f CafeFinding) (int64, error) {
	table, sourceID := FindingSourcePlace, f.PlaceID
	if f.ProbeID != nil {
		table, sourceID = FindingSourceProbe, *f.ProbeID
	}
	subj := f.Subject
	if subj == nil {
		subj = map[string]any{}
	}
	sb, err := json.Marshal(subj)
	if err != nil {
		return 0, fmt.Errorf("leadstore: café finding: %w", err)
	}
	ev := f.Evidence
	if ev == nil {
		ev = []FindingDetail{}
	}
	eb, err := json.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("leadstore: café finding: %w", err)
	}
	var id int64
	err = q.db.QueryRow(ctx,
		`INSERT INTO finding (org_id, kind, detector, subject, evidence, source_table, source_id, observed_at)
		 SELECT $1, $2, $3, $4::jsonb, $5::jsonb, $6::text, $7::bigint,
		   coalesce((SELECT created_at FROM site_probe WHERE id = $7::bigint AND $6::text = 'site_probe'),
		            (SELECT imported_at FROM place WHERE id = $8::bigint), now())
		 ON CONFLICT (source_table, source_id) DO NOTHING RETURNING id`,
		f.OrgID, f.Kind, f.Detector, string(sb), string(eb), table, sourceID, f.PlaceID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("leadstore: café finding: %w", err)
	}
	if err := q.db.QueryRow(ctx, `SELECT id FROM finding WHERE source_table = $1 AND source_id = $2`, table, sourceID).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: café finding: %w", err)
	}
	return id, nil
}

// PlaceImportRow is one place_import row, for `leads places imports` and tests.
type PlaceImportRow struct {
	ID             int64     `db:"id"`
	Release        string    `db:"release"`
	CampaignID     int64     `db:"campaign_id"`
	FileName       string    `db:"file_name"`
	Lines          int       `db:"lines"`
	Kept           int       `db:"kept"`
	Malformed      int       `db:"malformed"`
	MalformedLines []int32   `db:"malformed_lines"`
	Stored         int       `db:"stored"`
	Already        int       `db:"already"`
	Chains         int       `db:"chains"`
	Orgs           int       `db:"orgs"`
	Sites          int       `db:"sites"`
	At             time.Time `db:"at"`
}

// PlaceImports returns campaign campaignID's place imports, newest first.
func (q Queries) PlaceImports(ctx context.Context, campaignID int64) ([]PlaceImportRow, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[PlaceImportRow],
		`SELECT id, release, campaign_id, file_name, lines, kept, malformed, malformed_lines, stored, already,
		   chains, orgs, sites, at
		 FROM place_import WHERE campaign_id = $1 ORDER BY id DESC`, orCampaign(campaignID))
	if err != nil {
		return nil, fmt.Errorf("leadstore: place imports: %w", err)
	}
	return rows, nil
}
