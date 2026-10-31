package leadstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/dial"
)

// Call steps (channel phone): the operator calls, then records what happened. The outcome
// drives the lead's sequence through campaign.Next, in the same transaction.

var (
	// ErrNotCall is returned by RecordCallOutcome for a message that is not a call step.
	ErrNotCall = errors.New("not a call step")
	// ErrNotDue is returned by RecordCallOutcome for a call whose due time has not come (or
	// that has none yet: it waits on an earlier step).
	ErrNotDue = errors.New("not due yet")
	// ErrSequenceEnded is returned by RecordCallOutcome when the lead replied, opted out,
	// bounced or was suppressed, so the call was cancelled with the rest of its sequence.
	ErrSequenceEnded = errors.New("the sequence has ended")
	// ErrCallAdvanced is returned by RecordCallOutcome for a second outcome on a call the
	// sequence has already moved on from; the error names the first outcome.
	ErrCallAdvanced = errors.New("the sequence has moved on")
	// ErrBadOutcome is returned by RecordCallOutcome for an unknown outcome, a note over 500
	// characters, a consent on a call that did not connect, or a consent without an address
	// or wording.
	ErrBadOutcome = errors.New("bad call outcome")
)

// NotDueError is ErrNotDue with why: the step the call waits on (an earlier step of its lead
// in the batch that is not yet sent, shadow-sent or done; WaitsOnStep is 0-based, -1 when
// none is named) or, when it waits on nothing, the due time still to come (nil: none yet).
type NotDueError struct {
	WaitsOnStep    int
	WaitsOnChannel string // the waited-on step's channel: email | phone | form | linkedin
	DueAt          *time.Time
}

func (e *NotDueError) Error() string {
	switch {
	case e.WaitsOnStep >= 0:
		return fmt.Sprintf("%s: it waits on step %d (%s)", ErrNotDue, e.WaitsOnStep+1, e.WaitsOnChannel)
	case e.DueAt != nil:
		return fmt.Sprintf("%s: due %s", ErrNotDue, e.DueAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: it waits on an earlier step", ErrNotDue)
}

// Unwrap makes a NotDueError match ErrNotDue.
func (e *NotDueError) Unwrap() error { return ErrNotDue }

// pendingBefore is the condition on an earlier row w of the same lead and batch that a later
// step still waits on: anything not sent, shadow-sent, done or cancelled. It matches where
// campaign.Next stops its walk, so no later step can be due while such a row exists.
const pendingBefore = `w.lead_id = m.lead_id AND w.batch_id = m.batch_id AND w.step < m.step
	   AND w.state NOT IN ('sent', 'shadow_sent', 'done', 'cancelled')`

// MaxCallNote is the longest note a call outcome may carry (call_outcome_note_check).
const MaxCallNote = 500

// NoNumber is what SequenceOf says of a step with no row in a sequence that has a call step:
// the lead had no callable number at batch time.
const NoNumber = "skipped: no number"

// ConsentInput is an agreement, won on a connected call, to get email from the campaign.
type ConsentInput struct {
	Address string // the address they gave or confirmed; stored lower case
	Wording string // the sentence the operator said (the call card's fixed text)
}

// CallRecorded is what RecordCallOutcome did.
type CallRecorded struct {
	OutcomeID  int64            `json:"outcome_id"`
	LeadID     int64            `json:"lead_id"`
	Shadow     bool             `json:"shadow"`
	Outcome    string           `json:"outcome"`
	ConsentID  int64            `json:"consent_id,omitempty"` // the email_consent row; 0 when none
	Suppressed bool             `json:"suppressed"`           // a do-not-call phone suppression was written
	Moved      bool             `json:"moved"`                // the lead moved from batched to contacted (live only)
	Due        map[int64]string `json:"due"`                  // message id → its new due time (RFC 3339), as applied
	Cancelled  map[int64]string `json:"cancelled"`            // message id → cancel reason, as applied
}

// beginner is a DBTX that can open a (sub)transaction: a pool, a connection or a pgx.Tx
// (a savepoint).
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// RecordCallOutcome records outcome (a campaign.Outcomes value) on call message messageID of
// account accountID (0: DefaultAccountID), in one transaction (a savepoint when q already runs
// in one) with the lead and its messages locked:
//   - refused (nothing written) when the row is not a call (ErrNotCall), is not approved
//     (ErrMessageState), is not due at now (ErrNotDue, as a *NotDueError: an earlier step of
//     the lead is not yet sent or done, whatever the call's due time, or no due time yet, or
//     one still to come), was cancelled because the lead replied,
//     opted out, bounced or was suppressed (ErrSequenceEnded: "They replied: the sequence has
//     ended"), or already has an outcome the sequence moved on from (ErrCallAdvanced, naming
//     the first outcome). A second outcome on a call nothing has moved on from (no later step
//     scheduled, cancelled or sent) is taken: the latest wins;
//   - else it writes the call_outcome row (note at most MaxCallNote characters), marks the call
//     done, writes the email_consent row when consent is given (connected only), writes a
//     lead_event, moves a live lead from batched to contacted on connected, voicemail or
//     do_not_call, runs campaign.Next over the lead's rows of the batch with cal and applies
//     the decision (ApplyDecision), and for do_not_call writes a phone suppression of the
//     number in every form that dials it (the stored value, its E.164 form and every other
//     stored fact value that dials as it, as RecordScreening does; also in a shadow batch: a
//     person's wish is real); wrong_number records the
//     dialled phone fact as bad (phone_status, 00020), so no later batch drafts it, and a
//     later outcome replacing a wrong_number on the same call records it good again.
//
// actor is ActorGremy (stored as operator on the outcome) or ActorSystem.
func (q Queries) RecordCallOutcome(ctx context.Context, accountID, messageID int64, outcome, note string,
	consent *ConsentInput, actor string, now time.Time, cal campaign.Calendar) (CallRecorded, error) {
	return q.RecordCallOutcomeWithCallBack(ctx, accountID, messageID, outcome, note, consent, nil, actor, now, cal)
}

// RecordCallOutcomeWithCallBack is RecordCallOutcome that also stores callBackOn, the day the
// operator means to call the lead again (call_outcome.call_back_on; nil: none). Only the
// calendar date of callBackOn in its own location is kept, and it may not be before now's
// date in that location. A call-back comes with any outcome but do_not_call and wrong_number
// (there is no number to call back on); either refusal is ErrBadOutcome and writes nothing.
// The call queue (CallQueue) lists the lead on that day while this outcome is its latest.
func (q Queries) RecordCallOutcomeWithCallBack(ctx context.Context, accountID, messageID int64, outcome, note string,
	consent *ConsentInput, callBackOn *time.Time, actor string, now time.Time, cal campaign.Calendar) (CallRecorded, error) {
	res, err := q.recordCallOutcomeTx(ctx, orAccount(accountID), messageID, outcome, note, consent, callBackOn, actor, now, cal)
	if err != nil {
		return CallRecorded{}, fmt.Errorf("leadstore: record call outcome %d: %w", messageID, err)
	}
	return res, nil
}

// callBackDate checks callBackOn against outcome and now and returns the date to store
// ("YYYY-MM-DD"; nil when none).
func callBackDate(outcome string, callBackOn *time.Time, now time.Time) (*string, error) {
	if callBackOn == nil {
		return nil, nil
	}
	if outcome == campaign.OutcomeDoNotCall || outcome == campaign.OutcomeWrongNumber {
		return nil, fmt.Errorf("%w: a call-back does not go with %s", ErrBadOutcome, outcome)
	}
	loc := callBackOn.Location()
	y, m, d := callBackOn.Date()
	ny, nm, nd := now.In(loc).Date()
	if time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Before(time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC)) {
		return nil, fmt.Errorf("%w: the call-back date is in the past", ErrBadOutcome)
	}
	s := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	return &s, nil
}

func (q Queries) recordCallOutcomeTx(ctx context.Context, accountID, messageID int64, outcome, note string,
	consent *ConsentInput, callBackOn *time.Time, actor string, now time.Time, cal campaign.Calendar) (CallRecorded, error) {
	if err := checkOutcomeInput(outcome, note, consent, actor, cal); err != nil {
		return CallRecorded{}, err
	}
	back, err := callBackDate(outcome, callBackOn, now)
	if err != nil {
		return CallRecorded{}, err
	}
	b, ok := q.db.(beginner)
	if !ok {
		return q.recordCallOutcome(ctx, accountID, messageID, outcome, note, consent, back, actor, now, cal)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return CallRecorded{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	res, err := NewQueries(tx).recordCallOutcome(ctx, accountID, messageID, outcome, note, consent, back, actor, now, cal)
	if err != nil {
		return CallRecorded{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CallRecorded{}, err
	}
	return res, nil
}

func checkOutcomeInput(outcome, note string, consent *ConsentInput, actor string, cal campaign.Calendar) error {
	switch {
	case !slices.Contains(campaign.Outcomes, outcome):
		return fmt.Errorf("%w: unknown outcome %q", ErrBadOutcome, outcome)
	case utf8.RuneCountInString(note) > MaxCallNote:
		return fmt.Errorf("%w: the note is over %d characters", ErrBadOutcome, MaxCallNote)
	case actor != ActorGremy && actor != ActorSystem:
		return fmt.Errorf("%w: %q", ErrUnknownActor, actor)
	case cal == nil:
		return errors.New("no calendar")
	}
	if consent != nil {
		addr := strings.ToLower(strings.TrimSpace(consent.Address))
		local, dom, at := strings.Cut(addr, "@")
		switch {
		case outcome != campaign.OutcomeConnected:
			return fmt.Errorf("%w: consent comes only with a connected call", ErrBadOutcome)
		case !at || local == "" || dom == "" || strings.ContainsAny(addr, " \t\r\n") || strings.Contains(dom, "@"):
			return fmt.Errorf("%w: consent needs the address they gave", ErrBadOutcome)
		case strings.TrimSpace(consent.Wording) == "" || strings.ContainsAny(consent.Wording, "\r\n"):
			return fmt.Errorf("%w: consent needs the one-line wording the operator said", ErrBadOutcome)
		}
	}
	return nil
}

// callRow is the call message and its batch, as RecordCallOutcome reads it.
type callRow struct {
	leadID, batchID, orgID, campaignID int64
	step                               int
	channel, state, cancelReason       string
	seqName, leadState                 string
	dueAt                              *time.Time
	phoneFactID                        *int64
	shadow                             bool
	sequence                           []byte
}

func (q Queries) recordCallOutcome(ctx context.Context, accountID, messageID int64, outcome, note string,
	consent *ConsentInput, callBack *string, actor string, now time.Time, cal campaign.Calendar) (CallRecorded, error) {
	var leadID int64
	err := q.db.QueryRow(ctx,
		`SELECT m.lead_id FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE m.id = $1 AND b.account_id = $2`, messageID, accountID).Scan(&leadID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CallRecorded{}, ErrMessageNotFound
	}
	if err != nil {
		return CallRecorded{}, err
	}
	// Lock the lead, then its messages (the order Suppress and the mailbox take: lead first).
	if _, err := q.db.Exec(ctx, `SELECT 1 FROM lead WHERE id = $1 FOR UPDATE`, leadID); err != nil {
		return CallRecorded{}, err
	}
	if _, err := q.db.Exec(ctx, `SELECT 1 FROM outreach_message WHERE lead_id = $1 ORDER BY id FOR UPDATE`, leadID); err != nil {
		return CallRecorded{}, err
	}
	var r callRow
	if err := q.db.QueryRow(ctx,
		`SELECT m.lead_id, m.batch_id, l.org_id, b.campaign_id, m.step, m.channel, m.state, m.cancel_reason,
		   coalesce(m.sequence_name, ''), l.state, m.due_at, m.phone_fact_id, b.shadow, b.sequence
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE m.id = $1`, messageID).Scan(&r.leadID, &r.batchID, &r.orgID, &r.campaignID, &r.step, &r.channel,
		&r.state, &r.cancelReason, &r.seqName, &r.leadState, &r.dueAt, &r.phoneFactID, &r.shadow, &r.sequence); err != nil {
		return CallRecorded{}, err
	}
	first, err := q.firstOutcome(ctx, messageID)
	if err != nil {
		return CallRecorded{}, err
	}
	if err := q.callRefusal(ctx, messageID, r, first, now); err != nil {
		return CallRecorded{}, err
	}

	res := CallRecorded{LeadID: r.leadID, Shadow: r.shadow, Outcome: outcome, Due: map[int64]string{}, Cancelled: map[int64]string{}}
	storedActor := "system"
	if actor == ActorGremy {
		storedActor = ActorOperator
	}
	var consentAddr *string
	if consent != nil {
		a := strings.ToLower(strings.TrimSpace(consent.Address))
		consentAddr = &a
	}
	prev, err := q.latestOutcome(ctx, messageID)
	if err != nil {
		return CallRecorded{}, err
	}
	if err := q.db.QueryRow(ctx,
		`INSERT INTO call_outcome (message_id, outcome, note, email_consent, consent_address, actor, at, call_back_on)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8::date) RETURNING id`,
		messageID, outcome, note, consent != nil, consentAddr, storedActor, now, callBack).Scan(&res.OutcomeID); err != nil {
		return CallRecorded{}, err
	}
	// A wrong number marks the dialled fact bad (also in a shadow batch: the operator really
	// rang it), so no later batch drafts it again; an outcome replacing a wrong_number on the
	// same call marks it good again.
	if r.phoneFactID != nil {
		if err := q.markWrongNumber(ctx, messageID, *r.phoneFactID, res.OutcomeID, outcome, prev, storedActor); err != nil {
			return CallRecorded{}, err
		}
	}
	if consent != nil {
		if err := q.db.QueryRow(ctx,
			`INSERT INTO email_consent (org_id, campaign_id, address, call_outcome_id, wording, actor, at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			r.orgID, r.campaignID, *consentAddr, res.OutcomeID, strings.TrimSpace(consent.Wording), storedActor, now).
			Scan(&res.ConsentID); err != nil {
			return CallRecorded{}, err
		}
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'done', sent_at = coalesce(sent_at, $2) WHERE id = $1 AND state IN ('approved', 'done')`,
		messageID, now)
	if err := markResult("mark call done", messageID, tag.RowsAffected(), err); err != nil {
		return CallRecorded{}, err
	}
	reason := fmt.Sprintf("call %d: %s", messageID, outcome)
	if r.shadow {
		reason += " (shadow)"
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO lead_event (lead_id, state, reason, actor) SELECT id, state, $2, $3 FROM lead WHERE id = $1`,
		r.leadID, reason, actor); err != nil {
		return CallRecorded{}, err
	}
	if !r.shadow && (outcome == campaign.OutcomeConnected || outcome == campaign.OutcomeVoicemail || outcome == campaign.OutcomeDoNotCall) {
		if res.Moved, err = q.setLeadStateFrom(ctx, r.leadID, []string{LeadBatched}, LeadContacted, reason, actor); err != nil {
			return CallRecorded{}, err
		}
	}

	d, err := q.leadDecision(ctx, r, now, cal)
	if err != nil {
		return CallRecorded{}, err
	}
	if _, err := q.ApplyDecision(ctx, d); err != nil {
		return CallRecorded{}, err
	}
	for id, why := range d.Cancel {
		res.Cancelled[id] = why
	}
	for id, t := range d.Due {
		if _, c := d.Cancel[id]; !c {
			res.Due[id] = t.UTC().Format(time.RFC3339)
		}
	}

	// After the decision, so the engine's own reasons cancel this lead's later steps; the
	// suppression then reaches any other lead that dials the same number.
	if outcome == campaign.OutcomeDoNotCall && r.phoneFactID != nil {
		var normalized, country string
		if err := q.db.QueryRow(ctx, `SELECT f.normalized, coalesce(o.country, '') FROM org_fact f JOIN org o ON o.id = f.org_id
			WHERE f.id = $1`, *r.phoneFactID).Scan(&normalized, &country); err != nil {
			return CallRecorded{}, err
		}
		// Every form that dials this number, as RecordScreening and ForgetOrg write them: the
		// stored value, its E.164 form, and every other stored fact value that dials as it, so
		// another org listing the number in another form is not called either.
		forms := []string{normalized}
		if e := dial.Number(normalized, country); e164RE.MatchString(e) {
			more, err := q.numberForms(ctx, e)
			if err != nil {
				return CallRecorded{}, err
			}
			for _, n := range more {
				if !slices.Contains(forms, n) {
					forms = append(forms, n)
				}
			}
		}
		for _, n := range forms {
			if _, err := q.Suppress(ctx, accountID, SuppressPhone, PhoneSuppressionValue(n), ReasonDoNotCall,
				fmt.Sprintf("call:%d", messageID)); err != nil {
				return CallRecorded{}, err
			}
		}
		res.Suppressed = true
	}
	return res, nil
}

// latestOutcome returns the latest outcome recorded on message id; "" when none.
func (q Queries) latestOutcome(ctx context.Context, id int64) (string, error) {
	var o string
	err := q.db.QueryRow(ctx, `SELECT outcome FROM call_outcome WHERE message_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&o)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return o, err
}

// firstOutcome returns the first outcome recorded on message id; "" when none.
func (q Queries) firstOutcome(ctx context.Context, id int64) (string, error) {
	var o string
	err := q.db.QueryRow(ctx, `SELECT outcome FROM call_outcome WHERE message_id = $1 ORDER BY id LIMIT 1`, id).Scan(&o)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return o, err
}

// callRefusal says why an outcome on call r (message id) cannot be taken at now; nil when it can.
func (q Queries) callRefusal(ctx context.Context, id int64, r callRow, first string, now time.Time) error {
	ended := func(why string) error {
		if why == LeadReplied || why == "reply" {
			return fmt.Errorf("They replied: %w", ErrSequenceEnded) //nolint:staticcheck // the operator's own words, shown as a sentence on the card
		}
		return fmt.Errorf("%s: %w", why, ErrSequenceEnded)
	}
	switch {
	case r.channel != ChannelPhone:
		return fmt.Errorf("message is a %s step: %w", r.channel, ErrNotCall)
	case r.state == MsgCancelled:
		if !r.shadow && r.leadState == LeadReplied {
			return ended(LeadReplied)
		}
		return ended(r.cancelReason)
	case r.state == MsgDone && first != "":
		var advanced bool
		if err := q.db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM outreach_message WHERE lead_id = $1 AND batch_id = $2 AND step > $3
			   AND (state <> 'approved' OR due_at IS NOT NULL))`, r.leadID, r.batchID, r.step).Scan(&advanced); err != nil {
			return err
		}
		if advanced {
			return fmt.Errorf("already recorded as %s: %w", first, ErrCallAdvanced)
		}
		return nil
	case r.state != MsgApproved:
		return fmt.Errorf("message %d is %s: %w", id, r.state, ErrMessageState)
	case !r.shadow && slices.Contains([]string{LeadReplied, LeadOptedOut, LeadBounced, LeadSuppressed, LeadClosed}, r.leadState):
		return ended(r.leadState)
	}
	// Not due while an earlier step is unsent, whatever the due time says: campaign.Next
	// counts from the last completed step, so an outcome here would cancel and schedule
	// nothing and leave the later emails to go out.
	nd := &NotDueError{WaitsOnStep: -1}
	err := q.db.QueryRow(ctx,
		`SELECT w.step, w.channel FROM outreach_message m JOIN outreach_message w ON `+pendingBefore+`
		 WHERE m.id = $1 ORDER BY w.step DESC LIMIT 1`, id).Scan(&nd.WaitsOnStep, &nd.WaitsOnChannel)
	switch {
	case err == nil:
		return nd
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	case r.dueAt == nil:
		return nd
	case r.dueAt.After(now):
		nd.DueAt = r.dueAt
		return nd
	}
	return nil
}

// SequenceDecision runs campaign.Next for the lead of message messageID over its rows of that
// message's batch, at now with cal: what send applies (ApplyDecision) after an email step is
// sent or shadow-sent. legacy is true for a batch with no stored sequence (made before 00017):
// the decision is empty and the old follow-up rule (ScheduleFollowup) applies. Run it in the
// transaction that applies the decision.
func (q Queries) SequenceDecision(ctx context.Context, messageID int64, now time.Time, cal campaign.Calendar) (d campaign.Decision, legacy bool, err error) {
	var r callRow
	err = q.db.QueryRow(ctx,
		`SELECT m.lead_id, m.batch_id, coalesce(m.sequence_name, ''), b.sequence
		 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id WHERE m.id = $1`, messageID).
		Scan(&r.leadID, &r.batchID, &r.seqName, &r.sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return campaign.Decision{}, false, fmt.Errorf("leadstore: sequence decision: message %d: %w", messageID, ErrMessageNotFound)
	}
	if err != nil {
		return campaign.Decision{}, false, fmt.Errorf("leadstore: sequence decision: %w", err)
	}
	if seqs, err := decodeSequences(r.sequence); err != nil {
		return campaign.Decision{}, false, fmt.Errorf("leadstore: sequence decision: %w", err)
	} else if seqs == nil {
		return campaign.Decision{Due: map[int64]time.Time{}, Cancel: map[int64]string{}}, true, nil
	}
	if cal == nil {
		return campaign.Decision{}, false, errors.New("leadstore: sequence decision: no calendar")
	}
	if d, err = q.leadDecision(ctx, r, now, cal); err != nil {
		return campaign.Decision{}, false, fmt.Errorf("leadstore: sequence decision: %w", err)
	}
	return d, false, nil
}

// leadDecision runs campaign.Next over call r's lead's rows of its batch.
func (q Queries) leadDecision(ctx context.Context, r callRow, now time.Time, cal campaign.Calendar) (campaign.Decision, error) {
	seqs, err := decodeSequences(r.sequence)
	if err != nil {
		return campaign.Decision{}, err
	}
	seq := seqs.For(r.seqName)
	if seq == nil {
		return campaign.Decision{}, fmt.Errorf("batch %d holds no sequence %q for the call", r.batchID, r.seqName)
	}
	rows, outcomes, err := q.engineRows(ctx, r.leadID, r.batchID)
	if err != nil {
		return campaign.Decision{}, err
	}
	return campaign.Next(seq, rows, outcomes, now, cal), nil
}

// engineRows returns lead leadID's rows of batch batchID and every outcome on them, as
// campaign.Next reads them (outcomes oldest first, so the latest wins a tie).
func (q Queries) engineRows(ctx context.Context, leadID, batchID int64) ([]campaign.Row, []campaign.Outcome, error) {
	rows, err := collect(ctx, q.db, func(row pgx.CollectableRow) (campaign.Row, error) {
		var r campaign.Row
		err := row.Scan(&r.MessageID, &r.Step, &r.State, &r.CancelReason, &r.SentAt, &r.DueAt)
		return r, err
	}, `SELECT id, step, state, cancel_reason, sent_at, due_at FROM outreach_message
	    WHERE lead_id = $1 AND batch_id = $2 ORDER BY step`, leadID, batchID)
	if err != nil {
		return nil, nil, err
	}
	outcomes, err := collect(ctx, q.db, func(row pgx.CollectableRow) (campaign.Outcome, error) {
		var o campaign.Outcome
		err := row.Scan(&o.MessageID, &o.Outcome, &o.EmailConsent, &o.At)
		return o, err
	}, `SELECT c.message_id, c.outcome, c.email_consent, c.at FROM call_outcome c
	    JOIN outreach_message m ON m.id = c.message_id
	    WHERE m.lead_id = $1 AND m.batch_id = $2 ORDER BY c.id`, leadID, batchID)
	if err != nil {
		return nil, nil, err
	}
	return rows, outcomes, nil
}

// CallRow is one call step with what the call card and the lists show: the org, the number
// and where it was read, the card text and the latest outcome.
type CallRow struct {
	MessageID       int64      `db:"message_id" json:"message_id"`
	LeadID          int64      `db:"lead_id" json:"lead_id"`
	BatchID         int64      `db:"batch_id" json:"batch_id"`
	CampaignID      int64      `db:"campaign_id" json:"campaign_id"`
	OrgID           int64      `db:"org_id" json:"org_id"`
	OrgName         string     `db:"org_name" json:"org_name"`
	OrgDomain       string     `db:"org_domain" json:"org_domain"`
	Country         string     `db:"country" json:"country"`         // ISO 3166-1 alpha-2, "" when unknown
	Region          string     `db:"region" json:"region,omitempty"` // ISO 3166-2 ("US-IL"), "" when unknown (00021)
	Step            int        `db:"step" json:"step"`
	State           string     `db:"state" json:"state"`
	CancelReason    string     `db:"cancel_reason" json:"cancel_reason,omitempty"`
	DueAt           *time.Time `db:"due_at" json:"due_at,omitempty"`
	Shadow          bool       `db:"shadow" json:"shadow"`
	Subject         string     `db:"subject" json:"subject"` // "Call <org name>"
	Card            string     `db:"card" json:"card"`       // the call card text (talking points, one per line)
	PhoneFactID     *int64     `db:"phone_fact_id" json:"phone_fact_id,omitempty"`
	Phone           string     `db:"phone" json:"phone"`                       // as shown on the page; "" when the fact was erased
	PhoneNormalized string     `db:"phone_normalized" json:"phone_normalized"` // + and digits, for a tel: link
	SourceURL       string     `db:"source_url" json:"source_url"`             // the page the number was read from
	FetchedAt       *time.Time `db:"fetched_at" json:"fetched_at,omitempty"`   // when that page was read
	DerivedBy       string     `db:"derived_by" json:"derived_by"`             // enrich@1, research@1, ...
	Outcome         string     `db:"outcome" json:"outcome,omitempty"`         // the latest outcome; "" before any
	OutcomeAt       *time.Time `db:"outcome_at" json:"outcome_at,omitempty"`
	Note            string     `db:"note" json:"note,omitempty"`
	// PrevStep and PrevChannel are the step this call waits on: the lead's nearest earlier
	// row in the batch that was not cancelled; -1 and "" when there is none.
	PrevStep    int    `db:"prev_step" json:"prev_step"`
	PrevChannel string `db:"prev_channel" json:"prev_channel,omitempty"`
	// WaitsOnStep and WaitsOnChannel are the nearest earlier row of the lead in the batch that
	// is not yet sent, shadow-sent, done or cancelled: while there is one the call is not due,
	// whatever its due time says (RecordCallOutcome refuses it); its 0-based step. nil and ""
	// when there is none.
	WaitsOnStep    *int   `db:"waits_on_step" json:"waits_on_step,omitempty"`
	WaitsOnChannel string `db:"waits_on_channel" json:"waits_on_channel,omitempty"`
}

const callRowSQL = `SELECT m.id AS message_id, m.lead_id, m.batch_id, b.campaign_id, o.id AS org_id, o.name AS org_name,
	  o.domain AS org_domain, o.country, o.region, m.step, m.state, m.cancel_reason, m.due_at, b.shadow, m.subject,
	  m.body_text AS card, m.phone_fact_id, coalesce(f.value, '') AS phone, coalesce(f.normalized, '') AS phone_normalized,
	  coalesce(h.url, '') AS source_url, h.fetched_at, coalesce(f.derived_by, '') AS derived_by,
	  coalesce(co.outcome, '') AS outcome, co.at AS outcome_at, coalesce(co.note, '') AS note,
	  coalesce(pv.step, -1) AS prev_step, coalesce(pv.channel, '') AS prev_channel,
	  wt.step AS waits_on_step, coalesce(wt.channel, '') AS waits_on_channel
	 FROM outreach_message m
	 JOIN outreach_batch b ON b.id = m.batch_id
	 JOIN lead l ON l.id = m.lead_id
	 JOIN org o ON o.id = l.org_id
	 LEFT JOIN org_fact f ON f.id = m.phone_fact_id
	 LEFT JOIN http_fetch h ON h.id = f.fetch_id
	 LEFT JOIN LATERAL (SELECT c.outcome, c.at, c.note FROM call_outcome c WHERE c.message_id = m.id
	   ORDER BY c.id DESC LIMIT 1) co ON true
	 LEFT JOIN LATERAL (SELECT p.step, p.channel FROM outreach_message p WHERE p.lead_id = m.lead_id
	   AND p.batch_id = m.batch_id AND p.step < m.step AND p.state <> 'cancelled' ORDER BY p.step DESC LIMIT 1) pv ON true
	 LEFT JOIN LATERAL (SELECT w.step, w.channel FROM outreach_message w WHERE ` + pendingBefore + `
	   ORDER BY w.step DESC LIMIT 1) wt ON true
	 WHERE m.channel = 'phone'`

// CallsDue returns the approved call steps of approved batches of account accountID (0:
// DefaultAccountID) due at or before now, of campaign campaignID (0: every campaign), shadow
// and live, oldest due first. Whether a call may be made now (the call window, the call rules)
// is the caller's to judge.
func (q Queries) CallsDue(ctx context.Context, accountID, campaignID int64, now time.Time) ([]CallRow, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[CallRow],
		callRowSQL+` AND m.state = 'approved' AND b.approved_at IS NOT NULL AND b.account_id = $1
		   AND ($2::bigint = 0 OR b.campaign_id = $2::bigint) AND m.due_at IS NOT NULL AND m.due_at <= $3
		 ORDER BY m.due_at, m.id`, orAccount(accountID), campaignID, now)
	if err != nil {
		return nil, fmt.Errorf("leadstore: calls due: %w", err)
	}
	return rows, nil
}

// TodayWaitingFew is how many waiting calls Today lists beside their count.
const TodayWaitingFew = 10

// CallsWaiting returns the approved call steps of approved batches of account accountID (0:
// DefaultAccountID) and campaign campaignID (0: every campaign) that are not due at now: no
// due time yet (they wait on an earlier step) or one still to come. The first limit, those
// with a due time first (soonest first), then by id; and how many there are in all.
func (q Queries) CallsWaiting(ctx context.Context, accountID, campaignID int64, now time.Time, limit int) ([]CallRow, int, error) {
	const where = ` AND m.state = 'approved' AND b.approved_at IS NOT NULL AND b.account_id = $1
		   AND ($2::bigint = 0 OR b.campaign_id = $2::bigint) AND (m.due_at IS NULL OR m.due_at > $3)`
	var n int
	if err := q.db.QueryRow(ctx, `SELECT count(*) FROM (`+callRowSQL+where+`) x`, orAccount(accountID), campaignID, now).Scan(&n); err != nil {
		return nil, 0, fmt.Errorf("leadstore: calls waiting: %w", err)
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[CallRow],
		callRowSQL+where+` ORDER BY m.due_at NULLS LAST, m.id LIMIT $4`, orAccount(accountID), campaignID, now, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("leadstore: calls waiting: %w", err)
	}
	return rows, n, nil
}

// CallByID returns call step messageID of account accountID (0: DefaultAccountID) in any
// state, for the call card; ErrMessageNotFound when there is none (or it is not a call).
func (q Queries) CallByID(ctx context.Context, accountID, messageID int64) (CallRow, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[CallRow],
		callRowSQL+` AND m.id = $1 AND b.account_id = $2`, messageID, orAccount(accountID))
	if err != nil {
		return CallRow{}, fmt.Errorf("leadstore: call %d: %w", messageID, err)
	}
	if len(rows) == 0 {
		return CallRow{}, fmt.Errorf("leadstore: call %d: %w", messageID, ErrMessageNotFound)
	}
	return rows[0], nil
}

// TodayItem is one email or manual (form, LinkedIn) step due today.
type TodayItem struct {
	MessageID  int64     `db:"message_id" json:"message_id"`
	LeadID     int64     `db:"lead_id" json:"lead_id"`
	BatchID    int64     `db:"batch_id" json:"batch_id"`
	CampaignID int64     `db:"campaign_id" json:"campaign_id"`
	OrgName    string    `db:"org_name" json:"org_name"`
	OrgDomain  string    `db:"org_domain" json:"org_domain"`
	Country    string    `db:"country" json:"country"`
	Channel    string    `db:"channel" json:"channel"` // email | form | linkedin
	Step       int       `db:"step" json:"step"`
	DueAt      time.Time `db:"due_at" json:"due_at"`
	Shadow     bool      `db:"shadow" json:"shadow"`
}

// TodayLead is one lead whose reply waits for the operator (state replied).
type TodayLead struct {
	LeadID     int64     `db:"lead_id" json:"lead_id"`
	CampaignID int64     `db:"campaign_id" json:"campaign_id"`
	OrgName    string    `db:"org_name" json:"org_name"`
	OrgDomain  string    `db:"org_domain" json:"org_domain"`
	Country    string    `db:"country" json:"country"`
	RepliedAt  time.Time `db:"replied_at" json:"replied_at"`
}

// TodayFirstFew is how many emails due today Today lists beside their count.
const TodayFirstFew = 5

// Today is the operator's day, decision first: calls due now and overdue, replies waiting,
// emails the send job sends today (count and the first few), manual items due.
type Today struct {
	Now            time.Time   `json:"now"`
	CallsDueNow    []CallRow   `json:"calls_due_now"` // due since the start of today, up to now
	CallsOverdue   []CallRow   `json:"calls_overdue"` // due before today
	CallsWaiting   []CallRow   `json:"calls_waiting"` // not due yet: the first TodayWaitingFew
	CallsWaitingN  int         `json:"calls_waiting_count"`
	RepliesWaiting []TodayLead `json:"replies_waiting"` // leads in state replied, newest first
	EmailsDueCount int         `json:"emails_due_count"`
	EmailsDue      []TodayItem `json:"emails_due"` // the first TodayFirstFew, oldest due first
	Manual         []TodayItem `json:"manual"`     // form and LinkedIn steps due by the end of today
}

// TodayView returns Today for account accountID (0: DefaultAccountID) and campaign campaignID
// (0: every campaign) at now. "Today" is now's calendar day in now's location; shadow and
// live rows both show (each carries Shadow).
func (q Queries) TodayView(ctx context.Context, accountID, campaignID int64, now time.Time) (Today, error) {
	accountID = orAccount(accountID)
	y, mo, d := now.Date()
	dayStart := time.Date(y, mo, d, 0, 0, 0, 0, now.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	t := Today{Now: now, CallsDueNow: []CallRow{}, CallsOverdue: []CallRow{}}
	calls, err := q.CallsDue(ctx, accountID, campaignID, now)
	if err != nil {
		return Today{}, fmt.Errorf("leadstore: today: %w", err)
	}
	if t.CallsWaiting, t.CallsWaitingN, err = q.CallsWaiting(ctx, accountID, campaignID, now, TodayWaitingFew); err != nil {
		return Today{}, fmt.Errorf("leadstore: today: %w", err)
	}
	for _, c := range calls {
		if c.DueAt.Before(dayStart) {
			t.CallsOverdue = append(t.CallsOverdue, c)
		} else {
			t.CallsDueNow = append(t.CallsDueNow, c)
		}
	}
	if t.RepliesWaiting, err = collect(ctx, q.db, pgx.RowToStructByName[TodayLead],
		`SELECT l.id AS lead_id, l.campaign_id, o.name AS org_name, o.domain AS org_domain, o.country, l.state_at AS replied_at
		 FROM lead l JOIN org o ON o.id = l.org_id JOIN campaign c ON c.id = l.campaign_id
		 WHERE l.state = 'replied' AND c.account_id = $1 AND ($2::bigint = 0 OR l.campaign_id = $2::bigint)
		 ORDER BY l.state_at DESC, l.id`, accountID, campaignID); err != nil {
		return Today{}, fmt.Errorf("leadstore: today: replies: %w", err)
	}
	const itemSQL = `SELECT m.id AS message_id, m.lead_id, m.batch_id, b.campaign_id, o.name AS org_name,
	    o.domain AS org_domain, o.country, m.channel, m.step, m.due_at, b.shadow
	   FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
	   JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
	   WHERE m.state = 'approved' AND b.approved_at IS NOT NULL AND b.account_id = $1
	     AND ($2::bigint = 0 OR b.campaign_id = $2::bigint) AND m.due_at IS NOT NULL AND m.due_at < $3`
	if err := q.db.QueryRow(ctx,
		`SELECT count(*) FROM (`+itemSQL+` AND m.channel = 'email') x`, accountID, campaignID, dayEnd).
		Scan(&t.EmailsDueCount); err != nil {
		return Today{}, fmt.Errorf("leadstore: today: emails: %w", err)
	}
	if t.EmailsDue, err = collect(ctx, q.db, pgx.RowToStructByName[TodayItem],
		itemSQL+` AND m.channel = 'email' ORDER BY m.due_at, m.id LIMIT $4`, accountID, campaignID, dayEnd, TodayFirstFew); err != nil {
		return Today{}, fmt.Errorf("leadstore: today: emails: %w", err)
	}
	if t.Manual, err = collect(ctx, q.db, pgx.RowToStructByName[TodayItem],
		itemSQL+` AND m.channel IN ('form', 'linkedin') ORDER BY m.due_at, m.id`, accountID, campaignID, dayEnd); err != nil {
		return Today{}, fmt.Errorf("leadstore: today: manual: %w", err)
	}
	return t, nil
}

// SequenceStep is one step of a lead's sequence in its latest batch, as the lead page shows it.
type SequenceStep struct {
	BatchID           int64      `json:"batch_id"`
	Shadow            bool       `json:"shadow"`
	Step              int        `json:"step"`
	Type              string     `json:"type"`              // email | call (campaign.Step* values)
	Channel           string     `json:"channel,omitempty"` // the row's channel; "" when the step has no row
	When              string     `json:"when"`              // the step's condition
	AfterBusinessDays int        `json:"after_business_days"`
	MessageID         int64      `json:"message_id,omitempty"`
	State             string     `json:"state"` // the row's state; "skipped" when the step has no row
	DueAt             *time.Time `json:"due_at,omitempty"`
	SentAt            *time.Time `json:"sent_at,omitempty"` // sent, shadow-sent or done
	CancelReason      string     `json:"cancel_reason,omitempty"`
	Outcome           string     `json:"outcome,omitempty"` // a call's latest outcome
	OutcomeAt         *time.Time `json:"outcome_at,omitempty"`
	Note              string     `json:"note,omitempty"`
	Skipped           string     `json:"skipped,omitempty"` // why a step has no row: NoNumber, or "not drafted"
}

// SequenceOf returns each step of lead leadID's latest batch (the newest one not discarded, else
// the newest), in step order: the batch's frozen sequence for the lead, each step with its row
// when it has one. A step with no row reads NoNumber when the sequence has a call step (no
// callable number at batch time), else "not drafted". A legacy batch (no stored sequence)
// lists its rows as they are. No batch: an empty list.
func (q Queries) SequenceOf(ctx context.Context, leadID int64) ([]SequenceStep, error) {
	var (
		batchID int64
		shadow  bool
		rawSeq  []byte
	)
	err := q.db.QueryRow(ctx,
		`SELECT b.id, b.shadow, b.sequence FROM outreach_batch b
		 WHERE b.id IN (SELECT m.batch_id FROM outreach_message m WHERE m.lead_id = $1)
		 ORDER BY (b.discarded_at IS NULL) DESC, b.id DESC LIMIT 1`, leadID).Scan(&batchID, &shadow, &rawSeq)
	if errors.Is(err, pgx.ErrNoRows) {
		return []SequenceStep{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("leadstore: sequence of lead %d: %w", leadID, err)
	}
	type row struct {
		ID           int64      `db:"id"`
		Step         int        `db:"step"`
		Channel      string     `db:"channel"`
		Condition    string     `db:"condition"`
		SequenceName string     `db:"sequence_name"`
		State        string     `db:"state"`
		DueAt        *time.Time `db:"due_at"`
		SentAt       *time.Time `db:"sent_at"`
		CancelReason string     `db:"cancel_reason"`
		Outcome      string     `db:"outcome"`
		OutcomeAt    *time.Time `db:"outcome_at"`
		Note         string     `db:"note"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT m.id, m.step, m.channel, m.condition, coalesce(m.sequence_name, '') AS sequence_name, m.state,
		   m.due_at, m.sent_at, m.cancel_reason, coalesce(co.outcome, '') AS outcome, co.at AS outcome_at,
		   coalesce(co.note, '') AS note
		 FROM outreach_message m
		 LEFT JOIN LATERAL (SELECT c.outcome, c.at, c.note FROM call_outcome c WHERE c.message_id = m.id
		   ORDER BY c.id DESC LIMIT 1) co ON true
		 WHERE m.lead_id = $1 AND m.batch_id = $2 ORDER BY m.step`, leadID, batchID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: sequence of lead %d: %w", leadID, err)
	}
	fill := func(s *SequenceStep, r row) {
		s.Channel, s.MessageID, s.State, s.DueAt, s.SentAt = r.Channel, r.ID, r.State, r.DueAt, r.SentAt
		s.CancelReason, s.Outcome, s.OutcomeAt, s.Note = r.CancelReason, r.Outcome, r.OutcomeAt, r.Note
	}
	stepType := func(channel string) string {
		if channel == ChannelPhone {
			return campaign.StepCall
		}
		return campaign.StepEmail
	}
	seqs, err := decodeSequences(rawSeq)
	if err != nil {
		return nil, fmt.Errorf("leadstore: sequence of lead %d: %w", leadID, err)
	}
	name := ""
	if len(rows) > 0 {
		name = rows[0].SequenceName
	}
	seq := seqs.For(name)
	out := []SequenceStep{}
	if seq == nil { // a legacy batch: its rows as they are
		for _, r := range rows {
			s := SequenceStep{BatchID: batchID, Shadow: shadow, Step: r.Step, Type: stepType(r.Channel), When: r.Condition}
			fill(&s, r)
			out = append(out, s)
		}
		return out, nil
	}
	byStep := make(map[int]row, len(rows))
	for _, r := range rows {
		byStep[r.Step] = r
	}
	hasCall := slices.ContainsFunc(seq, func(st campaign.Step) bool { return st.Type == campaign.StepCall })
	for _, st := range seq {
		when := st.When
		if when == "" {
			when = campaign.WhenAlways
		}
		s := SequenceStep{BatchID: batchID, Shadow: shadow, Step: st.Step, Type: st.Type, When: when, AfterBusinessDays: st.AfterBusinessDays}
		if r, ok := byStep[st.Step]; ok {
			fill(&s, r)
		} else {
			s.State = MsgSkipped
			s.Skipped = "not drafted"
			if hasCall {
				s.Skipped = NoNumber
			}
		}
		out = append(out, s)
	}
	return out, nil
}
