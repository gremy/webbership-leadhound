package leadstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

// DuplicateSkip is one lead the build skips as a duplicate, and the lead it duplicates.
type DuplicateSkip struct {
	LeadID   int64
	KeeperID int64
}

// DuplicateCollision is one group in which a lead the operator called or starred is a
// duplicate of the keeper, so it stays beside the keeper rather than being skipped.
type DuplicateCollision struct {
	KeeperID int64
	Stayed   []int64 // the called or starred leads still new beside the keeper, by id
}

// Duplicates is what DuplicateLeads found in one campaign.
type Duplicates struct {
	// Groups is how many groups of two or more leads share one place, a group whose
	// duplicates the system already skipped included, so a rebuild still counts it.
	Groups int
	// Leads is how many leads those groups hold, the keepers included.
	Leads int
	// Skips are the leads still new that the build should skip, by lead id.
	Skips []DuplicateSkip
	// Collisions are the groups where a called or starred lead stays beside its keeper.
	Collisions []DuplicateCollision
}

// duplicateRow is one lead of a campaign with its latest place and what decides its keeper.
type duplicateRow struct {
	LeadID     int64  `db:"lead_id"`
	State      string `db:"state"`
	SystemSkip bool   `db:"system_skip"`
	Protected  bool   `db:"protected"`
	Email      bool   `db:"email"`
	Name       string `db:"name"`
	Freeform   string `db:"freeform"`
	Locality   string `db:"locality"`
}

// duplicateRowsSQL is every lead of campaign $1 that is not a selftest, with its org's latest
// place imported into that campaign. A system skip is a skipped lead whose latest lead_event
// is the system's; a lead is protected when its current call is the operator's or it is
// starred; an email lead is one whose channel is email.
const duplicateRowsSQL = `SELECT l.id AS lead_id, l.state,
	   (l.state = 'skipped' AND coalesce((SELECT e.actor FROM lead_event e WHERE e.lead_id = l.id
	     ORDER BY e.id DESC LIMIT 1), '') = 'system') AS system_skip,
	   (coalesce((SELECT x.band IS NOT NULL AND x.actor = 'operator' FROM lead_label x WHERE x.lead_id = l.id
	     ORDER BY x.id DESC LIMIT 1), false)
	    OR coalesce((SELECT s.action <> 'unstar' FROM lead_star s WHERE s.lead_id = l.id
	     ORDER BY s.id DESC LIMIT 1), false)) AS protected,
	   l.channel = 'email' AS email,
	   pl.name, pl.freeform, pl.locality
	 FROM lead l
	 JOIN org o ON o.id = l.org_id
	 JOIN LATERAL (
	   SELECT x.name, x.freeform, x.locality FROM place x JOIN place_import i ON i.id = x.import_id
	   WHERE x.org_id = l.org_id AND i.campaign_id = l.campaign_id ORDER BY x.id DESC LIMIT 1
	 ) pl ON true
	 WHERE l.campaign_id = $1 AND l.kind <> 'selftest' AND o.kind <> 'selftest'
	 ORDER BY l.id`

// duplicateKey is the place a lead stands for, normalized once with cafe.NormalizeName (case,
// diacritics and punctuation folded): its name, freeform address and town. A place with no
// name or no freeform address has no key, so no two leads are matched by address alone or by
// name alone.
func duplicateKey(r duplicateRow) (string, bool) {
	name, addr := cafe.NormalizeName(r.Name), cafe.NormalizeName(r.Freeform)
	if name == "" || addr == "" {
		return "", false
	}
	return name + "\x00" + addr + "\x00" + cafe.NormalizeName(r.Locality), true
}

// keeperRank orders a group's candidates for keeper, lowest first: a lead already past new,
// then one the operator called or starred, then one with an email route, then any other. A
// lead the system skipped is never a keeper, and ok is false for it.
func keeperRank(r duplicateRow) (rank int, ok bool) {
	switch {
	case r.SystemSkip:
		return 0, false
	case r.State != LeadNew:
		return 0, true
	case r.Protected:
		return 1, true
	case r.Email:
		return 2, true
	default:
		return 3, true
	}
}

// DuplicateLeads finds the leads of campaign campaignID whose orgs' latest places in the
// campaign share a normalized name, freeform address and town. In each such group the keeper
// is chosen by keeperRank, the lowest lead id breaking a tie. Every other lead still new is
// a skip, except one the operator called or starred, which stays and is reported in a
// collision. A group whose leads the system all skipped has no keeper and changes nothing.
func (q Queries) DuplicateLeads(ctx context.Context, campaignID int64) (Duplicates, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[duplicateRow], duplicateRowsSQL, orCampaign(campaignID))
	if err != nil {
		return Duplicates{}, fmt.Errorf("leadstore: duplicate leads: %w", err)
	}
	groups := map[string][]duplicateRow{}
	var keys []string
	for _, r := range rows {
		k, ok := duplicateKey(r)
		if !ok {
			continue
		}
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	var out Duplicates
	for _, k := range keys {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		out.Groups++
		out.Leads += len(g)
		// Rows come by lead id, so the first lead of the best rank is the lowest id.
		keeper, best := -1, 0
		for i, r := range g {
			if rank, ok := keeperRank(r); ok && (keeper < 0 || rank < best) {
				keeper, best = i, rank
			}
		}
		if keeper < 0 {
			continue
		}
		var stayed []int64
		for i, r := range g {
			if i == keeper || r.State != LeadNew {
				continue
			}
			if r.Protected {
				stayed = append(stayed, r.LeadID)
				continue
			}
			out.Skips = append(out.Skips, DuplicateSkip{LeadID: r.LeadID, KeeperID: g[keeper].LeadID})
		}
		if len(stayed) > 0 {
			out.Collisions = append(out.Collisions, DuplicateCollision{KeeperID: g[keeper].LeadID, Stayed: stayed})
		}
	}
	slices.SortFunc(out.Skips, func(a, b DuplicateSkip) int { return cmp.Compare(a.LeadID, b.LeadID) })
	return out, nil
}
