package leadstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LeadPlace is what the map listing says of a lead's place: its name, category and address
// and the websites it lists. Phones and emails stay out (they reach a lead as routes).
type LeadPlace struct {
	Name     string   `db:"name"`
	Category string   `db:"category"`
	Country  string   `db:"country"`
	Region   string   `db:"region"`
	Locality string   `db:"locality"`
	Postcode string   `db:"postcode"`
	Freeform string   `db:"freeform"`
	Websites []string `db:"websites"`
}

// LeadPlace returns the latest place row of org orgID among the places imported into
// campaign campaignID; ok is false when that campaign's imports hold none for the org.
func (q Queries) LeadPlace(ctx context.Context, orgID, campaignID int64) (LeadPlace, bool, error) {
	rows, err := q.db.Query(ctx,
		`SELECT p.name, p.category, p.country, p.region, p.locality, p.postcode, p.freeform, p.websites
		 FROM place p JOIN place_import i ON i.id = p.import_id
		 WHERE p.org_id = $1 AND i.campaign_id = $2
		 ORDER BY p.id DESC LIMIT 1`, orgID, orCampaign(campaignID))
	if err != nil {
		return LeadPlace{}, false, fmt.Errorf("leadstore: lead place: %w", err)
	}
	pl, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[LeadPlace])
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadPlace{}, false, nil
	}
	if err != nil {
		return LeadPlace{}, false, fmt.Errorf("leadstore: lead place: %w", err)
	}
	return pl, true, nil
}
