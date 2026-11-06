package leadstore

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// A café lead's place type (lead_place_type, migration 00030): whether it has a café to sit
// in, or is a roastery or an online-only coffee store. It is separate from the lead's kind,
// so templates, rating types and history stay untouched. Every answer is a row, from one of
// four sources, strongest first: the operator's own call, Instagram's place read (00032),
// Jev's read of the website, and a roastery word in the name.

// The place types a row may name (lead_place_type_type_check). PlaceUnclear never decides a
// lead's type: from Jev it is "asked, no answer", stored so a rerun does not ask again; from
// Instagram it is "silent now" (an unmatch, an unclear call), so the next source speaks.
const (
	PlaceCafe     = "cafe"
	PlaceRoastery = "roastery"
	PlaceUnclear  = "unclear"
)

// The sources of a place type (lead_place_type_source_check), strongest first in
// placeTypeWinnerSQL: a roastery's name does not mean it has no seating, so name words are
// the weakest.
const (
	PlaceFromOperator = "operator"
	// PlaceFromInstagram is Instagram's place call on the lead's strongly matched profile,
	// written by SyncIGPlaceTypes. It ranks under the operator and over Jev: a profile's own
	// posts show seating better than a website read, and the operator's call always wins.
	PlaceFromInstagram = "instagram"
	PlaceFromJev       = "jev"
	PlaceFromName      = "name"
)

var (
	// ErrBadPlaceType is returned by SetPlaceType for a type or source outside the lists
	// above, an unclear answer from any source but Jev or Instagram, or a Jev answer without
	// its input's SHA-256 (any other source's answer with one).
	ErrBadPlaceType = errors.New("leadstore: not a valid place type")
	// ErrNotCafeLead is returned by SetPlaceType for a lead whose rating type is not café:
	// such a lead never has a place type.
	ErrNotCafeLead = errors.New("leadstore: not a café lead")
)

// placeTypeWinnerSQL is the row deciding the place type of the lead l, with no row when no
// source speaks: the latest row of each source, read by lead_place_type_lead_idx, then the
// strongest of those that does not say unclear: operator, then instagram, then jev, then
// name. An unclear latest row of Instagram's or Jev's therefore falls through to the next
// source, and an older answer of the same source does not stand in for it.
const placeTypeWinnerSQL = `SELECT w.place_type, w.source, w.detail FROM (
	     SELECT DISTINCT ON (t.source) t.place_type, t.source, t.detail
	     FROM lead_place_type t WHERE t.lead_id = l.id ORDER BY t.source, t.id DESC
	   ) w
	   WHERE w.place_type <> '` + PlaceUnclear + `'
	   ORDER BY CASE w.source WHEN '` + PlaceFromOperator + `' THEN 0 WHEN '` + PlaceFromInstagram + `' THEN 1
	                         WHEN '` + PlaceFromJev + `' THEN 2 ELSE 3 END
	   LIMIT 1`

// effectivePlaceTypeSQL is the SQL expression giving the place type of the lead l: the
// winning row's, or café when no source speaks. It is empty for a lead whose rating type is
// not café (typeSQL), and every reader takes empty as "no chip, no hold". Every reader of a
// lead's place type uses this one fragment, so the list, the queue and the sends agree.
var effectivePlaceTypeSQL = "CASE WHEN l.kind IN (" + cafeKindsSQL() + ") THEN coalesce((SELECT e.place_type FROM (" +
	placeTypeWinnerSQL + ") e), " + sqlLiteral(PlaceCafe) + ") ELSE '' END"

// PlaceTypeRow is a lead's effective place type and where it came from.
type PlaceTypeRow struct {
	LeadID int64  `db:"lead_id"`
	Type   string `db:"place_type"` // empty for a lead whose rating type is not café
	Source string `db:"source"`     // empty when no source spoke, and Type is the default
	Detail string `db:"detail"`     // the winning row's matched word or question name
}

// PlaceTypeCounts is how many of a campaign's open leads have each effective place type.
type PlaceTypeCounts struct {
	Cafe     int
	Roastery int
}

// SetPlaceType appends one answer on lead leadID's place type: placeType and source from the
// lists above, detail the matched word, question name or Instagram's call, sha the SHA-256
// of what Jev was asked (Jev's rows only, nil for the rest), actor gremy or system. Unclear
// comes from Jev or Instagram only. The row is written
// whatever the lead's state, and it never touches the lead's call or state. A lead that does
// not exist is ErrLeadNotFound; a lead whose rating type is not café, or a selftest one, is
// ErrNotCafeLead.
func (q Queries) SetPlaceType(ctx context.Context, leadID int64, placeType, source, detail string, sha []byte, actor string) error {
	switch {
	case actor != ActorGremy && actor != ActorSystem:
		return fmt.Errorf("leadstore: set place type: %w: %q", ErrUnknownActor, actor)
	case !slices.Contains([]string{PlaceCafe, PlaceRoastery, PlaceUnclear}, placeType),
		!slices.Contains([]string{PlaceFromOperator, PlaceFromInstagram, PlaceFromJev, PlaceFromName}, source),
		placeType == PlaceUnclear && source != PlaceFromJev && source != PlaceFromInstagram,
		(source == PlaceFromJev) != (len(sha) == 32),
		source != PlaceFromJev && len(sha) != 0:
		return fmt.Errorf("leadstore: set place type: %w: %q from %q with a %d-byte hash", ErrBadPlaceType, placeType, source, len(sha))
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO lead_place_type (lead_id, place_type, source, detail, input_sha256, actor)
		 SELECT l.id, $2, $3, $4, $5, $6 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.id = $1 AND l.kind IN (`+cafeKindsSQL()+`) AND o.kind <> 'selftest'`,
		leadID, placeType, source, detail, sha, actor)
	if err != nil {
		return fmt.Errorf("leadstore: set place type %d: %w", leadID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lead WHERE id = $1)`, leadID).Scan(&exists); err != nil {
		return fmt.Errorf("leadstore: set place type %d: %w", leadID, err)
	}
	if !exists {
		return fmt.Errorf("leadstore: set place type %d: %w", leadID, ErrLeadNotFound)
	}
	return fmt.Errorf("leadstore: set place type %d: %w", leadID, ErrNotCafeLead)
}

// PlaceTypes reads the effective place type of each lead in ids, in one query, keyed by lead
// id: a lead whose rating type is not café has an empty Type, and an id with no lead is not
// in the map.
func (q Queries) PlaceTypes(ctx context.Context, ids []int64) (map[int64]PlaceTypeRow, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[PlaceTypeRow],
		`SELECT l.id AS lead_id, `+effectivePlaceTypeSQL+` AS place_type,
		        CASE WHEN l.kind IN (`+cafeKindsSQL()+`) THEN coalesce(w.source, '') ELSE '' END AS source,
		        CASE WHEN l.kind IN (`+cafeKindsSQL()+`) THEN coalesce(w.detail, '') ELSE '' END AS detail
		 FROM lead l LEFT JOIN LATERAL (`+placeTypeWinnerSQL+`) w ON true
		 WHERE l.id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("leadstore: place types: %w", err)
	}
	out := make(map[int64]PlaceTypeRow, len(rows))
	for _, r := range rows {
		out[r.LeadID] = r
	}
	return out, nil
}

// PlaceTypeCounts counts the open leads (state new, never a selftest one, as the list's open
// status) of campaign campaignID by effective place type; 0 counts every campaign's. A lead
// whose rating type is not café counts in neither.
func (q Queries) PlaceTypeCounts(ctx context.Context, campaignID int64) (PlaceTypeCounts, error) {
	var out PlaceTypeCounts
	err := q.db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE m.place_type = '`+PlaceCafe+`')::int,
		        count(*) FILTER (WHERE m.place_type = '`+PlaceRoastery+`')::int
		 FROM (SELECT `+effectivePlaceTypeSQL+` AS place_type
		       FROM lead l JOIN org o ON o.id = l.org_id
		       WHERE l.state = 'new' AND l.kind <> 'selftest' AND o.kind <> 'selftest'
		         AND ($1::bigint = 0 OR l.campaign_id = $1::bigint)) m`, campaignID).Scan(&out.Cafe, &out.Roastery)
	if err != nil {
		return PlaceTypeCounts{}, fmt.Errorf("leadstore: place type counts: %w", err)
	}
	return out, nil
}

// RoasteryOutreachOn reads whether roasteries are in café outreach for the account (zero
// account = the default account): the latest roastery_outreach_switch row, and no row is off.
func (q Queries) RoasteryOutreachOn(ctx context.Context, account int64) (bool, error) {
	var on bool
	err := q.db.QueryRow(ctx,
		`SELECT is_on FROM roastery_outreach_switch WHERE account_id = $1 ORDER BY id DESC LIMIT 1`,
		orAccount(account)).Scan(&on)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("leadstore: roastery outreach: %w", err)
	}
	return on, nil
}

// SetRoasteryOutreach turns roasteries in café outreach on or off for the account. The table
// is append-only, so the switch's history stays; a write that would not change the value is
// skipped (changed false), in the same statement that reads the current value.
func (q Queries) SetRoasteryOutreach(ctx context.Context, account int64, on bool, actor string) (changed bool, err error) {
	if actor != ActorGremy && actor != ActorSystem {
		return false, fmt.Errorf("leadstore: set roastery outreach: %w: %q", ErrUnknownActor, actor)
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO roastery_outreach_switch (account_id, is_on, actor)
		 SELECT $1, $2, $3
		 WHERE coalesce((SELECT is_on FROM roastery_outreach_switch WHERE account_id = $1
		                 ORDER BY id DESC LIMIT 1), false) IS DISTINCT FROM $2`,
		orAccount(account), on, actor)
	if err != nil {
		return false, fmt.Errorf("leadstore: set roastery outreach: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
