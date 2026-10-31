package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Manual send (00027): a manual batch the operator confirms with the warnings the confirm
// page showed, the send_request row that confirm writes for the send service, and the
// send_override row each message sent past a warning leaves behind.

// The warning codes a manual send may override (send_request_codes_check and
// send_override_codes_check hold the same closed set). Every hard stop stays outside it.
const (
	OverrideWindow        = "window"
	OverrideBusinessHours = "business_hours"
	OverrideRamp          = "ramp"
	OverrideDomainDaily   = "domain_daily"
	OverrideSpacingJitter = "spacing_jitter"
	OverrideSchedule      = "schedule"
)

// OverrideCodes is the closed set of overridable warning codes, in display order.
var OverrideCodes = []string{OverrideWindow, OverrideBusinessHours, OverrideRamp, OverrideDomainDaily,
	OverrideSpacingJitter, OverrideSchedule}

// SendRequest is one send_request row: a confirmed manual send of batch BatchID with the
// override codes Codes, the batch hash prefix the confirm page showed and the operator who
// confirmed it. Shown is the warnings, with their numbers, the confirm page showed (a JSON
// array; nil stores []). UsedAt is nil until the send service claims it.
type SendRequest struct {
	ID         int64           `db:"id"`
	BatchID    int64           `db:"batch_id"`
	Codes      []string        `db:"codes"`
	HashPrefix string          `db:"hash_prefix"`
	Operator   string          `db:"operator"`
	CreatedAt  time.Time       `db:"created_at"`
	UsedAt     *time.Time      `db:"used_at"`
	Shown      json.RawMessage `db:"shown"`
}

// SendOverride is one send_override row: lead LeadID of batch BatchID went out in run RunID
// (nil when not known) as message MessageID (nil when no message was written) with the
// warnings Codes overridden by Operator; Detail carries the numbers each warning showed (a
// JSON object; nil stores {}).
type SendOverride struct {
	ID        int64           `db:"id"`
	RunID     *int64          `db:"run_id"`
	BatchID   int64           `db:"batch_id"`
	LeadID    int64           `db:"lead_id"`
	MessageID *int64          `db:"message_id"`
	Codes     []string        `db:"codes"`
	Detail    json.RawMessage `db:"detail"`
	Operator  string          `db:"operator"`
	At        time.Time       `db:"at"`
}

// MarkBatchManual marks batch batchID as a manual batch. Only a batch neither approved nor
// discarded is marked; marking it again is a no-op. A missing batch is ErrBatchNotFound, an
// approved one ErrBatchApproved, a discarded one ErrBatchDiscarded. Run it in the transaction
// that creates the batch, so no other batch new ever sees it unmarked.
func (q Queries) MarkBatchManual(ctx context.Context, batchID int64) error {
	var approved, discarded bool
	err := q.db.QueryRow(ctx,
		`SELECT approved_at IS NOT NULL, discarded_at IS NOT NULL FROM outreach_batch WHERE id = $1 FOR UPDATE`,
		batchID).Scan(&approved, &discarded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("leadstore: mark batch manual %d: %w", batchID, ErrBatchNotFound)
	case err != nil:
		return fmt.Errorf("leadstore: mark batch manual: %w", err)
	case approved:
		return fmt.Errorf("leadstore: mark batch manual %d: %w", batchID, ErrBatchApproved)
	case discarded:
		return fmt.Errorf("leadstore: mark batch manual %d: %w", batchID, ErrBatchDiscarded)
	}
	if _, err := q.db.Exec(ctx, `UPDATE outreach_batch SET manual = true WHERE id = $1 AND NOT manual`, batchID); err != nil {
		return fmt.Errorf("leadstore: mark batch manual: %w", err)
	}
	return nil
}

// DiscardStaleManualBatches discards every manual batch neither approved nor discarded that
// was created before before, oldest first, as DiscardBatch does (its messages cancelled, its
// leads back to new), with the lead moves recorded as the system's. It returns how many it
// discarded. Run it in a transaction.
func (q Queries) DiscardStaleManualBatches(ctx context.Context, before time.Time) (int, error) {
	ids, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT id FROM outreach_batch
		 WHERE manual AND approved_at IS NULL AND discarded_at IS NULL AND created_at < $1
		 ORDER BY id`, before)
	if err != nil {
		return 0, fmt.Errorf("leadstore: discard stale manual batches: %w", err)
	}
	n := 0
	for _, id := range ids {
		d, err := q.discardBatch(ctx, id, ActorSystem)
		if err != nil {
			return n, fmt.Errorf("leadstore: discard stale manual batches: %w", err)
		}
		if !d.Already {
			n++
		}
	}
	return n, nil
}

// InsertSendRequest stores r (ID, CreatedAt and UsedAt are ignored: the row starts unused,
// created now) and returns its id. A code outside OverrideCodes, or a Shown that is not a JSON
// array, is refused by the table (a check violation); a missing batch by its foreign key.
func (q Queries) InsertSendRequest(ctx context.Context, r SendRequest) (int64, error) {
	codes := r.Codes
	if codes == nil {
		codes = []string{}
	}
	shown := []byte(r.Shown)
	if len(shown) == 0 {
		shown = []byte(`[]`)
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO send_request (batch_id, codes, hash_prefix, operator, shown) VALUES ($1, $2, $3, $4, $5::jsonb) RETURNING id`,
		r.BatchID, codes, r.HashPrefix, r.Operator, string(shown)).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: insert send request: %w", err)
	}
	return id, nil
}

// ClaimSendRequest takes the newest unused send request of batch batchID created at or after
// notBefore and marks it used at now, in one statement, so a request is claimed once only:
// a second claim, or one racing it, finds nothing. ok is false when there is none to claim.
func (q Queries) ClaimSendRequest(ctx context.Context, batchID int64, notBefore, now time.Time) (SendRequest, bool, error) {
	rs, err := collect(ctx, q.db, pgx.RowToStructByName[SendRequest],
		`UPDATE send_request SET used_at = $3
		 WHERE id = (SELECT id FROM send_request
		             WHERE batch_id = $1 AND used_at IS NULL AND created_at >= $2
		             ORDER BY created_at DESC, id DESC LIMIT 1
		             FOR UPDATE SKIP LOCKED)
		   AND used_at IS NULL
		 RETURNING id, batch_id, codes, hash_prefix, operator, created_at, used_at, shown`, batchID, notBefore, now)
	if err != nil {
		return SendRequest{}, false, fmt.Errorf("leadstore: claim send request: %w", err)
	}
	if len(rs) == 0 {
		return SendRequest{}, false, nil
	}
	return rs[0], true, nil
}

// PendingSendRequest is ClaimSendRequest's read without the claim: the newest unused send
// request of batch batchID created at or after notBefore, its row locked for the transaction (one
// another transaction holds is passed over), so the send can check it and only then mark it used
// (UseSendRequest) in the same transaction. A request the check refuses stays unused. ok is false
// when there is none. Run it in a transaction.
func (q Queries) PendingSendRequest(ctx context.Context, batchID int64, notBefore time.Time) (SendRequest, bool, error) {
	rs, err := collect(ctx, q.db, pgx.RowToStructByName[SendRequest],
		`SELECT id, batch_id, codes, hash_prefix, operator, created_at, used_at, shown FROM send_request
		 WHERE batch_id = $1 AND used_at IS NULL AND created_at >= $2
		 ORDER BY created_at DESC, id DESC LIMIT 1
		 FOR UPDATE SKIP LOCKED`, batchID, notBefore)
	if err != nil {
		return SendRequest{}, false, fmt.Errorf("leadstore: pending send request: %w", err)
	}
	if len(rs) == 0 {
		return SendRequest{}, false, nil
	}
	return rs[0], true, nil
}

// UseSendRequest marks send request id used at now. ok is false when it was used already.
func (q Queries) UseSendRequest(ctx context.Context, id int64, now time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx, `UPDATE send_request SET used_at = $2 WHERE id = $1 AND used_at IS NULL`, id, now)
	if err != nil {
		return false, fmt.Errorf("leadstore: use send request: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// InsertSendOverride stores o (ID is ignored; a zero At stores now). A code outside
// OverrideCodes is refused by the table (a check violation).
func (q Queries) InsertSendOverride(ctx context.Context, o SendOverride) error {
	codes := o.Codes
	if codes == nil {
		codes = []string{}
	}
	detail := []byte(o.Detail)
	if len(detail) == 0 {
		detail = []byte(`{}`)
	}
	var at *time.Time
	if !o.At.IsZero() {
		at = &o.At
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO send_override (run_id, batch_id, lead_id, message_id, codes, detail, operator, at)
		 VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, coalesce($8, now()))`,
		o.RunID, o.BatchID, o.LeadID, o.MessageID, codes, string(detail), o.Operator, at); err != nil {
		return fmt.Errorf("leadstore: insert send override: %w", err)
	}
	return nil
}

// SendOverrides returns batch batchID's override rows in the order they were written.
func (q Queries) SendOverrides(ctx context.Context, batchID int64) ([]SendOverride, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[SendOverride],
		`SELECT id, run_id, batch_id, lead_id, message_id, codes, detail, operator, at
		 FROM send_override WHERE batch_id = $1 ORDER BY id`, batchID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: send overrides: %w", err)
	}
	return rows, nil
}

// FirstEmailGone reports whether lead leadID's first email already went, or is going, in a
// message other than exceptID: a step-0 email message of a live batch that is sending, sent or
// maybe sent, or, when shadow is set (a shadow send asks), also one of a shadow batch written to
// the outbox (shadow_sent). A shadow send never stops a live one: a rehearsal is not a send.
// batchID is the earliest such message's batch. A send checks it before its first email, so a
// lead in two batches never gets two first emails.
func (q Queries) FirstEmailGone(ctx context.Context, leadID, exceptID int64, shadow bool) (batchID int64, gone bool, err error) {
	err = q.db.QueryRow(ctx,
		`SELECT m.batch_id FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE m.lead_id = $1 AND m.id <> $2 AND m.step = 0 AND m.channel = 'email'
		   AND ((NOT b.shadow AND m.state IN ('sending', 'sent', 'maybe_sent'))
		        OR ($3 AND b.shadow AND m.state = 'shadow_sent'))
		 ORDER BY m.id LIMIT 1`, leadID, exceptID, shadow).Scan(&batchID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("leadstore: first email gone: %w", err)
	}
	return batchID, true, nil
}
