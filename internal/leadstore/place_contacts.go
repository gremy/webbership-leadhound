package leadstore

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/places"
)

// PlaceContacts is one place row of an import that points at an org, with the emails and
// phones its map listing gave (as stored on the place) and what the filters read of its org.
type PlaceContacts struct {
	PlaceID   int64    `db:"place_id"`
	OrgID     int64    `db:"org_id"`
	OrgDomain string   `db:"org_domain"` // its registrable domain, or PlaceOrgPrefix… with no website
	Country   string   `db:"country"`
	HasSite   bool     `db:"has_site"`
	Emails    []string `db:"emails"`
	Phones    []string `db:"phones"`
}

// ImportedPlaceContacts returns the places import importID stored that point at an org
// (chains point at none) and list at least one email or phone, by place id.
func (q Queries) ImportedPlaceContacts(ctx context.Context, importID int64) ([]PlaceContacts, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[PlaceContacts],
		`SELECT p.id AS place_id, o.id AS org_id, o.domain AS org_domain, o.country, o.site_id IS NOT NULL AS has_site,
		   p.emails, p.phones
		 FROM place p JOIN org o ON o.id = p.org_id
		 WHERE p.import_id = $1 AND (p.emails <> '[]'::jsonb OR p.phones <> '[]'::jsonb)
		 ORDER BY p.id`, importID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: imported place contacts: %w", err)
	}
	return rows, nil
}

// orgSuppressedByID is a SQL condition that is true when an active domain suppression of
// any account covers the domain of org $1 (orgDomainSuppressedSQL).
func orgSuppressedByID() string {
	return `EXISTS (SELECT 1 FROM org o WHERE o.id = $1::bigint AND ` + orgDomainSuppressedSQL("o") + `)`
}

// InsertPlaceRoute writes addr (already through the contact search's filters) as an email
// route of org orgID read from place placeID (derived_by overture@1, no fetch). Nothing is
// written when the org already has that route (a route the contact search or research found
// keeps its source), when an active suppression of any account covers the address or its
// domain, or when one covers the org's domain (a forgotten or opted-out org). inserted
// reports whether this call added the row. Run it in the import's transaction.
func (q Queries) InsertPlaceRoute(ctx context.Context, orgID, placeID int64, addr string, mxOK bool) (inserted bool, err error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return false, fmt.Errorf("leadstore: insert place route: %w", ErrEmptyRoute)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO contact_route (org_id, kind, value, place_id, mx_ok, derived_by)
		 SELECT $1::bigint, 'email', $3::text, $2::bigint, $4::boolean, $5::text
		 WHERE NOT `+orgSuppressedByID()+`
		   AND NOT EXISTS (SELECT 1 FROM suppression_active s WHERE (s.kind = 'address' AND s.value = $3::text)
		     OR (s.kind = 'domain' AND (split_part($3::text, '@', 2) = s.value
		       OR right(split_part($3::text, '@', 2), length(s.value) + 1) = '.' || s.value)))
		 ON CONFLICT (org_id, kind, value) DO NOTHING`,
		orgID, placeID, addr, mxOK, places.DerivedBy)
	if err != nil {
		return false, fmt.Errorf("leadstore: insert place route: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// InsertPlacePhone writes a phone of org orgID read from place placeID (derived_by
// overture@1, no fetch): value as listed, normalized its E.164 form. Nothing is written when
// the org already has that number, when an active phone suppression of any account holds it
// (PhoneSuppressionValue), or when a domain suppression covers the org's domain. It is a
// phone fact like any other, so the call rules, the screening registers and a wrong-number
// mark apply to it as they do to a number read from a page. Run it in the import's
// transaction.
func (q Queries) InsertPlacePhone(ctx context.Context, orgID, placeID int64, value, normalized string) (inserted bool, err error) {
	value, normalized = strings.TrimSpace(value), strings.TrimSpace(normalized)
	if value == "" || normalized == "" {
		return false, fmt.Errorf("leadstore: insert place phone: %w", ErrEmptyFact)
	}
	if utf8.RuneCountInString(value) > MaxFactRunes || utf8.RuneCountInString(normalized) > MaxFactRunes {
		return false, fmt.Errorf("leadstore: insert place phone: %w", ErrFactTooLong)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO org_fact (org_id, kind, value, normalized, place_id, derived_by)
		 SELECT $1::bigint, 'phone', $3::text, $4::text, $2::bigint, $5::text
		 WHERE NOT `+orgSuppressedByID()+`
		   AND NOT EXISTS (SELECT 1 FROM suppression_active s WHERE s.kind = 'phone' AND s.value = $6::text)
		 ON CONFLICT (org_id, kind, normalized) DO NOTHING`,
		orgID, placeID, value, normalized, places.DerivedBy, PhoneSuppressionValue(normalized))
	if err != nil {
		return false, fmt.Errorf("leadstore: insert place phone: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
