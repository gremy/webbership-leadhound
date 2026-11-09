package leadstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrBadBusinessKind is returned by SetBusinessKind for a hash that is not 32 bytes, or for a
// selftest lead, which never holds a kind.
var ErrBadBusinessKind = errors.New("leadstore: not a valid business kind answer")

// SetBusinessKind appends Jev's answer on what lead leadID's business is: kind from the closed
// list of migration 00034, question its versioned name, sha the SHA-256 of what Jev was asked
// and confidence Jev's. The row is written whatever the lead's state, and it never touches the
// lead's call or state. The table's checks refuse a kind off the list, a malformed question and
// a confidence outside 0 to 1. A lead that does not exist is ErrLeadNotFound.
func (q Queries) SetBusinessKind(ctx context.Context, leadID int64, kind, question string, sha []byte, confidence float64) error {
	if len(sha) != 32 {
		return fmt.Errorf("leadstore: set business kind %d: %w: a %d-byte hash", leadID, ErrBadBusinessKind, len(sha))
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO lead_business_kind (lead_id, kind, confidence, question, input_sha256)
		 SELECT l.id, $2, $3, $4, $5 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.id = $1 AND l.kind <> 'selftest' AND o.kind <> 'selftest'`,
		leadID, kind, confidence, question, sha)
	if err != nil {
		return fmt.Errorf("leadstore: set business kind %d: %w", leadID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lead WHERE id = $1)`, leadID).Scan(&exists); err != nil {
		return fmt.Errorf("leadstore: set business kind %d: %w", leadID, err)
	}
	if !exists {
		return fmt.Errorf("leadstore: set business kind %d: %w", leadID, ErrLeadNotFound)
	}
	return fmt.Errorf("leadstore: set business kind %d: %w: a selftest lead", leadID, ErrBadBusinessKind)
}

// BusinessKindLead is an open lead with the input hash of its latest business-kind answer.
type BusinessKindLead struct {
	LeadID int64 `db:"lead_id"`
	// LatestSHA is the input_sha256 of the lead's latest lead_business_kind row; nil when Jev
	// was never asked about it.
	LatestSHA []byte `db:"latest_sha"`
}

// BusinessKindLeads reads the open leads (state new, never a selftest one) of campaign
// campaignID, 0 for every campaign's, each with its latest business-kind hash, in lead id order.
// The latest row is read by lead_business_kind_lead_idx.
func (q Queries) BusinessKindLeads(ctx context.Context, campaignID int64) ([]BusinessKindLead, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[BusinessKindLead],
		`SELECT l.id AS lead_id,
		        (SELECT b.input_sha256 FROM lead_business_kind b
		          WHERE b.lead_id = l.id ORDER BY b.id DESC LIMIT 1) AS latest_sha
		 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.state = 'new' AND l.kind <> 'selftest' AND o.kind <> 'selftest'
		   AND ($1::bigint = 0 OR l.campaign_id = $1::bigint)
		 ORDER BY l.id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: business kind leads: %w", err)
	}
	return rows, nil
}
