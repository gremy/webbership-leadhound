package leadstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// JevPlaceTypeLead is an open café lead with the input hash of its latest place-type answer
// from Jev.
type JevPlaceTypeLead struct {
	LeadID int64 `db:"lead_id"`
	// LatestSHA is the input_sha256 of the lead's latest Jev lead_place_type row; nil when
	// Jev was never asked about it.
	LatestSHA []byte `db:"latest_sha"`
}

// JevPlaceTypeLeads reads the open café leads (state new, never a selftest one) of campaign
// campaignID, 0 for every campaign's, each with its latest Jev place-type hash, in lead id
// order. The latest row is read by lead_place_type_lead_idx; an operator's or a name's row
// never counts here.
func (q Queries) JevPlaceTypeLeads(ctx context.Context, campaignID int64) ([]JevPlaceTypeLead, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[JevPlaceTypeLead],
		`SELECT l.id AS lead_id,
		        (SELECT t.input_sha256 FROM lead_place_type t
		          WHERE t.lead_id = l.id AND t.source = '`+PlaceFromJev+`'
		          ORDER BY t.id DESC LIMIT 1) AS latest_sha
		 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.state = 'new' AND l.kind IN (`+cafeKindsSQL()+`) AND o.kind <> 'selftest'
		   AND ($1::bigint = 0 OR l.campaign_id = $1::bigint)
		 ORDER BY l.id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: jev place-type leads: %w", err)
	}
	return rows, nil
}
