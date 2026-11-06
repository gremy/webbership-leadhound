package leadstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// What the Leads list shows beyond its own columns: the extras of one page's rows, read in a
// batch by lead id, and the towns a campaign's filter offers.

// leadPlaceWhereSQL keeps the places x (joined to their import i) that speak for the lead l:
// those of l's org imported into l's campaign, and only when l is a café lead. It is the same
// choice as the pl lateral of rateLeadSQL.
var leadPlaceWhereSQL = `l.kind IN (` + cafeKindsSQL() + `) AND x.org_id = l.org_id AND i.campaign_id = l.campaign_id`

// leadTownSQL is the SQL expression giving the town of the lead l: its latest place's locality,
// and empty for a lead with no place or one whose rating type is not café.
var leadTownSQL = `coalesce((SELECT x.locality FROM place x JOIN place_import i ON i.id = x.import_id
	   WHERE ` + leadPlaceWhereSQL + ` ORDER BY x.id DESC LIMIT 1), '')`

// listTowns is towns without its blank values, in order.
func listTowns(towns []string) []string {
	out := make([]string, 0, len(towns))
	for _, t := range towns {
		if strings.TrimSpace(t) != "" {
			out = append(out, t)
		}
	}
	return out
}

// RowExtras is what one row of the list shows beyond ListRow. The slices are never nil, and a
// retired route or fact is in none of them.
type RowExtras struct {
	LeadID      int64    `db:"lead_id"`
	Description string   `db:"description"` // the org's first live description fact
	Address     string   `db:"address"`     // the latest place's street line and postcode, without the town
	Category    string   `db:"category"`    // the latest place's category
	Socials     []string `db:"socials"`
	Phones      []string `db:"phones"`
	Emails      []string `db:"emails"`
	Forms       []string `db:"forms"`
	LinkedIns   []string `db:"linkedins"`
	Instagrams  []string `db:"instagrams"`
	LastActor   string   `db:"-"` // who wrote the lead's latest event; "" with no event
	LastReason  string   `db:"-"` // the latest event's reason
}

// TownCount is one town of ListTowns: its most common spelling, and how many open leads are in
// it under any spelling.
type TownCount struct {
	Town  string `db:"town"`
	Count int    `db:"count"`
}

// ListExtras reads the extras of the leads leadIDs in two queries, keyed by lead id; an id with
// no lead is not in the map. The first query reads each org's live facts and routes as
// LeadDetail does, in id order within a kind, and each café lead's latest place. The second
// reads the latest events in one pass, because lead_event has no index on lead_id and a read
// per row would scan the table once for each.
func (q Queries) ListExtras(ctx context.Context, leadIDs []int64) (map[int64]RowExtras, error) {
	out := make(map[int64]RowExtras, len(leadIDs))
	if len(leadIDs) == 0 {
		return out, nil
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByNameLax[RowExtras],
		`SELECT l.id AS lead_id, coalesce(fx.description, '') AS description,
		   coalesce(pl.address, '') AS address, coalesce(pl.category, '') AS category,
		   coalesce(fx.socials, '{}') AS socials, coalesce(fx.phones, '{}') AS phones,
		   coalesce(rt.emails, '{}') AS emails, coalesce(rt.forms, '{}') AS forms,
		   coalesce(rt.linkedins, '{}') AS linkedins, coalesce(rt.instagrams, '{}') AS instagrams
		 FROM lead l
		 LEFT JOIN LATERAL (
		   SELECT concat_ws(', ', nullif(x.freeform, ''), nullif(x.postcode, '')) AS address, x.category
		   FROM place x JOIN place_import i ON i.id = x.import_id
		   WHERE `+leadPlaceWhereSQL+` ORDER BY x.id DESC LIMIT 1
		 ) pl ON true
		 LEFT JOIN LATERAL (
		   SELECT (array_agg(x.value ORDER BY x.id) FILTER (WHERE x.kind = 'description'))[1] AS description,
		     array_agg(x.value ORDER BY x.id) FILTER (WHERE x.kind = 'social') AS socials,
		     array_agg(x.value ORDER BY x.id) FILTER (WHERE x.kind = 'phone') AS phones
		   FROM org_fact x WHERE x.org_id = l.org_id AND `+reachFactLiveSQL+`
		 ) fx ON true
		 LEFT JOIN LATERAL (
		   SELECT array_agg(r.value ORDER BY r.id) FILTER (WHERE r.kind = 'email') AS emails,
		     array_agg(r.value ORDER BY r.id) FILTER (WHERE r.kind = 'form') AS forms,
		     array_agg(r.value ORDER BY r.id) FILTER (WHERE r.kind = 'linkedin') AS linkedins,
		     array_agg(r.value ORDER BY r.id) FILTER (WHERE r.kind = 'instagram') AS instagrams
		   FROM contact_route r WHERE r.org_id = l.org_id AND `+reachRouteLiveSQL+`
		 ) rt ON true
		 WHERE l.id = ANY($1::bigint[])`, leadIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: list extras: %w", err)
	}
	for _, r := range rows {
		out[r.LeadID] = r
	}
	type event struct {
		LeadID        int64
		Actor, Reason string
	}
	events, err := collect(ctx, q.db, pgx.RowToStructByPos[event],
		`SELECT DISTINCT ON (e.lead_id) e.lead_id, e.actor, e.reason
		 FROM lead_event e WHERE e.lead_id = ANY($1::bigint[]) ORDER BY e.lead_id, e.id DESC`, leadIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: list extras: events: %w", err)
	}
	for _, e := range events {
		if r, ok := out[e.LeadID]; ok {
			r.LastActor, r.LastReason = e.Actor, e.Reason
			out[e.LeadID] = r
		}
	}
	return out, nil
}

// ListTowns returns the non-empty towns of the open leads (state new, never a selftest one, as
// the list's open status) of campaign campaignID, most leads first, then by name; 0 reads every
// campaign's. Spellings merge by lower() only, and a town shows its most common spelling, the
// first in sort order on a tie.
func (q Queries) ListTowns(ctx context.Context, campaignID int64) ([]TownCount, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[TownCount],
		`SELECT mode() WITHIN GROUP (ORDER BY m.town) AS town, count(*)::int AS count
		 FROM (SELECT `+leadTownSQL+` AS town
		       FROM lead l JOIN org o ON o.id = l.org_id
		       WHERE l.state = 'new' AND l.kind <> 'selftest' AND o.kind <> 'selftest'
		         AND ($1::bigint = 0 OR l.campaign_id = $1::bigint)) m
		 WHERE m.town <> ''
		 GROUP BY lower(m.town)
		 ORDER BY count(*) DESC, lower(m.town)`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: list towns: %w", err)
	}
	return rows, nil
}
