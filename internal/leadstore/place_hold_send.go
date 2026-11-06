package leadstore

import (
	"context"
	"fmt"
)

// HeldLeadOutcome is what CancelHeldLead did to one held lead.
type HeldLeadOutcome struct {
	// Cancelled is how many of the lead's unsent messages (draft or approved, follow-ups and
	// calls included) were cancelled with SkipRoasteryHeld (SkipIntentHeld for an intent hold).
	Cancelled int64
	// BackToNew is set when the lead was batched with nothing of it ever reached, and moved
	// back to new so it is kept for later.
	BackToNew bool
}

// CancelHeldLead holds lead leadID out of café outreach at send time: every unsent message of
// it (draft or approved) is cancelled with SkipRoasteryHeld, and a batched lead none of whose
// messages ever went (sending, sent, maybe sent or done by hand) moves back to new, with a
// lead_event naming the hold. A lead with something already sent keeps its state: only its
// unsent follow-ups and calls are cancelled. Run it in one transaction with the caller's read
// of the hold, so the cancel and the state move land together.
func (q Queries) CancelHeldLead(ctx context.Context, leadID int64) (HeldLeadOutcome, error) {
	return q.cancelHeldLead(ctx, leadID, SkipRoasteryHeld)
}

// cancelHeldLead is CancelHeldLead with the hold's reason: the cancel reason and the
// lead_event's (CancelIntentHeldLead holds by loyalty intent the same way).
func (q Queries) cancelHeldLead(ctx context.Context, leadID int64, reason string) (HeldLeadOutcome, error) {
	var out HeldLeadOutcome
	n, err := q.CancelPending(ctx, CancelTarget{LeadID: leadID}, reason)
	if err != nil {
		return out, fmt.Errorf("leadstore: cancel held lead %d: %w", leadID, err)
	}
	out.Cancelled = n
	var reached bool
	if err := q.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM outreach_message WHERE lead_id = $1 AND state = ANY($2::text[]))`,
		leadID, []string{MsgSending, MsgSent, MsgMaybeSent, MsgDone}).Scan(&reached); err != nil {
		return out, fmt.Errorf("leadstore: cancel held lead %d: %w", leadID, err)
	}
	if reached {
		return out, nil
	}
	moved, err := q.setLeadStateFrom(ctx, leadID, []string{LeadBatched}, LeadNew, reason, ActorSystem)
	if err != nil {
		return out, fmt.Errorf("leadstore: cancel held lead %d: %w", leadID, err)
	}
	out.BackToNew = moved
	return out, nil
}
