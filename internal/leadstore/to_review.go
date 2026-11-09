package leadstore

import (
	"context"
	"fmt"
)

// OpenNotReviewed counts the non-selftest leads of account accountID's live campaigns (0: the
// default account) that are open and not yet reviewed, the step bar's Review count. It is
// Total - Reviewed of CampaignProgress summed over those campaigns: a lead in state new with
// no current call. A lead skipped by the operator is not new, so its last event need not be
// read here.
func (q Queries) OpenNotReviewed(ctx context.Context, accountID int64) (int, error) {
	var n int
	err := q.db.QueryRow(ctx,
		`SELECT count(*)::int
		 FROM lead l JOIN org o ON o.id = l.org_id JOIN campaign c ON c.id = l.campaign_id
		 WHERE c.account_id = $1 AND c.retired_at IS NULL AND l.state = 'new'
		   AND l.kind <> 'selftest' AND o.kind <> 'selftest'
		   AND `+currentBandSQL("l.id")+` IS NULL`, orAccount(accountID)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("leadstore: open not reviewed: %w", err)
	}
	return n, nil
}
