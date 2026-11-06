package leadstore

import (
	"context"
	"fmt"
	"time"
)

// SkipIntentHeld is the reason a lead held by its loyalty intent has its messages cancelled
// and its lead_event written with (CancelIntentHeldLead), beside SkipRoasteryHeld.
const SkipIntentHeld = "held: loyalty intent"

// intentHeldBatchSQL is true when the message m (of batch b, lead l) is held by its lead's
// loyalty intent at now: the lead is held (intentHeldSQL) and the batch is not a manual one,
// since Send now on chosen leads is the operator's own act and is never held.
func intentHeldBatchSQL(now string) string {
	return `(NOT b.manual AND ` + intentHeldSQL(now) + `)`
}

// IntentHeldMessages returns, in one query, the messages of ids whose lead its loyalty intent
// holds from sends at now (intentHeldSQL: a Just chose inside its hold, an Out), each mapped
// to true. A message of a manual batch is never held, nor is one whose lead has no intent row
// and no call (every Drupal lead), nor an id with no message.
func (q Queries) IntentHeldMessages(ctx context.Context, ids []int64, now time.Time) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx,
		`SELECT m.id FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE m.id = ANY($1::bigint[]) AND `+intentHeldBatchSQL("$2::timestamptz"), ids, now)
	if err != nil {
		return nil, fmt.Errorf("leadstore: intent held messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("leadstore: intent held messages: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("leadstore: intent held messages: %w", err)
	}
	return out, nil
}

// CancelIntentHeldLead holds lead leadID by its loyalty intent at send time, exactly as
// CancelHeldLead holds a roastery: every unsent message of it is cancelled with
// SkipIntentHeld, and a batched lead none of whose messages ever went moves back to new with
// a lead_event naming the hold. The batch's other leads are untouched. Run it in one
// transaction with the caller's read of the hold.
func (q Queries) CancelIntentHeldLead(ctx context.Context, leadID int64) (HeldLeadOutcome, error) {
	return q.cancelHeldLead(ctx, leadID, SkipIntentHeld)
}
