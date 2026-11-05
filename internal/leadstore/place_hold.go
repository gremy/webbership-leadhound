package leadstore

import (
	"context"
	"fmt"
)

// SkipRoasteryHeld is the reason a held lead's message is cancelled or left out with, and the
// plain words a send road shows for it: the lead is a roastery and roasteries are off in café
// outreach for its account. It sits beside SkipSuppressed in outreach_message.cancel_reason.
const SkipRoasteryHeld = "roastery held out of café outreach"

// StarredRoasteryWords is the star note's detail for a starred lead held as a roastery.
const StarredRoasteryWords = "a roastery: held out of café outreach until you switch roasteries on in Controls"

// roasteryHeldSQL is the SQL condition that is true when the lead l is held out of café
// outreach: its effective place type (effectivePlaceTypeSQL) is roastery and the latest
// roastery_outreach_switch row of the account accountExpr names is not on (no row is off). A
// lead whose rating type is not café has no place type and is never held. The switch is read
// first, so with roasteries on the place type is not read at all.
func roasteryHeldSQL(accountExpr string) string {
	return `(NOT coalesce((SELECT rsw.is_on FROM roastery_outreach_switch rsw WHERE rsw.account_id = ` + accountExpr +
		` ORDER BY rsw.id DESC LIMIT 1), false) AND (` + effectivePlaceTypeSQL + `) = ` + sqlLiteral(PlaceRoastery) + `)`
}

// HeldLeads returns, in one query, the leads of ids held out of café outreach (roasteryHeldSQL
// under each lead's own campaign's account), each mapped to true. A lead not held, or an id
// with no lead, is not in the map.
func (q Queries) HeldLeads(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT l.id FROM lead l JOIN campaign cg ON cg.id = l.campaign_id
		 WHERE l.id = ANY($1::bigint[]) AND `+roasteryHeldSQL("cg.account_id"), ids)
	if err != nil {
		return nil, fmt.Errorf("leadstore: held leads: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("leadstore: held leads: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leadstore: held leads: %w", err)
	}
	return out, nil
}
