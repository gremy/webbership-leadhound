//go:build integration

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// cp3FiveSteps is the mixed sequence the call tests draft: two emails, a call, "tried to
// reach you" when the call did not connect, and a final email.
var cp3FiveSteps = []campaign.Step{
	{Step: 0, Type: campaign.StepEmail, When: campaign.WhenAlways},
	{Step: 1, Type: campaign.StepEmail, When: campaign.WhenAlways, AfterBusinessDays: 3},
	{Step: 2, Type: campaign.StepCall, When: campaign.WhenHasCallablePhone, AfterBusinessDays: 2},
	{Step: 3, Type: campaign.StepEmail, When: campaign.WhenCallNotConnected, AfterBusinessDays: 1},
	{Step: 4, Type: campaign.StepEmail, When: campaign.WhenAlways, AfterBusinessDays: 5},
}

// cp3Number is the fictional (Ofcom drama range) number the call rows dial.
const cp3Number = "+441632960002"

// phoneFact adds number as a phone fact of lead's org (read from the latest fetch) and
// returns its id.
func (w *sendWorld) phoneFact(t *testing.T, lead int64, number string) int64 {
	t.Helper()
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	var orgID, fetchID int64
	if err := owner.QueryRow(ctx, `SELECT org_id FROM lead WHERE id = $1`, lead).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT max(id) FROM http_fetch`).Scan(&fetchID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.lq.InsertFact(ctx, leadstore.FactRow{OrgID: orgID, Kind: leadstore.FactPhone, Value: number,
		Normalized: number, FetchID: fetchID, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := owner.QueryRow(ctx, `SELECT id FROM org_fact WHERE org_id = $1 AND normalized = $2`, orgID, number).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// seqBatch drafts steps for lead (to info@domain, from the seeded mailbox; a call step dials
// fact) in one batch storing seq as its default sequence, approves it with the steps in
// dueNow due at the clock's now (the rest unscheduled), and returns the message ids by step.
func (w *sendWorld) seqBatch(t *testing.T, shadow bool, lead int64, domain string, fact int64, seq []campaign.Step, dueNow ...int) map[int]int64 {
	t.Helper()
	ctx := context.Background()
	var drafts []leadstore.DraftRow
	first := ""
	for _, st := range seq {
		d := leadstore.DraftRow{LeadID: lead, Step: st.Step, Channel: "email", To: "info@" + domain, Condition: st.When,
			Subject: fmt.Sprintf("Your Drupal site, note %d", st.Step+1), Body: fmt.Sprintf("Hello %s, note %d.\n", domain, st.Step+1),
			MessageID: fmt.Sprintf("seq%dl%ds%d@%s", len(drafts), lead, st.Step, testSenderDomain)}
		switch {
		case st.Type == campaign.StepCall:
			d.Channel, d.To, d.MessageID, d.PhoneFactID = leadstore.ChannelPhone, "", "", fact
			d.Subject, d.Body = "Call "+domain, "Who I am\nWhy I call\nThe free report"
		case first == "":
			d.MailboxID, d.FromName = w.mailbox.ID, w.fromName
			first = d.MessageID
		default:
			d.InReplyTo = first
		}
		drafts = append(drafts, d)
	}
	tx, err := storetest.AppConn(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := leadstore.NewQueries(tx)
	id, sum, err := q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{campaign.SequenceDefault: seq}}, shadow, drafts, nil)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	now := w.clock.now()
	if _, err := q.ApproveBatch(ctx, id, hex.EncodeToString(sum), nil, func(m leadstore.Message) time.Time {
		if slices.Contains(dueNow, m.Step) {
			return now
		}
		return time.Time{}
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	ms, err := q.BatchMessages(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	out := map[int]int64{}
	for _, m := range ms {
		out[m.Step] = m.ID
	}
	return out
}

type cp3Row struct {
	state, channel, reason string
	due, sent              *time.Time
}

func (w *sendWorld) row(t *testing.T, id int64) cp3Row {
	t.Helper()
	var r cp3Row
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT state, channel, cancel_reason, due_at, sent_at FROM outreach_message WHERE id = $1`, id).
		Scan(&r.state, &r.channel, &r.reason, &r.due, &r.sent); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSendNeverTouchesCallRows (CP3 sweep): `leads send`, shadow and live (mock SMTP), never
// returns, claims, sends or writes out a call row, even one approved and due right now;
// the email beside it goes as usual. The store refuses to claim a call row too.
func TestSendNeverTouchesCallRows(t *testing.T) {
	ctx := context.Background()
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "shadow", true: "live"}[live], func(t *testing.T) {
			w := newSendWorld(t, live, 10)
			lead := w.lead(t, "callsweep.example")
			fact := w.phoneFact(t, lead, cp3Number)
			ids := w.seqBatch(t, !live, lead, "callsweep.example", fact, cp3FiveSteps, 0, 2)
			call := ids[2]
			if r := w.row(t, call); r.channel != leadstore.ChannelPhone || r.state != leadstore.MsgApproved || r.due == nil {
				t.Fatalf("call row before send = %+v, want an approved phone row due now", r)
			}
			rows, err := w.lq.SendRows(ctx, w.clock.now().Add(time.Hour), !live)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.Channel != "email" {
					t.Errorf("SendRows returned a %s row (message %d)", r.Channel, r.ID)
				}
			}
			w.mustRun(t, "leads", "send")
			w.clock.set(w.clock.now().Add(30 * time.Minute))
			w.mustRun(t, "leads", "send")

			if r := w.row(t, call); r.state != leadstore.MsgApproved || r.sent != nil {
				t.Errorf("call row after send = %+v, want approved and never sent", r)
			}
			want := leadstore.MsgShadowSent
			if live {
				want = leadstore.MsgSent
			}
			if r := w.row(t, ids[0]); r.state != want {
				t.Errorf("step 0 = %+v, want %s", r, want)
			}
			if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE channel = 'phone'
				AND state IN ('sending', 'sent', 'maybe_sent', 'shadow_sent', 'refused')`); n != 0 {
				t.Errorf("%d call rows claimed or sent", n)
			}
			if live {
				if got := w.smtp.recipients(); len(got) != 1 || got[0] != "info@callsweep.example" {
					t.Errorf("SMTP got %v, want the one email", got)
				}
			} else {
				entries, err := os.ReadDir(filepath.Join(w.dir, outboxDir))
				if err != nil || len(entries) != 1 {
					t.Errorf("outbox: %d entries, %v; want the one email", len(entries), err)
				}
				for _, e := range entries {
					b, _ := os.ReadFile(filepath.Join(w.dir, outboxDir, e.Name())) // #nosec G304 -- the test's temp dir
					if strings.Contains(string(b), cp3Number) || strings.Contains(string(b), "Who I am") {
						t.Errorf("the outbox holds the call card or its number: %s", e.Name())
					}
				}
				if w.smtp.count() != 0 {
					t.Error("shadow reached SMTP")
				}
			}
			// The store's own claims refuse a call row.
			if ok, err := w.lq.MarkSending(ctx, call); err != nil || ok {
				t.Errorf("MarkSending on a call row = %v, %v; want false", ok, err)
			}
			if !live {
				if err := w.lq.MarkShadowSent(ctx, call, w.clock.now()); err == nil {
					t.Error("MarkShadowSent took a call row")
				}
			}
			if r := w.row(t, call); r.state != leadstore.MsgApproved {
				t.Errorf("call row after the claims = %+v", r)
			}
		})
	}
}

// TestScheduleNextLegacyContractDrift (CP3 contract drift): a legacy batch (no stored
// sequence) and a batch storing the legacy 3-step sequence, both walked by `leads send`,
// get every follow-up due exactly where the old rule puts it (followup_business_days after
// the step before, on the business-hours calendar), and nothing cancelled.
func TestScheduleNextLegacyContractDrift(t *testing.T) {
	ctx := context.Background()
	w := newSendWorld(t, true, 10)
	cfg, err := leadconfig.Load(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	days, bh := cfg.Schedule.FollowupBusinessDays, cfg.Schedule.BusinessHours
	loc := cfg.Policy("GB").Location()
	oldRule := func(prev time.Time, step int) time.Time { return outreach.FollowupDue(prev, days[step], loc, bh) }

	legacy := w.lead(t, "legacy-drift.example")
	w.batch(t, false, map[int64]string{legacy: "legacy-drift.example"})
	stored := w.lead(t, "stored-drift.example")
	storedIDs := w.seqBatch(t, false, stored, "stored-drift.example", 0, campaign.LegacySequence(days), 0)
	if got := w.msgID(t, "stored-drift.example", 0); got != storedIDs[0] {
		t.Fatalf("stored batch step 0 = message %d, want %d", got, storedIDs[0])
	}
	if b, err := w.lq.BatchByID(ctx, w.batchOf(t, storedIDs[0])); err != nil || b.Sequence == nil {
		t.Fatalf("stored batch = %+v, %v; want a stored sequence", b, err)
	}
	if b, err := w.lq.BatchByID(ctx, w.batchOf(t, w.msgID(t, "legacy-drift.example", 0))); err != nil || b.Sequence != nil {
		t.Fatalf("legacy batch = %+v, %v; want no stored sequence", b, err)
	}
	ids := map[string]map[int]int64{
		"legacy-drift.example": {0: w.msgID(t, "legacy-drift.example", 0), 1: w.msgID(t, "legacy-drift.example", 1), 2: w.msgID(t, "legacy-drift.example", 2)},
		"stored-drift.example": storedIDs,
	}
	for step := range 3 {
		w.mustRun(t, "leads", "send")
		var latest time.Time
		for domain, m := range ids {
			r := w.row(t, m[step])
			if r.state != leadstore.MsgSent || r.sent == nil {
				t.Fatalf("%s step %d = %+v, want sent", domain, step, r)
			}
			if step == 2 {
				continue
			}
			next := w.row(t, m[step+1])
			want := oldRule(*r.sent, step)
			if next.due == nil || !next.due.Equal(want) {
				t.Errorf("%s step %d due %v; the old rule says %v", domain, step+1, next.due, want)
			}
			if next.due != nil && next.due.After(latest) {
				latest = *next.due
			}
		}
		if step < 2 {
			w.clock.set(latest.Add(time.Minute))
		}
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE state = 'cancelled'`); n != 0 {
		t.Errorf("%d messages cancelled; the legacy sequence cancels none", n)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE state = 'approved'`); n != 0 {
		t.Errorf("%d messages still pending after three sends", n)
	}
}

func (w *sendWorld) batchOf(t *testing.T, msg int64) int64 {
	t.Helper()
	return int64(w.count(t, `SELECT batch_id::int FROM outreach_message WHERE id = $1`, msg))
}

func (w *sendWorld) msgID(t *testing.T, domain string, step int) int64 {
	t.Helper()
	return int64(w.count(t, `SELECT m.id::int FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = $1 AND m.step = $2 ORDER BY m.id DESC LIMIT 1`, domain, step))
}

// TestLeadsCallsCommand (CP3 grid, the command's words): `leads calls list` says calls are
// off and lists the call once it falls due; `leads calls outcome` refuses a call not yet due,
// an email, a second outcome once the sequence moved on and a call whose lead was
// suppressed, each naming why with nothing written; an outcome on a due call in a shadow
// batch applies and schedules the next email; do_not_call writes the phone suppression.
func TestLeadsCallsCommand(t *testing.T) {
	ctx := context.Background()
	w := newSendWorld(t, false, 10)
	lead := w.lead(t, "callcmd.example")
	fact := w.phoneFact(t, lead, cp3Number)
	ids := w.seqBatch(t, true, lead, "callcmd.example", fact, cp3FiveSteps, 0)
	call := ids[2]
	dncLead := w.lead(t, "calldnc.example")
	dncIDs := w.seqBatch(t, true, dncLead, "calldnc.example", w.phoneFact(t, dncLead, "+441632960004"), cp3FiveSteps, 0)
	outcomes := func() int {
		return w.count(t, `SELECT count(*) FROM call_outcome WHERE message_id = $1`, call)
	}
	refused := func(what string, wantWords string, args ...string) {
		t.Helper()
		code, out := w.run(t, append([]string{"leads", "calls", "outcome"}, args...)...)
		if code == 0 || !strings.Contains(out, wantWords) {
			t.Errorf("%s: exit %d, want refused with %q:\n%s", what, code, wantWords, out)
		}
	}

	out := w.mustRun(t, "leads", "calls", "list")
	wantLines(t, "list before", out, callsOffWords, "No call is scheduled.")
	refused("waiting on the email before it", "Not due yet: this call waits on the email before it. Nothing was saved.", fmt.Sprint(call), "no_answer")
	refused("an email row", "is not a call. Nothing was saved.", fmt.Sprint(ids[0]), "connected")

	// Steps 0 and 1 go (shadow); the call falls due.
	w.mustRun(t, "leads", "send")
	latest := func(steps ...int64) time.Time {
		t.Helper()
		var at time.Time
		for _, id := range steps {
			r := w.row(t, id)
			if r.due == nil || r.state != leadstore.MsgApproved {
				t.Fatalf("message %d = %+v, want approved and scheduled", id, r)
			}
			if r.due.After(at) {
				at = *r.due
			}
		}
		return at
	}
	w.clock.set(latest(ids[1], dncIDs[1]).Add(time.Minute))
	w.mustRun(t, "leads", "send")
	callDue := latest(call, dncIDs[2])
	out = w.mustRun(t, "leads", "calls", "list")
	wantLines(t, "list scheduled", out, callsOffWords, fmt.Sprintf("message %d · callcmd.example · step 3", call), "shadow batch")
	if strings.Contains(out, cp3Number) {
		t.Error("the call list prints the number")
	}
	refused("before its due time", "Not due yet. Nothing was saved.", fmt.Sprint(call), "no_answer")
	if n := outcomes(); n != 0 {
		t.Fatalf("a refused outcome wrote %d rows", n)
	}

	w.clock.set(callDue.Add(time.Minute))
	out = w.mustRun(t, "leads", "calls", "list", "--due")
	wantLines(t, "list due", out, callsOffWords, fmt.Sprintf("message %d", call))
	out = w.mustRun(t, "leads", "calls", "outcome", fmt.Sprint(call), "no_answer", "--note", "rang twice")
	wantLines(t, "outcome", out, "Saved: no answer. (shadow batch)", "Next: the email at step 4 is due")
	if r := w.row(t, call); r.state != leadstore.MsgDone {
		t.Errorf("call row = %+v, want done", r)
	}
	if r := w.row(t, ids[3]); r.due == nil {
		t.Errorf("step 3 = %+v, want scheduled", r)
	}
	refused("a second outcome", "Already recorded as no answer, and the sequence has moved on. Nothing was saved.", fmt.Sprint(call), "voicemail")
	if n := outcomes(); n != 1 {
		t.Errorf("outcomes = %d, want 1", n)
	}

	// A third lead, suppressed before its call; then do_not_call on the second.
	stopLead := w.lead(t, "callstop.example")
	stopIDs := w.seqBatch(t, true, stopLead, "callstop.example", w.phoneFact(t, stopLead, "+441632960003"), cp3FiveSteps, 0, 2)
	if _, err := w.lq.Suppress(ctx, 0, leadstore.SuppressDomain, "callstop.example", leadstore.ReasonManual, "test"); err != nil {
		t.Fatal(err)
	}
	refused("after the lead was suppressed", "The sequence has ended", fmt.Sprint(stopIDs[2]), "connected")

	out = w.mustRun(t, "leads", "calls", "outcome", fmt.Sprint(dncIDs[2]), "do_not_call")
	wantLines(t, "do not call", out, "Saved: do not call. (shadow batch)", "The sequence has ended", "Their number is on the do-not-call list now.")
	for _, st := range []int{3, 4} {
		if r := w.row(t, dncIDs[st]); r.state != leadstore.MsgCancelled || r.reason != campaign.CancelDoNotCall {
			t.Errorf("do not call: step %d = %+v, want cancelled %s", st, r, campaign.CancelDoNotCall)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM suppression_active WHERE kind = 'phone' AND reason = 'do_not_call' AND value = $1`,
		leadstore.PhoneSuppressionValue("+441632960004")); n != 1 {
		t.Errorf("phone suppressions = %d, want 1 (shadow writes it)", n)
	}
	if n := w.count(t, `SELECT count(*) FROM suppression WHERE value LIKE '%1632960004%'`); n != 0 {
		t.Error("a suppression holds the number itself")
	}
}

// TestLeadsTransferPhoneSuppressions: a phone suppression (its sha256: value, never the
// number) crosses export and import for each phone reason, lands in the target's Phones set
// and never in Domains, and a second import finds them all already there.
func TestLeadsTransferPhoneSuppressions(t *testing.T) {
	leadstoretest.Reset(t)
	t.Setenv("LEADHOUND_DATABASE_URL", storetest.AppDSN())
	ctx := context.Background()
	conn := storetest.AppConn(t)
	numbers := map[string]string{
		leadstore.ReasonDoNotCall: "+441632960010",
		leadstore.ReasonTPS:       "+441632960011",
		leadstore.ReasonCTPS:      "+441632960012",
	}
	var want []string
	for reason, n := range numbers {
		v := leadstore.PhoneSuppressionValue(n)
		want = append(want, v)
		if _, err := leadstore.NewQueries(conn).Suppress(ctx, 0, leadstore.SuppressPhone, v, reason, "cli"); err != nil {
			t.Fatalf("suppress %s: %v", reason, err)
		}
	}
	slices.Sort(want)

	dir := filepath.Join(t.TempDir(), "transfer")
	code, out, errOut := leadsRun(t, "leads", "transfer", "export", dir)
	if code != 0 {
		t.Fatalf("export exited %d: %s", code, errOut)
	}
	wantLines(t, "export", out, "suppressions 3")
	body, err := os.ReadFile(filepath.Join(dir, tfSuppressions)) // #nosec G304 -- the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	for reason, n := range numbers {
		line := fmt.Sprintf(`"kind":"phone","value":%q,"reason":%q`, leadstore.PhoneSuppressionValue(n), reason)
		if !strings.Contains(string(body), line) {
			t.Errorf("suppressions file lacks %s:\n%s", line, body)
		}
		if strings.Contains(string(body), strings.TrimPrefix(n, "+")) {
			t.Errorf("suppressions file holds the number %s", n)
		}
	}

	leadstoretest.Reset(t)
	code, out, errOut = leadsRun(t, "leads", "transfer", "import", dir)
	if code != 0 {
		t.Fatalf("import exited %d: %s", code, errOut)
	}
	wantLines(t, "import", out, "suppressions: 3 added · 0 already there")
	set, err := leadstore.NewQueries(conn).SuppressionSet(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(set.Phones)
	slices.Sort(got)
	if !slices.Equal(got, want) || len(set.Domains) != 0 || len(set.Addresses) != 0 {
		t.Errorf("target set = %+v, want the three hashes in Phones only", set)
	}
	var reasons []string
	rows, err := conn.Query(ctx, `SELECT reason FROM suppression_active WHERE kind = 'phone' ORDER BY reason`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, r)
	}
	rows.Close()
	if !slices.Equal(reasons, []string{leadstore.ReasonCTPS, leadstore.ReasonDoNotCall, leadstore.ReasonTPS}) {
		t.Errorf("target reasons = %v", reasons)
	}
	code, out, errOut = leadsRun(t, "leads", "transfer", "import", dir)
	if code != 0 {
		t.Fatalf("second import exited %d: %s", code, errOut)
	}
	wantLines(t, "second import", out, "suppressions: 0 added · 3 already there")
}
