package leadstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrSelftestMessage is returned by ResolveSent and ResolveNotSent for a selftest's
// message: a selftest goes only in its own run, so nothing would ever send it again, and it
// is never checked by hand.
var ErrSelftestMessage = errors.New("leadstore: a selftest message is not checked by hand")

// Resolved is what ResolveSent or ResolveNotSent did to one email checked by hand.
type Resolved struct {
	BatchID   int64
	LeadID    int64
	Step      int
	SentAt    time.Time // ResolveSent: when it went (its attempt time); zero for not sent
	LeadMoved bool      // ResolveSent: the lead moved from batched to contacted
}

// lockCheckable locks live email id and returns it when it is maybe_sent or sending (an
// email that may have gone) and not a selftest's; otherwise ErrMessageState,
// ErrSelftestMessage or ErrMessageNotFound.
func (q Queries) lockCheckable(ctx context.Context, op string, id int64) (Resolved, error) {
	var (
		r        Resolved
		state    string
		shadow   bool
		selftest bool
	)
	err := q.db.QueryRow(ctx,
		`SELECT m.batch_id, m.lead_id, m.step, m.state, b.shadow, l.kind = 'selftest'
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE m.id = $1 FOR UPDATE OF m`, id).Scan(&r.BatchID, &r.LeadID, &r.Step, &state, &shadow, &selftest)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Resolved{}, fmt.Errorf("leadstore: %s: message %d: %w", op, id, ErrMessageNotFound)
	case err != nil:
		return Resolved{}, fmt.Errorf("leadstore: %s: %w", op, err)
	case selftest:
		return Resolved{}, fmt.Errorf("leadstore: %s: message %d: %w", op, id, ErrSelftestMessage)
	case shadow || (state != MsgMaybeSent && state != MsgSending):
		return Resolved{}, fmt.Errorf("leadstore: %s: message %d is %s: %w", op, id, state, ErrMessageState)
	}
	return r, nil
}

// ResolveSent records that the operator found live email id (maybe_sent, or sending as a
// broken run left it) in the mailbox's Sent folder: it becomes sent at its attempt time (now
// when it has none) and its lead moves from batched to contacted, as MarkSent does. The
// caller schedules the next step and holds the send lock, because a sending row is only
// known stuck while no send runs. Run it in a transaction.
func (q Queries) ResolveSent(ctx context.Context, id int64) (Resolved, error) {
	r, err := q.lockCheckable(ctx, "resolve sent", id)
	if err != nil {
		return Resolved{}, err
	}
	if err := q.db.QueryRow(ctx,
		`UPDATE outreach_message SET state = 'sent', sent_at = coalesce(sent_at, now()) WHERE id = $1 RETURNING sent_at`,
		id).Scan(&r.SentAt); err != nil {
		return Resolved{}, fmt.Errorf("leadstore: resolve sent: %w", err)
	}
	if r.LeadMoved, err = q.setLeadStateFrom(ctx, r.LeadID, []string{LeadBatched}, LeadContacted,
		fmt.Sprintf("message %d found sent by hand", id), ActorGremy); err != nil {
		return Resolved{}, fmt.Errorf("leadstore: resolve sent: %w", err)
	}
	return r, nil
}

// ResolveNotSent records that live email id (maybe_sent or sending) is not in the mailbox's
// Sent folder: it goes back to approved with no attempt time, so the cap counts it on the
// day it really goes, and send picks it up at its due time. The lead stays as it is. The
// caller holds the send lock, as for ResolveSent. Run it in a transaction.
func (q Queries) ResolveNotSent(ctx context.Context, id int64) (Resolved, error) {
	r, err := q.lockCheckable(ctx, "resolve not sent", id)
	if err != nil {
		return Resolved{}, err
	}
	if _, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'approved', sent_at = NULL WHERE id = $1`, id); err != nil {
		return Resolved{}, fmt.Errorf("leadstore: resolve not sent: %w", err)
	}
	return r, nil
}
