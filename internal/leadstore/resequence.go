package leadstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/campaign"
)

// A sequence moves on when a step is sent (send) or a call gets its outcome
// (RecordCallOutcome): both run campaign.Next in the same transaction. Anything else that
// takes a pending row out of a sequence must run it too, or the steps after that row wait
// for ever: a phone suppression (Suppress kind phone: the TPS listed import, do_not_call on
// another lead, `leads suppress phone`, forget) cancelling a pending call, and a number found
// wrong cancelling the other pending calls on the same phone fact. Those writers hold no
// calendar (the lead's location and the business hours live in leads.yaml), so they run the
// engine with none: its cancels apply at once, and a due time it would set waits for
// HealSequences, which send runs at its start with the real calendar (and the commands that
// write such a suppression run straight after).

// LeadCalendar is the business-day calendar for a lead of kind (lead.kind) whose org is in
// country (ISO alpha-2, "" when unknown); the caller builds it as send does.
type LeadCalendar func(kind, country string) campaign.Calendar

// noSlot finds no slot: under it the engine sets no due time, and its cancels still apply.
var noSlot = campaign.CalendarFunc(func(time.Time, int) time.Time { return time.Time{} })

// seqTarget is one lead's rows of one approved batch that stores a sequence.
type seqTarget struct {
	LeadID   int64  `db:"lead_id"`
	BatchID  int64  `db:"batch_id"`
	SeqName  string `db:"seq_name"`
	Sequence []byte `db:"sequence"`
	Kind     string `db:"kind"`
	Country  string `db:"country"`
}

// resequence runs campaign.Next for each of leadIDs over its rows of every approved,
// undiscarded batch that stores a sequence and still holds an approved row of it, at now,
// and applies the decision (ApplyDecision). cal nil: no due time is set (noSlot). Run it in
// a transaction that holds the leads' locks (lead first, then its messages).
func (q Queries) resequence(ctx context.Context, leadIDs []int64, now time.Time, cal LeadCalendar) (DecisionApplied, error) {
	var res DecisionApplied
	if len(leadIDs) == 0 {
		return res, nil
	}
	ids := slices.Clone(leadIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	targets, err := collect(ctx, q.db, pgx.RowToStructByName[seqTarget],
		`SELECT DISTINCT ON (m.lead_id, m.batch_id) m.lead_id, m.batch_id, coalesce(m.sequence_name, '') AS seq_name,
		   b.sequence, coalesce(l.kind, '') AS kind, coalesce(o.country, '') AS country
		 FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id
		 JOIN lead l ON l.id = m.lead_id
		 JOIN org o ON o.id = l.org_id
		 WHERE m.lead_id = ANY($1) AND m.state = 'approved' AND b.approved_at IS NOT NULL
		   AND b.discarded_at IS NULL AND b.sequence IS NOT NULL
		 ORDER BY m.lead_id, m.batch_id, m.step`, ids)
	if err != nil {
		return res, err
	}
	for _, t := range targets {
		c := campaign.Calendar(noSlot)
		if cal != nil {
			if c = cal(t.Kind, t.Country); c == nil {
				c = noSlot
			}
		}
		d, err := q.leadDecision(ctx, callRow{leadID: t.LeadID, batchID: t.BatchID, seqName: t.SeqName, sequence: t.Sequence}, now, c)
		if err != nil {
			return res, err
		}
		a, err := q.ApplyDecision(ctx, d)
		if err != nil {
			return res, err
		}
		res.Scheduled += a.Scheduled
		res.Cancelled += a.Cancelled
	}
	return res, nil
}

// HealSequences finds every lead whose sequence the engine could move on but nobody ran it
// for (an approved row with no due time whose earlier steps are all sent, done, cancelled or
// skipped: a call taken out by a suppression or a wrong number, or a follow-up whose
// scheduling failed after its send) and runs campaign.Next for it with cal at now, applying
// the decision. Send runs it at its start; it is safe to run at any time (Next only sets a
// due time where none is, and cancels only rows whose condition fails). It runs in one
// transaction (a savepoint when q already runs in one) and locks each lead, then its
// messages, as RecordCallOutcome and Suppress do.
func (q Queries) HealSequences(ctx context.Context, now time.Time, cal LeadCalendar) (DecisionApplied, error) {
	if cal == nil {
		return DecisionApplied{}, fmt.Errorf("leadstore: heal sequences: no calendar")
	}
	var res DecisionApplied
	err := q.inTx(ctx, func(tq Queries) error {
		ids, err := collect(ctx, tq.db, pgx.RowTo[int64],
			`SELECT DISTINCT m.lead_id FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
			 WHERE m.state = 'approved' AND m.due_at IS NULL AND b.approved_at IS NOT NULL
			   AND b.discarded_at IS NULL AND b.sequence IS NOT NULL
			   AND EXISTS (SELECT 1 FROM outreach_message w WHERE w.lead_id = m.lead_id AND w.batch_id = m.batch_id AND w.step < m.step)
			   AND NOT EXISTS (SELECT 1 FROM outreach_message w WHERE w.lead_id = m.lead_id AND w.batch_id = m.batch_id
			     AND w.step < m.step AND w.state NOT IN ('sent', 'shadow_sent', 'done', 'cancelled', 'skipped'))
			 ORDER BY 1`)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if _, err := tq.db.Exec(ctx, `SELECT 1 FROM lead WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids); err != nil {
			return err
		}
		if _, err := tq.db.Exec(ctx, `SELECT 1 FROM outreach_message WHERE lead_id = ANY($1) ORDER BY lead_id, id FOR UPDATE`, ids); err != nil {
			return err
		}
		res, err = tq.resequence(ctx, ids, now, cal)
		return err
	})
	if err != nil {
		return DecisionApplied{}, fmt.Errorf("leadstore: heal sequences: %w", err)
	}
	return res, nil
}

// cancelCallsOnFact cancels the pending (draft or approved) call rows that dial phone fact
// factID, bar message except (0: none), with reason, and runs the engine (no calendar) for
// each lead it touched, so their later steps move on.
func (q Queries) cancelCallsOnFact(ctx context.Context, factID, except int64, reason string) error {
	ids, err := collect(ctx, q.db, pgx.RowTo[int64],
		`UPDATE outreach_message SET state = 'cancelled', cancel_reason = $3
		 WHERE phone_fact_id = $1 AND id <> $2 AND channel = 'phone' AND state IN ('draft', 'approved')
		 RETURNING lead_id`, factID, except, reason)
	if err != nil {
		return err
	}
	_, err = q.resequence(ctx, ids, time.Now(), nil)
	return err
}
