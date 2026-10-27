package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/store"
)

// The source tables a finding is read from (finding.source_table) today; a later source
// names its own.
const (
	FindingSourceProbe  = "site_probe"
	FindingSourceAgency = "agency_finding"
)

// Finding is one finding row (migration 00010): what a detector saw that makes an org a
// lead, and the source row it was read from.
type Finding struct {
	ID       int64           `db:"id"`
	OrgID    int64           `db:"org_id"`
	Kind     string          `db:"kind"`     // a lead_kind name
	Detector string          `db:"detector"` // e.g. "drupal@1"
	Subject  json.RawMessage `db:"subject"`  // an object: {major, exact_version, ambiguous} or {url, phrase}
	Evidence json.RawMessage `db:"evidence"` // an array of {name, value, fetch_id}
	// SourceTable and SourceID name the source row (FindingSourceProbe, FindingSourceAgency);
	// both nil for a finding with no source row.
	SourceTable *string   `db:"source_table"`
	SourceID    *int64    `db:"source_id"`
	ObservedAt  time.Time `db:"observed_at"`
	CreatedAt   time.Time `db:"created_at"`
}

const findingColumns = `f.id, f.org_id, f.kind, f.detector, f.subject, f.evidence, f.source_table, f.source_id, f.observed_at, f.created_at`

// FindingFor returns lead leadID's own finding; ok is false for a lead without one (a
// selftest or hand lead). A missing lead is ErrLeadNotFound.
func (q Queries) FindingFor(ctx context.Context, leadID int64) (f Finding, ok bool, err error) {
	type row struct {
		Finding
		Has bool `db:"has"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT f.id IS NOT NULL AS has, coalesce(f.id, 0) AS id, coalesce(f.org_id, l.org_id) AS org_id,
		   coalesce(f.kind, '') AS kind, coalesce(f.detector, '') AS detector,
		   coalesce(f.subject, '{}') AS subject, coalesce(f.evidence, '[]') AS evidence,
		   f.source_table, f.source_id, coalesce(f.observed_at, l.created_at) AS observed_at,
		   coalesce(f.created_at, l.created_at) AS created_at
		 FROM lead l LEFT JOIN finding f ON f.id = l.finding_id WHERE l.id = $1`, leadID)
	if err != nil {
		return Finding{}, false, fmt.Errorf("leadstore: finding for lead: %w", err)
	}
	if len(rows) == 0 {
		return Finding{}, false, fmt.Errorf("leadstore: finding for lead %d: %w", leadID, ErrLeadNotFound)
	}
	if !rows[0].Has {
		return Finding{}, false, nil
	}
	return rows[0].Finding, true, nil
}

// FindingsFor returns every finding of org orgID, newest first: the lead's own and any it
// pointed at before its probe changed.
func (q Queries) FindingsFor(ctx context.Context, orgID int64) ([]Finding, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[Finding],
		`SELECT `+findingColumns+` FROM finding f WHERE f.org_id = $1 ORDER BY f.id DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: findings for org: %w", err)
	}
	return rows, nil
}

// EnsureFinding returns the finding l's probe or agency finding gives (looked up by its
// source row, inserted for l.OrgID and l.Kind when none exists), or nil when l points at
// neither. A caller that inserts lead rows itself (leads transfer) sets lead.finding_id
// from it.
func (q Queries) EnsureFinding(ctx context.Context, l LeadRow) (*int64, error) {
	id, err := findingFor(ctx, q.db, l)
	if err != nil {
		return nil, fmt.Errorf("leadstore: ensure finding: %w", err)
	}
	return id, nil
}

// findingSource is the source row l's finding is read from: its probe, else its agency
// finding. ok is false when l points at neither.
func findingSource(l LeadRow) (table string, id int64, ok bool) {
	switch {
	case l.ProbeID != nil:
		return FindingSourceProbe, *l.ProbeID, true
	case l.AgencyFindingID != nil:
		return FindingSourceAgency, *l.AgencyFindingID, true
	}
	return "", 0, false
}

// The finding inserts, one per source table, with 00010's backfill shapes: a probe's subject
// is its version, its evidence its signals ({signal} renamed {name}); an agency finding's
// subject is its page and words, its evidence the matched words. $1 org, $2 kind, $3 source id.
const (
	insertProbeFindingSQL = `INSERT INTO finding (org_id, kind, detector, subject, evidence, source_table, source_id, observed_at)
		SELECT $1, $2, p.derived_by,
		       jsonb_build_object('major', p.major, 'exact_version', p.exact_version, 'ambiguous', p.ambiguous),
		       coalesce((SELECT jsonb_agg(jsonb_build_object('name', e.v->>'signal', 'value', e.v->'value', 'fetch_id', e.v->'fetch_id') ORDER BY e.i)
		                 FROM jsonb_array_elements(p.signals) WITH ORDINALITY AS e(v, i)), '[]'::jsonb),
		       'site_probe', p.id, p.created_at
		FROM site_probe p WHERE p.id = $3
		ON CONFLICT (source_table, source_id) DO NOTHING RETURNING id`
	insertAgencyFindingSQL = `INSERT INTO finding (org_id, kind, detector, subject, evidence, source_table, source_id, observed_at)
		SELECT $1, $2, af.derived_by,
		       jsonb_build_object('url', af.url, 'phrase', af.phrase),
		       jsonb_build_array(jsonb_build_object('name', 'phrase', 'value', af.phrase, 'fetch_id', af.fetch_id)),
		       'agency_finding', af.id, af.created_at
		FROM agency_finding af WHERE af.id = $3
		ON CONFLICT (source_table, source_id) DO NOTHING RETURNING id`
)

// findingFor looks up the finding of l's source row, or inserts it, and returns its id; nil
// when l points at no source row, or the row does not exist (the lead insert then fails on
// its own foreign key). A finding made by a concurrent build is found by the second lookup.
func findingFor(ctx context.Context, db store.DBTX, l LeadRow) (*int64, error) {
	table, sourceID, ok := findingSource(l)
	if !ok {
		return nil, nil
	}
	lookup := func() (*int64, error) {
		var id int64
		err := db.QueryRow(ctx, `SELECT id FROM finding WHERE source_table = $1 AND source_id = $2`, table, sourceID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &id, nil
	}
	if id, err := lookup(); err != nil || id != nil {
		return id, err
	}
	insert := insertProbeFindingSQL
	if table == FindingSourceAgency {
		insert = insertAgencyFindingSQL
	}
	var id int64
	err := db.QueryRow(ctx, insert, l.OrgID, l.Kind, sourceID).Scan(&id)
	if err == nil {
		return &id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return lookup()
}
