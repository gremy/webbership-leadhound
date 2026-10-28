//go:build integration

package leadstore_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// fiveSteps is the mixed sequence: two emails, a call, "tried to reach you" when the call did
// not connect, and a final email.
var fiveSteps = []campaign.Step{
	{Step: 0, Type: campaign.StepEmail, When: campaign.WhenAlways},
	{Step: 1, Type: campaign.StepEmail, When: campaign.WhenAlways, AfterBusinessDays: 3},
	{Step: 2, Type: campaign.StepCall, When: campaign.WhenHasCallablePhone, AfterBusinessDays: 2},
	{Step: 3, Type: campaign.StepEmail, When: campaign.WhenCallNotConnected, AfterBusinessDays: 1},
	{Step: 4, Type: campaign.StepEmail, When: campaign.WhenAlways, AfterBusinessDays: 5},
}

// plainDays is a calendar that counts every day.
var plainDays = campaign.CalendarFunc(func(from time.Time, n int) time.Time { return from.AddDate(0, 0, n) })

const callNumber = "+441632960001"

// seqLead is one lead on the five-step sequence: its id, batch and message ids by step.
type seqLead struct {
	lead, batch int64
	msg         map[int]int64
	phoneFact   int64
}

// phoneFact adds callNumber as a phone fact of lead's org and returns its id.
func (f outreachFixture) phoneFact(t *testing.T, lead int64, number string) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID int64
	if err := f.owner.QueryRow(ctx, `SELECT org_id FROM lead WHERE id = $1`, lead).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.InsertFact(ctx, leadstore.FactRow{OrgID: orgID, Kind: leadstore.FactPhone, Value: number,
		Normalized: number, FetchID: f.fid, DerivedBy: "enrich@1"}); err != nil {
		t.Fatalf("insert fact: %v", err)
	}
	var id int64
	if err := f.owner.QueryRow(ctx, `SELECT id FROM org_fact WHERE org_id = $1 AND normalized = $2`, orgID, number).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seqDrafts returns the five steps of lead (the call row dialling fact).
func (f outreachFixture) seqDrafts(t *testing.T, lead, fact int64) []leadstore.DraftRow {
	t.Helper()
	var to string
	if err := f.owner.QueryRow(context.Background(),
		`SELECT coalesce(r.value, '') FROM lead l LEFT JOIN contact_route r ON r.id = l.route_id WHERE l.id = $1`, lead).Scan(&to); err != nil {
		t.Fatal(err)
	}
	mb := f.mailbox(t)
	var out []leadstore.DraftRow
	for _, st := range fiveSteps {
		d := leadstore.DraftRow{LeadID: lead, Step: st.Step, Channel: "email", To: to, Condition: st.When,
			Subject: fmt.Sprintf("Step %d", st.Step), Body: fmt.Sprintf("Body %d of lead %d", st.Step, lead),
			MessageID: fmt.Sprintf("<%d.%d@seq.invalid>", lead, st.Step), MailboxID: mb}
		if st.Type == campaign.StepCall {
			d.Channel, d.To, d.MessageID, d.PhoneFactID = leadstore.ChannelPhone, "", "", fact
			d.Subject, d.Body = "Call Example Org", "Who I am\nWhy I call\nThe free report"
		}
		out = append(out, d)
	}
	return out
}

// seqBatch drafts and approves the five-step batch for lead (step 0 due at dueBase) and
// returns it.
func (f outreachFixture) seqBatch(t *testing.T, shadow bool, lead int64) seqLead {
	t.Helper()
	ctx := context.Background()
	fact := f.phoneFact(t, lead, callNumber)
	id, sum, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}},
		shadow, f.seqDrafts(t, lead, fact), []byte("t"))
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	a, err := f.q.ApproveBatch(ctx, id, hex.EncodeToString(sum), nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return dueBase
		}
		return time.Time{}
	})
	if err != nil || a.Approved != 5 {
		t.Fatalf("approve: %+v %v", a, err)
	}
	ms, err := f.q.BatchMessages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	s := seqLead{lead: lead, batch: id, msg: map[int]int64{}, phoneFact: fact}
	for _, m := range ms {
		s.msg[m.Step] = m.ID
	}
	return s
}

// send marks step of s sent (or shadow-sent) at at and applies the sequence decision, as send
// does.
func (f outreachFixture) send(t *testing.T, s seqLead, shadow bool, step int, at time.Time) {
	t.Helper()
	ctx := context.Background()
	id := s.msg[step]
	if shadow {
		if err := f.q.MarkShadowSent(ctx, id, at); err != nil {
			t.Fatal(err)
		}
	} else {
		if ok, err := f.q.MarkSending(ctx, id); err != nil || !ok {
			t.Fatalf("mark sending: %v %v", ok, err)
		}
		if err := f.q.MarkSent(ctx, id, "250 ok", at); err != nil {
			t.Fatal(err)
		}
	}
	d, legacy, err := f.q.SequenceDecision(ctx, id, at, plainDays)
	if err != nil || legacy {
		t.Fatalf("decision: %v legacy=%v", err, legacy)
	}
	if _, err := f.q.ApplyDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
}

type rowState struct {
	state, reason string
	due           *time.Time
}

func (f outreachFixture) rowOf(t *testing.T, id int64) rowState {
	t.Helper()
	var r rowState
	if err := f.owner.QueryRow(context.Background(),
		`SELECT state, cancel_reason, due_at FROM outreach_message WHERE id = $1`, id).Scan(&r.state, &r.reason, &r.due); err != nil {
		t.Fatal(err)
	}
	return r
}

func day(n int) time.Time { return dueBase.AddDate(0, 0, n) }

func TestCallOutcomeGrid(t *testing.T) {
	ctx := context.Background()

	t.Run("applies when due, refused before, refused again after advance", func(t *testing.T) {
		f := newOutreachFixture(t)
		s := f.seqBatch(t, false, f.siteLead(t, "call-due.localhost", nil, 7, "info@call-due.localhost"))
		f.send(t, s, false, 0, day(0))
		if r := f.rowOf(t, s.msg[1]); r.due == nil || !r.due.Equal(day(3)) {
			t.Fatalf("step 1 due = %v, want %v", r.due, day(3))
		}
		f.send(t, s, false, 1, day(3))
		if r := f.rowOf(t, s.msg[2]); r.due == nil || !r.due.Equal(day(5)) {
			t.Fatalf("call due = %v, want %v", r.due, day(5))
		}
		if _, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeNoAnswer, "", nil, leadstore.ActorGremy, day(4), plainDays); !errors.Is(err, leadstore.ErrNotDue) || !strings.Contains(err.Error(), "not due yet") {
			t.Fatalf("before due: %v, want not due yet", err)
		}
		res, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeNoAnswer, "rang twice", nil, leadstore.ActorGremy, day(5), plainDays)
		if err != nil {
			t.Fatalf("record: %v", err)
		}
		if res.Due[s.msg[3]] == "" || len(res.Cancelled) != 0 {
			t.Errorf("result = %+v, want step 3 scheduled", res)
		}
		if r := f.rowOf(t, s.msg[2]); r.state != leadstore.MsgDone {
			t.Errorf("call row = %+v, want done", r)
		}
		if r := f.rowOf(t, s.msg[3]); r.due == nil || !r.due.Equal(day(6)) {
			t.Errorf("step 3 due = %v, want %v", r.due, day(6))
		}
		var events int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM lead_event WHERE lead_id = $1 AND reason LIKE 'call %'`, s.lead).Scan(&events); err != nil || events != 1 {
			t.Errorf("call lead events = %d, %v; want 1", events, err)
		}
		_, err = f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeVoicemail, "", nil, leadstore.ActorGremy, day(5), plainDays)
		if !errors.Is(err, leadstore.ErrCallAdvanced) || !strings.Contains(err.Error(), "no_answer") {
			t.Fatalf("second outcome: %v, want refused naming no_answer", err)
		}
		var n int
		if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM call_outcome WHERE message_id = $1`, s.msg[2]).Scan(&n); err != nil || n != 1 {
			t.Errorf("outcomes = %d, %v; want 1 (the refused one wrote nothing)", n, err)
		}
		// The read models.
		steps, err := f.q.SequenceOf(ctx, s.lead)
		if err != nil || len(steps) != 5 {
			t.Fatalf("sequence of: %+v %v", steps, err)
		}
		if steps[2].Type != campaign.StepCall || steps[2].Outcome != campaign.OutcomeNoAnswer || steps[2].Note != "rang twice" ||
			steps[3].State != leadstore.MsgApproved || steps[3].DueAt == nil {
			t.Errorf("sequence of = %+v", steps)
		}
	})

	t.Run("refused on an email row", func(t *testing.T) {
		f := newOutreachFixture(t)
		s := f.seqBatch(t, false, f.siteLead(t, "call-email.localhost", nil, 7, "info@call-email.localhost"))
		if _, err := f.q.RecordCallOutcome(ctx, 0, s.msg[0], campaign.OutcomeConnected, "", nil, leadstore.ActorGremy, day(0), plainDays); !errors.Is(err, leadstore.ErrNotCall) {
			t.Fatalf("email row: %v, want ErrNotCall", err)
		}
	})

	t.Run("refused after a reply", func(t *testing.T) {
		f := newOutreachFixture(t)
		s := f.seqBatch(t, false, f.siteLead(t, "call-reply.localhost", nil, 7, "info@call-reply.localhost"))
		f.send(t, s, false, 0, day(0))
		f.send(t, s, false, 1, day(3))
		f.reply(t, s, mailbox.KindReply)
		if r := f.rowOf(t, s.msg[2]); r.state != leadstore.MsgCancelled {
			t.Fatalf("call row after reply = %+v, want cancelled", r)
		}
		_, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeConnected, "", nil, leadstore.ActorGremy, day(5), plainDays)
		if !errors.Is(err, leadstore.ErrSequenceEnded) || !strings.Contains(err.Error(), "They replied: the sequence has ended") {
			t.Fatalf("after reply: %v", err)
		}
	})

	t.Run("connected with consent ends the sequence and records the consent", func(t *testing.T) {
		f := newOutreachFixture(t)
		s := f.seqBatch(t, false, f.siteLead(t, "call-yes.localhost", nil, 7, "info@call-yes.localhost"))
		f.send(t, s, false, 0, day(0))
		f.send(t, s, false, 1, day(3))
		res, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeConnected, "spoke to the owner",
			&leadstore.ConsentInput{Address: " Owner@Call-Yes.localhost ", Wording: "May I send you the report by email?"},
			leadstore.ActorGremy, day(5), plainDays)
		if err != nil {
			t.Fatal(err)
		}
		if res.ConsentID == 0 || res.Cancelled[s.msg[3]] != campaign.CancelCallConnected || res.Cancelled[s.msg[4]] != campaign.CancelCallConnected {
			t.Errorf("connected: %+v", res)
		}
		var addr string
		if err := f.owner.QueryRow(ctx, `SELECT address FROM email_consent WHERE id = $1`, res.ConsentID).Scan(&addr); err != nil || addr != "owner@call-yes.localhost" {
			t.Errorf("consent address = %q, %v", addr, err)
		}
		if _, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeNoAnswer, "", &leadstore.ConsentInput{Address: "a@b.c", Wording: "x"},
			leadstore.ActorGremy, day(5), plainDays); !errors.Is(err, leadstore.ErrBadOutcome) {
			t.Errorf("consent on no_answer: %v, want ErrBadOutcome", err)
		}
	})

	t.Run("shadow applies and writes the do-not-call suppression", func(t *testing.T) {
		f := newOutreachFixture(t)
		lead := f.siteLead(t, "call-shadow.localhost", nil, 7, "info@call-shadow.localhost")
		s := f.seqBatch(t, true, lead)
		f.send(t, s, true, 0, day(0))
		f.send(t, s, true, 1, day(3))
		calls, err := f.q.CallsDue(ctx, 0, 0, day(5))
		if err != nil || len(calls) != 1 || calls[0].MessageID != s.msg[2] || !calls[0].Shadow ||
			calls[0].Phone != callNumber || calls[0].SourceURL != "https://evidence.localhost/" || calls[0].DerivedBy != "enrich@1" {
			t.Fatalf("calls due = %+v, %v", calls, err)
		}
		res, err := f.q.RecordCallOutcome(ctx, 0, s.msg[2], campaign.OutcomeDoNotCall, "", nil, leadstore.ActorGremy, day(5), plainDays)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Suppressed || !res.Shadow || res.Cancelled[s.msg[3]] != campaign.CancelDoNotCall || res.Cancelled[s.msg[4]] != campaign.CancelDoNotCall {
			t.Errorf("do not call: %+v", res)
		}
		want := leadstore.PhoneSuppressionValue(callNumber)
		if got := f.suppressions(t)["phone "+want]; got != leadstore.ReasonDoNotCall {
			t.Errorf("phone suppression = %q, want do_not_call (have %v)", got, f.suppressions(t))
		}
		set, err := f.q.SuppressionSet(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(set.Phones, []string{want}) || slices.Contains(set.Domains, want) || len(set.Domains) != 0 {
			t.Errorf("suppression set = %+v, want the hash in Phones only", set)
		}
		var state string
		if err := f.owner.QueryRow(ctx, `SELECT state FROM lead WHERE id = $1`, lead).Scan(&state); err != nil || state != leadstore.LeadNew {
			t.Errorf("shadow lead state = %q, %v; want new (RA1)", state, err)
		}
		// The number never gets a call row again.
		_, _, err = f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}},
			true, f.seqDrafts(t, lead, s.phoneFact), nil)
		if !errors.Is(err, leadstore.ErrNotDraftable) {
			t.Errorf("a new call to a do-not-call number: %v, want ErrNotDraftable", err)
		}
	})
}

// reply applies an incoming message of kind on s's step 0.
func (f outreachFixture) reply(t *testing.T, s seqLead, kind mailbox.MessageKind) {
	t.Helper()
	ctx := context.Background()
	refs, err := f.q.SentRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.ID == s.msg[0] {
			mb := f.mailbox(t)
			if _, err := f.q.ApplyIncomingFor(ctx, mb, leadstore.Incoming{Mailbox: "INBOX", UIDValidity: 1, UID: uint32(s.lead),
				Kind: kind, FromDomain: "reply.localhost", Matched: &r, At: day(4)}); err != nil {
				t.Fatalf("apply %s: %v", kind, err)
			}
			return
		}
	}
	t.Fatalf("no sent ref for step 0 of lead %d", s.lead)
}

// TestNoPendingWorkAfterStop: after a reply, opt-out, bounce or suppression, no row of the lead
// is approved with a due time (the call row included), and neither CallsDue nor TodayView shows
// the lead.
func TestNoPendingWorkAfterStop(t *testing.T) {
	ctx := context.Background()
	stops := map[string]func(f outreachFixture, s seqLead){
		"reply":   func(f outreachFixture, s seqLead) { f.reply(t, s, mailbox.KindReply) },
		"opt-out": func(f outreachFixture, s seqLead) { f.reply(t, s, mailbox.KindOptOut) },
		"bounce":  func(f outreachFixture, s seqLead) { f.reply(t, s, mailbox.KindBounce) },
		"suppress domain": func(f outreachFixture, s seqLead) {
			if _, err := f.q.Suppress(ctx, 0, leadstore.SuppressDomain, "stop.localhost", leadstore.ReasonManual, "test"); err != nil {
				t.Fatal(err)
			}
		},
		"suppress address": func(f outreachFixture, s seqLead) {
			if _, err := f.q.Suppress(ctx, 0, leadstore.SuppressAddress, "info@stop.localhost", leadstore.ReasonManual, "test"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, stop := range stops {
		t.Run(name, func(t *testing.T) {
			f := newOutreachFixture(t)
			s := f.seqBatch(t, false, f.siteLead(t, "stop.localhost", nil, 7, "info@stop.localhost"))
			f.send(t, s, false, 0, day(0))
			f.send(t, s, false, 1, day(3))
			// Every later step scheduled, the call included: the worst case to cancel.
			for _, st := range []int{3, 4} {
				if _, err := f.q.SetDueIfUnset(ctx, s.msg[st], day(10+st)); err != nil {
					t.Fatal(err)
				}
			}
			if calls, _ := f.q.CallsDue(ctx, 0, 0, day(30)); len(calls) != 1 {
				t.Fatalf("before the stop: %d calls due, want 1", len(calls))
			}
			stop(f, s)
			var pending int
			if err := f.owner.QueryRow(ctx,
				`SELECT count(*) FROM outreach_message WHERE lead_id = $1 AND state = 'approved' AND due_at IS NOT NULL`, s.lead).Scan(&pending); err != nil || pending != 0 {
				t.Errorf("pending due rows = %d, %v; want 0", pending, err)
			}
			if r := f.rowOf(t, s.msg[2]); r.state != leadstore.MsgCancelled {
				t.Errorf("call row = %+v, want cancelled", r)
			}
			calls, err := f.q.CallsDue(ctx, 0, 0, day(30))
			if err != nil || len(calls) != 0 {
				t.Errorf("calls due = %+v, %v; want none", calls, err)
			}
			today, err := f.q.TodayView(ctx, 0, 0, day(30))
			if err != nil {
				t.Fatal(err)
			}
			if len(today.CallsDueNow)+len(today.CallsOverdue)+len(today.Manual)+today.EmailsDueCount != 0 {
				t.Errorf("today = %+v, want no work for the lead", today)
			}
			if name == "reply" && (len(today.RepliesWaiting) != 1 || today.RepliesWaiting[0].LeadID != s.lead) {
				t.Errorf("replies waiting = %+v, want the lead", today.RepliesWaiting)
			}
		})
	}
}

func TestTodayViewBuckets(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	s := f.seqBatch(t, false, f.siteLead(t, "today.localhost", nil, 7, "info@today.localhost"))
	today, err := f.q.TodayView(ctx, 0, 0, day(0).Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if today.EmailsDueCount != 1 || len(today.EmailsDue) != 1 || today.EmailsDue[0].MessageID != s.msg[0] {
		t.Errorf("emails due = %d %+v", today.EmailsDueCount, today.EmailsDue)
	}
	f.send(t, s, false, 0, day(0))
	f.send(t, s, false, 1, day(3))
	// The call falls due on day 5: due now on day 5, overdue on day 7.
	if v, _ := f.q.TodayView(ctx, 0, 0, day(5).Add(time.Hour)); len(v.CallsDueNow) != 1 || len(v.CallsOverdue) != 0 {
		t.Errorf("day 5: %+v", v)
	}
	if v, _ := f.q.TodayView(ctx, 0, 0, day(7)); len(v.CallsDueNow) != 0 || len(v.CallsOverdue) != 1 {
		t.Errorf("day 7: %+v", v)
	}
	if v, _ := f.q.TodayView(ctx, 0, 99, day(7)); len(v.CallsOverdue) != 0 {
		t.Errorf("another campaign's view shows the call: %+v", v)
	}
	if c, err := f.q.CallByID(ctx, 0, s.msg[2]); err != nil || c.Card == "" || c.PhoneNormalized != callNumber {
		t.Errorf("call by id = %+v, %v", c, err)
	}
	if _, err := f.q.CallByID(ctx, 0, s.msg[0]); !errors.Is(err, leadstore.ErrMessageNotFound) {
		t.Errorf("call by id on an email: %v", err)
	}
}

// TestSequenceOfNoNumber: a lead drafted without a callable number has no call row and no
// "tried to reach you" row; the lead page says so.
func TestSequenceOfNoNumber(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "nonumber.localhost", nil, 7, "info@nonumber.localhost")
	var drafts []leadstore.DraftRow
	for _, d := range f.seqDrafts(t, lead, 0) {
		if d.Step != 2 && d.Step != 3 {
			drafts = append(drafts, d)
		}
	}
	if _, _, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}}, true, drafts, nil); err != nil {
		t.Fatal(err)
	}
	steps, err := f.q.SequenceOf(ctx, lead)
	if err != nil || len(steps) != 5 {
		t.Fatalf("sequence of: %+v %v", steps, err)
	}
	for _, i := range []int{2, 3} {
		if steps[i].Skipped != leadstore.NoNumber || steps[i].MessageID != 0 {
			t.Errorf("step %d = %+v, want %q", i, steps[i], leadstore.NoNumber)
		}
	}
	if steps[0].State != leadstore.MsgDraft || steps[4].Skipped != "" {
		t.Errorf("steps = %+v", steps)
	}
	if none, err := f.q.SequenceOf(ctx, 999999); err != nil || len(none) != 0 {
		t.Errorf("no batch: %+v %v", none, err)
	}
}

// TestSequenceColdStartTwin: a sequence batch's first send gives the same follow-up in a fresh
// database as in one holding a legacy (NULL sequence) batch mid-sequence, and the legacy batch
// is left on the old rule.
func TestSequenceColdStartTwin(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, withLegacy bool) (step1Due time.Time, legacyBefore, legacyAfter *time.Time) {
		f := newOutreachFixture(t)
		var legacyStep1 int64
		if withLegacy {
			old := f.siteLead(t, "legacy.localhost", nil, 7, "info@legacy.localhost")
			id, hash := f.batch(t, false, old)
			f.approve(t, id, hash)
			ms, err := f.q.BatchMessages(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if b, err := f.q.BatchByID(ctx, id); err != nil || b.Sequence != nil || b.CampaignID != leadstore.DefaultCampaignID {
				t.Fatalf("legacy batch = %+v, %v", b, err)
			}
			if ok, err := f.q.MarkSending(ctx, ms[0].ID); err != nil || !ok {
				t.Fatal(err)
			}
			if err := f.q.MarkSent(ctx, ms[0].ID, "250", day(-2)); err != nil {
				t.Fatal(err)
			}
			if d, legacy, err := f.q.SequenceDecision(ctx, ms[0].ID, day(-2), plainDays); err != nil || !legacy || len(d.Due)+len(d.Cancel) != 0 {
				t.Fatalf("legacy decision = %+v legacy=%v %v", d, legacy, err)
			}
			legacyStep1 = ms[1].ID
			if _, err := f.q.ScheduleFollowup(ctx, id, old, 1, day(1)); err != nil {
				t.Fatal(err)
			}
			legacyBefore = f.rowOf(t, legacyStep1).due
		}
		s := f.seqBatch(t, false, f.siteLead(t, "fresh.localhost", nil, 7, "info@fresh.localhost"))
		if b, err := f.q.BatchByID(ctx, s.batch); err != nil || b.Sequence == nil {
			t.Fatalf("sequence batch = %+v, %v", b, err)
		}
		f.send(t, s, false, 0, day(0))
		r := f.rowOf(t, s.msg[1])
		if r.due == nil {
			t.Fatal("step 1 not scheduled")
		}
		if withLegacy {
			legacyAfter = f.rowOf(t, legacyStep1).due
		}
		return *r.due, legacyBefore, legacyAfter
	}
	fresh, _, _ := run(t, false)
	mixed, legacyBefore, legacyDue := run(t, true)
	if !fresh.Equal(mixed) || !fresh.Equal(day(3)) {
		t.Errorf("step 1 due fresh %v, with legacy %v; want %v both", fresh, mixed, day(3))
	}
	if legacyDue == nil || legacyBefore == nil || !legacyDue.Equal(*legacyBefore) {
		t.Errorf("legacy step 1 due = %v, want %v (untouched)", legacyDue, legacyBefore)
	}
}

// TestBatchHashWithCallRows: the call rows are in the batch hash, the stored rows hash back to
// it (approval checks that), and an all-email draft hashes as before 00017.
func TestBatchHashWithCallRows(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "hash.localhost", nil, 7, "info@hash.localhost")
	fact := f.phoneFact(t, lead, callNumber)
	all := f.seqDrafts(t, lead, fact)
	var emails []leadstore.DraftRow
	for _, d := range all {
		if d.Channel == "email" {
			emails = append(emails, d)
		}
	}
	hAll, err := leadstore.ContentSHA256(all)
	if err != nil {
		t.Fatal(err)
	}
	hEmails, _ := leadstore.ContentSHA256(emails)
	if hex.EncodeToString(hAll) == hex.EncodeToString(hEmails) {
		t.Error("the call row is not in the hash")
	}
	id, sum, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}}, true, all, nil)
	if err != nil {
		t.Fatal(err)
	}
	re, err := f.q.BatchContentSHA256(ctx, id)
	if err != nil || hex.EncodeToString(re) != hex.EncodeToString(sum) {
		t.Errorf("stored rows hash %x, created %x (%v)", re, sum, err)
	}
	// "always" and "" are one condition, so a legacy draft's hash does not move.
	legacy := []leadstore.DraftRow{{LeadID: 1, Step: 0, Channel: "email", To: "a@b.localhost", Subject: "s", Body: "b", MailboxID: 1}}
	withAlways := []leadstore.DraftRow{legacy[0]}
	withAlways[0].Condition = campaign.WhenAlways
	h1, _ := leadstore.ContentSHA256(legacy)
	h2, _ := leadstore.ContentSHA256(withAlways)
	if hex.EncodeToString(h1) != hex.EncodeToString(h2) {
		t.Error(`condition "always" changes the hash of a legacy draft`)
	}
	// Bad sequence fields are refused before anything is written.
	bad := f.seqDrafts(t, lead, fact)
	bad[2].PhoneFactID = 0
	if _, _, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}}, true, bad, nil); !errors.Is(err, leadstore.ErrBadDraft) {
		t.Errorf("call row without a fact: %v", err)
	}
}
