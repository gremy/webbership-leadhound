package leadstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CafeLeadName is a café lead as the name words read it: the two names to look in, where its
// place type stands now, and the words its name rows already hold.
type CafeLeadName struct {
	LeadID int64 `db:"lead_id"`
	// OrgName is the org's name, which for a café is its map listing's name.
	OrgName string `db:"org_name"`
	// SiteName is the site's own name from the lead's probe; empty without one.
	SiteName  string   `db:"site_name"`
	PlaceType string   `db:"place_type"` // the effective type (effectivePlaceTypeSQL)
	Source    string   `db:"source"`     // the winning row's source; empty when no source spoke
	NameWords []string `db:"name_words"` // the detail of every name row of the lead
}

// cafeLeadNameSQL is every café lead, whatever its state and never a selftest one, as
// CafeLeadName's columns. Callers add to its WHERE clause.
var cafeLeadNameSQL = `SELECT l.id AS lead_id, coalesce(o.name, '') AS org_name, coalesce(p.site_name, '') AS site_name,
	   ` + effectivePlaceTypeSQL + ` AS place_type, coalesce(w.source, '') AS source,
	   ARRAY(SELECT DISTINCT t.detail FROM lead_place_type t
	         WHERE t.lead_id = l.id AND t.source = ` + sqlLiteral(PlaceFromName) + `) AS name_words
	 FROM lead l
	 JOIN org o ON o.id = l.org_id
	 LEFT JOIN site_probe p ON p.id = l.probe_id
	 LEFT JOIN LATERAL (` + placeTypeWinnerSQL + `) w ON true
	 WHERE l.kind IN (` + cafeKindsSQL() + `) AND o.kind <> 'selftest'`

// CafeLeadNames reads the café leads of campaign campaignID, by lead id; 0 reads every
// campaign's. A lead whose rating type is not café is never among them.
func (q Queries) CafeLeadNames(ctx context.Context, campaignID int64) ([]CafeLeadName, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[CafeLeadName],
		cafeLeadNameSQL+` AND ($1::bigint = 0 OR l.campaign_id = $1::bigint) ORDER BY l.id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: café lead names: %w", err)
	}
	return rows, nil
}

// CafeLeadNamesByID reads those of the leads in ids that are café leads, by lead id.
func (q Queries) CafeLeadNamesByID(ctx context.Context, ids []int64) ([]CafeLeadName, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[CafeLeadName],
		cafeLeadNameSQL+` AND l.id = ANY($1::bigint[]) ORDER BY l.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("leadstore: café lead names by id: %w", err)
	}
	return rows, nil
}
