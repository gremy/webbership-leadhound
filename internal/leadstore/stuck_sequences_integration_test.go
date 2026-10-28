//go:build integration

package leadstore_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// plainLeadDays is plainDays for every lead, as send's HealSequences gets it.
var plainLeadDays = leadstore.LeadCalendar(func(string, string) campaign.Calendar { return plainDays })

// heal runs HealSequences at now with plainLeadDays.
func (f outreachFixture) heal(t *testing.T, now time.Time) leadstore.DecisionApplied {
	t.Helper()
	a, err := f.q.HealSequences(context.Background(), now, plainLeadDays)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	return a
}

// seqBatchOnFact drafts the five-step batch for lead dialling fact (approve: approve it,
// step 0 due at dueBase).
func (f outreachFixture) seqBatchOnFact(t *testing.T, shadow bool, lead, fact int64, approve bool) seqLead {
	t.Helper()
	ctx := context.Background()
	id, sum, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}},
		shadow, f.seqDrafts(t, lead, fact), []byte("t"))
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	s := seqLead{lead: lead, batch: id, msg: map[int]int64{}, phoneFact: fact}
	if approve {
		f.approveSeq(t, s, hex.EncodeToString(sum))
	}
	ms, err := f.q.BatchMessages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		s.msg[m.Step] = m.ID
	}
	return s
}

func (f outreachFixture) approveSeq(t *testing.T, s seqLead, hash string) leadstore.Approval {
	t.Helper()
	a, err := f.q.ApproveBatch(context.Background(), s.batch, hash, nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return dueBase
		}
		return time.Time{}
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	return a
}

// wantEnds checks the end of a sequence whose call was taken out: step 3 ("tried to reach
// you") cancelled for its condition, step 4 due five days after step 1's send (day 3), and,
// once step 4 is sent, nothing of the lead left pending, so OpenElsewhere holds no other
// campaign for it.
func (f outreachFixture) wantEnds(t *testing.T, s seqLead, shadow bool) {
	t.Helper()
	ctx := context.Background()
	if r := f.rowOf(t, s.msg[3]); r.state != leadstore.MsgCancelled || r.reason != campaign.CancelCondition {
		t.Errorf("step 3 = %+v, want cancelled (condition)", r)
	}
	if r := f.rowOf(t, s.msg[4]); r.state != leadstore.MsgApproved || r.due == nil || !r.due.Equal(day(8)) {
		t.Fatalf("step 4 = %+v, want approved and due %v (step 1's send + 5)", r, day(8))
	}
	f.send(t, s, shadow, 4, day(8))
	var pending int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM outreach_message WHERE lead_id = $1
		AND state IN ('draft', 'approved', 'sending', 'maybe_sent')`, s.lead).Scan(&pending); err != nil || pending != 0 {
		t.Errorf("pending rows after the last step = %d, %v; want 0 (the sequence ended)", pending, err)
	}
	var org, camp int64
	if err := f.owner.QueryRow(ctx, `SELECT org_id, campaign_id FROM lead WHERE id = $1`, s.lead).Scan(&org, &camp); err != nil {
		t.Fatal(err)
	}
	other := ownerInt(t, f.owner, `INSERT INTO campaign (account_id, slug, name) VALUES (1, $1, 'Other') RETURNING id`, fmt.Sprintf("other-%d", s.lead))
	why, err := f.q.OpenElsewhere(ctx, 0, other, []int64{org})
	if err != nil || why[org] != "" {
		t.Errorf("open elsewhere = %v, %v; want the org free once its sequence ended", why, err)
	}
	_ = camp
}

// TestStuckSequences: a call row taken out of a sequence by a suppression or a wrong number
// never holds up the rest of it: the final email falls due and the sequence can end.
func TestStuckSequences(t *testing.T) {
	ctx := context.Background()

	t.Run("suppressed between batch and approval", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.siteLead(t, "stuck-approval.localhost", nil, 7, "info@stuck-approval.localhost")
		fact := f.phoneFact(t, lead, callNumber)
		id, sum, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}},
			false, f.seqDrafts(t, lead, fact), []byte("t"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.q.Suppress(ctx, 0, leadstore.SuppressPhone, leadstore.PhoneSuppressionValue(callNumber), leadstore.ReasonTPS, "test"); err != nil {
			t.Fatal(err)
		}
		s := seqLead{lead: lead, batch: id, msg: map[int]int64{}, phoneFact: fact}
		ms, err := f.q.BatchMessages(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ms {
			s.msg[m.Step] = m.ID
		}
		f.approveSeq(t, s, hex.EncodeToString(sum))
		if r := f.rowOf(t, s.msg[2]); r.state == leadstore.MsgApproved {
			t.Fatalf("call row = %+v, want skipped or cancelled (suppressed)", r)
		}
		f.send(t, s, false, 0, day(0))
		f.send(t, s, false, 1, day(3))
		f.wantEnds(t, s, false)
	})

	t.Run("phone suppressed mid-sequence", func(t *testing.T) {
		f := newOutreachFixture(t)
		s := f.seqBatch(t, false, f.siteLead(t, "stuck-mid.localhost", nil, 7, "info@stuck-mid.localhost"))
		f.send(t, s, false, 0, day(0))
		f.send(t, s, false, 1, day(3))
		if r := f.rowOf(t, s.msg[2]); r.due == nil || !r.due.Equal(day(5)) {
			t.Fatalf("call due = %+v, want %v", r, day(5))
		}
		res, err := f.q.Suppress(ctx, 0, leadstore.SuppressPhone, leadstore.PhoneSuppressionValue(callNumber), leadstore.ReasonDoNotCall, "test")
		if err != nil || res.Cancelled != 1 {
			t.Fatalf("suppress: %+v %v", res, err)
		}
		if r := f.rowOf(t, s.msg[3]); r.state != leadstore.MsgCancelled {
			t.Errorf("step 3 right after the suppression = %+v, want cancelled (the engine ran)", r)
		}
		if a := f.heal(t, day(6)); a.Scheduled != 1 {
			t.Errorf("heal = %+v, want step 4 scheduled", a)
		}
		f.wantEnds(t, s, false)
		if a := f.heal(t, day(9)); a != (leadstore.DecisionApplied{}) {
			t.Errorf("a second heal = %+v, want nothing to do", a)
		}
	})

	t.Run("wrong number on another lead's shared number", func(t *testing.T) {
		f := newOutreachFixture(t)
		a := f.seqBatch(t, true, f.siteLead(t, "stuck-shared.localhost", nil, 7, "info@stuck-shared.localhost"))
		camp := ownerInt(t, f.owner, `INSERT INTO campaign (account_id, slug, name) VALUES (1, 'shared-second', 'Second') RETURNING id`)
		other := ownerInt(t, f.owner, `INSERT INTO lead (org_id, kind, channel, route_id, campaign_id, finding_id, probe_id, agency_finding_id)
			SELECT org_id, kind, channel, route_id, $2, finding_id, probe_id, agency_finding_id FROM lead WHERE id = $1 RETURNING id`, a.lead, camp)
		b := f.seqBatchOnFact(t, false, other, a.phoneFact, true)
		f.send(t, b, false, 0, day(0))
		f.send(t, b, false, 1, day(3))
		f.send(t, a, true, 0, day(0))
		f.send(t, a, true, 1, day(3))
		if _, err := f.q.RecordCallOutcome(ctx, 0, a.msg[2], campaign.OutcomeWrongNumber, "", nil, leadstore.ActorGremy, day(5), plainDays); err != nil {
			t.Fatal(err)
		}
		if r := f.rowOf(t, b.msg[2]); r.state != leadstore.MsgCancelled || r.reason != campaign.CancelWrongNumber {
			t.Errorf("the other lead's call = %+v, want cancelled (wrong_number)", r)
		}
		f.heal(t, day(6))
		f.wantEnds(t, b, false)
	})
}

// TestDoNotCallEveryForm: do_not_call on a call dialling a fact kept in national form
// suppresses every form of the number, so another org's call to the same number in E.164
// form is no longer made (its row is cancelled, its sequence moves on).
func TestDoNotCallEveryForm(t *testing.T) {
	ctx := context.Background()
	f := newOutreachFixture(t)
	const national, e164 = "01632960201", "+441632960201"
	la := f.siteLead(t, "dnc-national.localhost", nil, 7, "info@dnc-national.localhost")
	lb := f.siteLead(t, "dnc-e164.localhost", nil, 7, "info@dnc-e164.localhost")
	f.setOrgCountry(t, la, "GB")
	f.setOrgCountry(t, lb, "GB")
	a := f.seqBatchOnFact(t, true, la, f.phoneFact(t, la, national), true)
	b := f.seqBatchOnFact(t, true, lb, f.phoneFact(t, lb, e164), true)
	for _, s := range []seqLead{a, b} {
		f.send(t, s, true, 0, day(0))
		f.send(t, s, true, 1, day(3))
	}
	if _, err := f.q.RecordCallOutcome(ctx, 0, a.msg[2], campaign.OutcomeDoNotCall, "", nil, leadstore.ActorGremy, day(5), plainDays); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{national, e164} {
		if got := f.suppressions(t)["phone "+leadstore.PhoneSuppressionValue(n)]; got != leadstore.ReasonDoNotCall {
			t.Errorf("suppression of %s = %q, want do_not_call", n, got)
		}
	}
	if r := f.rowOf(t, b.msg[2]); r.state != leadstore.MsgCancelled {
		t.Errorf("the other org's call to the E.164 form = %+v, want cancelled", r)
	}
	calls, err := f.q.CallsDue(ctx, 0, 0, day(6))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if c.MessageID == b.msg[2] {
			t.Errorf("the other org's call is still callable: %+v", c)
		}
	}
	f.heal(t, day(6))
	f.wantEnds(t, b, true)
}
