//go:build integration

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
	"github.com/gremy/webbership-leadhound/internal/verify"
)

// The promise of Decision 4: the operator's word on an address stands in for a mail server when
// a channel is chosen, and the email check before any send stays the real gate, on every road
// that sends an email. An address he added or claimed goes only with a passing check; without
// one its message is left approved and unsent, and goes once it has one. Other addresses are
// not asked for one here.

// opAddress is an address no contacts run found, at the tests' alpha.example.
const opAddress = "owner@alpha.example"

func (w *sendWorld) orgOfLead(t *testing.T, lead int64) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT org_id FROM lead WHERE id = $1`, lead).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// opBatch drafts lead's three emails to to from the seeded mailbox as one batch, unapproved.
func (w *sendWorld) opBatch(t *testing.T, origin string, lead int64, to string) (int64, string) {
	t.Helper()
	first := ""
	var drafts []leadstore.DraftRow
	for step := range 3 {
		d := leadstore.DraftRow{LeadID: lead, Step: step, Channel: "email", To: to,
			Subject: fmt.Sprintf("Your Drupal site, note %d", step+1), Body: fmt.Sprintf("Hello, note %d.\n", step+1),
			MessageID: fmt.Sprintf("op%dl%ds%d@%s", len(origin), lead, step, testSenderDomain)}
		if step == 0 {
			d.MailboxID, d.FromName = w.mailbox.ID, w.fromName
			first = d.MessageID
		} else {
			d.InReplyTo = first
		}
		drafts = append(drafts, d)
	}
	id, sum, err := w.lq.CreateBatchFor(context.Background(), leadstore.BatchMeta{Origin: origin}, false, drafts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id, hex.EncodeToString(sum)[:16]
}

// approveNow approves batch id with its first email due now.
func (w *sendWorld) approveNow(t *testing.T, id int64, prefix string) {
	t.Helper()
	now := w.clock.now()
	if _, err := w.lq.ApproveBatch(context.Background(), id, prefix, nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return now
		}
		return time.Time{}
	}); err != nil {
		t.Fatal(err)
	}
}

func (w *sendWorld) firstState(t *testing.T, lead int64) string {
	t.Helper()
	return w.str(t, `SELECT state FROM outreach_message WHERE lead_id = $1 AND step = 0`, lead)
}

func (w *sendWorld) addCheck(t *testing.T, addr, status string) {
	t.Helper()
	w.clock.set(w.clock.now().Add(time.Minute))
	if _, err := w.lq.AddEmailCheck(context.Background(), leadstore.EmailCheck{Address: addr, Provider: verify.Provider,
		Status: status, CheckedAt: w.clock.now()}); err != nil {
		t.Fatal(err)
	}
}

// opRoads is every road that sends a live email: each opens the batch its own way and returns a
// send that runs it once more.
var opRoads = []struct {
	name string
	open func(t *testing.T, w *sendWorld, lead int64, to string) func() string
}{
	{"morning batch, hourly send", func(t *testing.T, w *sendWorld, lead int64, to string) func() string {
		id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, to)
		w.approveNow(t, id, prefix)
		return func() string { return w.mustRun(t, "leads", "send") }
	}},
	{"morning batch, leads send --now", func(t *testing.T, w *sendWorld, lead int64, to string) func() string {
		id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, to)
		w.approveNow(t, id, prefix)
		return func() string { return w.mustRun(t, "leads", "send", "--now") }
	}},
	{"Draft it, leads batch approve, hourly send", func(t *testing.T, w *sendWorld, lead int64, to string) func() string {
		id, prefix := w.opBatch(t, leadstore.BatchOriginDraftIt, lead, to)
		w.mustRun(t, "leads", "batch", "approve", fmt.Sprint(id), "--hash", prefix)
		if _, err := storetest.OwnerConn(t).Exec(context.Background(),
			`UPDATE outreach_message SET due_at = $2 WHERE batch_id = $1 AND step = 0`, id, w.clock.now()); err != nil {
			t.Fatal(err)
		}
		return func() string { return w.mustRun(t, "leads", "send") }
	}},
	{"Send now", func(t *testing.T, w *sendWorld, lead int64, to string) func() string {
		id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, to)
		if err := w.lq.MarkBatchManual(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		w.approveNow(t, id, prefix)
		return func() string {
			w.confirm(t, id, prefix)
			_, out := w.sendFromRequest(t)
			return out
		}
	}},
}

// TestOperatorEmailGate: on every road an address the operator added waits, unsent and not
// refused, while it has no passing check; a bad answer keeps it unsent and says what to do; a
// good one sends it on the next run.
func TestOperatorEmailGate(t *testing.T) {
	for _, road := range opRoads {
		t.Run(road.name, func(t *testing.T) {
			w := newSendWorld(t, true, 10)
			lead := w.lead(t, "alpha.example")
			addr := opAddress
			if _, err := w.lq.AddOperatorRoute(context.Background(), leadstore.OperatorRoute{OrgID: w.orgOfLead(t, lead), Kind: leadstore.RouteEmail, Value: addr}); err != nil {
				t.Fatal(err)
			}
			send := road.open(t, w, lead, addr)

			out := send()
			wantLines(t, "unchecked", out, "your own address", "leads verify")
			if w.smtp.count() != 0 || w.firstState(t, lead) != leadstore.MsgApproved {
				t.Fatalf("unchecked: sent %d, state %s\n%s", w.smtp.count(), w.firstState(t, lead), out)
			}

			w.addCheck(t, addr, verify.StatusInvalid)
			out = send()
			wantLines(t, "bad", out, "your own address", "failed its email check", "Retire it")
			if w.smtp.count() != 0 || w.firstState(t, lead) != leadstore.MsgApproved {
				t.Fatalf("bad: sent %d, state %s\n%s", w.smtp.count(), w.firstState(t, lead), out)
			}
			if strings.Contains(out, addr) {
				t.Errorf("the run printed the address:\n%s", out)
			}

			w.addCheck(t, addr, verify.StatusSafe)
			out = send()
			if got := w.smtp.recipients(); !slices.Equal(got, []string{addr}) {
				t.Fatalf("good: sent to %v\n%s", got, out)
			}
			if w.firstState(t, lead) != leadstore.MsgSent {
				t.Errorf("good: state %s", w.firstState(t, lead))
			}
		})
	}
}

// TestOperatorEmailClaimed: a claimed address found by a contacts run is his word too: unchecked
// it waits, with a passing check it sends. The same address unclaimed goes as it always has.
func TestOperatorEmailClaimed(t *testing.T) {
	ctx := context.Background()
	claim := func(t *testing.T, w *sendWorld, lead int64) {
		t.Helper()
		var route int64
		if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT route_id FROM lead WHERE id = $1`, lead).Scan(&route); err != nil {
			t.Fatal(err)
		}
		if err := w.lq.MarkContact(ctx, leadstore.Mark{OrgID: w.orgOfLead(t, lead), RouteID: route, Mark: leadstore.MarkClaim}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("claimed with a pass sends", func(t *testing.T) {
		w := newSendWorld(t, true, 10)
		lead := w.lead(t, "alpha.example")
		claim(t, w, lead)
		w.addCheck(t, "info@alpha.example", verify.StatusSafe)
		id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, "info@alpha.example")
		w.approveNow(t, id, prefix)
		w.mustRun(t, "leads", "send")
		if w.smtp.count() != 1 {
			t.Errorf("sent %d, want 1", w.smtp.count())
		}
	})
	t.Run("claimed and unchecked waits", func(t *testing.T) {
		w := newSendWorld(t, true, 10)
		lead := w.lead(t, "alpha.example")
		claim(t, w, lead)
		id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, "info@alpha.example")
		w.approveNow(t, id, prefix)
		w.mustRun(t, "leads", "send")
		if w.smtp.count() != 0 || w.firstState(t, lead) != leadstore.MsgApproved {
			t.Errorf("sent %d, state %s", w.smtp.count(), w.firstState(t, lead))
		}
	})
	t.Run("an ordinary address behaves as before", func(t *testing.T) {
		w := newSendWorld(t, true, 10)
		lead := w.lead(t, "alpha.example")
		id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, "info@alpha.example")
		w.approveNow(t, id, prefix)
		out := w.mustRun(t, "leads", "send")
		if w.smtp.count() != 1 || strings.Contains(out, "your own address") {
			t.Errorf("sent %d\n%s", w.smtp.count(), out)
		}
	})
}

// TestOperatorEmailCheckOff: with the Email check switched off his address still waits, and the
// reason names the switch, because nothing can check it until it is on.
func TestOperatorEmailCheckOff(t *testing.T) {
	ctx := context.Background()
	w := newSendWorld(t, true, 10)
	lead := w.lead(t, "alpha.example")
	addr := opAddress
	if _, err := w.lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: w.orgOfLead(t, lead), Kind: leadstore.RouteEmail, Value: addr}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.lq.SetJobSwitch(ctx, 0, leadstore.JobVerify, false, "test", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, addr)
	w.approveNow(t, id, prefix)
	out := w.mustRun(t, "leads", "send")
	wantLines(t, "off", out, "your own address", "Email check is off", "Controls")
	if w.smtp.count() != 0 || w.firstState(t, lead) != leadstore.MsgApproved {
		t.Errorf("sent %d, state %s\n%s", w.smtp.count(), w.firstState(t, lead), out)
	}
}

// TestOperatorEmailHeldTakesNoRoom: a held message takes none of the day's room, so the leads
// behind it still go.
func TestOperatorEmailHeldTakesNoRoom(t *testing.T) {
	ctx := context.Background()
	w := newSendWorld(t, true, 1)
	held := w.lead(t, "alpha.example")
	addr := opAddress
	if _, err := w.lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: w.orgOfLead(t, held), Kind: leadstore.RouteEmail, Value: addr}); err != nil {
		t.Fatal(err)
	}
	id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, held, addr)
	w.approveNow(t, id, prefix)
	w.clock.set(w.clock.now().Add(time.Minute))
	w.batch(t, false, map[int64]string{w.lead(t, "bravo.example"): "bravo.example"})
	out := w.mustRun(t, "leads", "send")
	if got := w.smtp.recipients(); !slices.Equal(got, []string{"info@bravo.example"}) {
		t.Errorf("sent to %v, want the lead behind the held one\n%s", got, out)
	}
}

// TestOperatorEmailShadow: a shadow run sends nothing to anyone and no check runs in shadow, so
// it writes the operator's address to the outbox as it does any other.
func TestOperatorEmailShadow(t *testing.T) {
	w := newSendWorld(t, false, 10)
	lead := w.lead(t, "alpha.example")
	addr := opAddress
	if _, err := w.lq.AddOperatorRoute(context.Background(), leadstore.OperatorRoute{OrgID: w.orgOfLead(t, lead), Kind: leadstore.RouteEmail, Value: addr}); err != nil {
		t.Fatal(err)
	}
	id, sum, err := w.lq.CreateBatchFor(context.Background(), leadstore.BatchMeta{}, true, []leadstore.DraftRow{
		{LeadID: lead, Step: 0, Channel: "email", To: addr, Subject: "S", Body: "B\n", MessageID: "shadow@" + testSenderDomain, MailboxID: w.mailbox.ID},
		{LeadID: lead, Step: 1, Channel: "email", To: addr, Subject: "S", Body: "B\n", MessageID: "shadow1@" + testSenderDomain, InReplyTo: "shadow@" + testSenderDomain},
		{LeadID: lead, Step: 2, Channel: "email", To: addr, Subject: "S", Body: "B\n", MessageID: "shadow2@" + testSenderDomain, InReplyTo: "shadow@" + testSenderDomain},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.approveNow(t, id, hex.EncodeToString(sum)[:16])
	w.mustRun(t, "leads", "send")
	if w.firstState(t, lead) != leadstore.MsgShadowSent {
		t.Errorf("state %s", w.firstState(t, lead))
	}
}

// TestOperatorEmailVerifyQueue: leads verify reaches an approved message's held address first,
// because the batch queue no longer sees a batched lead; then the send takes it.
func TestOperatorEmailVerifyQueue(t *testing.T) {
	ctx := context.Background()
	w := newSendWorld(t, true, 10)
	lead := w.lead(t, "alpha.example")
	addr := opAddress
	if _, err := w.lq.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: w.orgOfLead(t, lead), Kind: leadstore.RouteEmail, Value: addr}); err != nil {
		t.Fatal(err)
	}
	id, prefix := w.opBatch(t, leadstore.BatchOriginBatch, lead, addr)
	w.approveNow(t, id, prefix)
	f := &fakeReoon{bal: verify.Balance{Daily: 20}, status: map[string]string{addr: verify.StatusSafe}}
	useReoon(t, f)
	w.mustRun(t, "leads", "verify", "--max", "1")
	if got := f.checked(); !slices.Equal(got, []string{addr}) {
		t.Fatalf("checked %v", got)
	}
	w.mustRun(t, "leads", "send")
	if got := w.smtp.recipients(); !slices.Equal(got, []string{addr}) {
		t.Errorf("sent to %v", got)
	}
}

// TestSendNowPlanOperatorEmailCheckOff: with the Email check off the confirm page does not offer
// to send to his unchecked address: the send would hold it. With a passing check it goes.
func TestSendNowPlanOperatorEmailCheckOff(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	setVerify(t, false)
	var org int64
	var addr string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT l.org_id, r.value FROM lead l JOIN contact_route r ON r.id = l.route_id WHERE l.id = $1`, ids[0]).
		Scan(&org, &addr); err != nil {
		t.Fatal(err)
	}
	if _, err := b.lq().AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: addr}); err != nil {
		t.Fatal(err)
	}
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	if l := planLead(p, ids[0]); !strings.Contains(l.Stop, "Email check") || !strings.Contains(l.Stop, "Controls") {
		t.Errorf("his address: %+v", l)
	}
	if l := planLead(p, ids[1]); l.Stop != "" {
		t.Errorf("an ordinary address is stopped: %+v", l)
	}
	if _, err := b.lq().AddEmailCheck(ctx, leadstore.EmailCheck{Address: addr, Provider: verify.Provider, Status: verify.StatusSafe, CheckedAt: leadsNow()}); err != nil {
		t.Fatal(err)
	}
	if p, err = b.SendNowPlan(ctx, p.BatchID); err != nil {
		t.Fatal(err)
	}
	if l := planLead(p, ids[0]); l.Stop != "" {
		t.Errorf("checked good: %+v", l)
	}
}

// TestForgetOrgSharedBatchSends: `leads forget` on one lead's org leaves a shared batch sendable
// for the other lead, on the hourly send and on Send now, and the forget says so; a message
// changed after that is still refused at send.
func TestForgetOrgSharedBatchSends(t *testing.T) {
	ctx := context.Background()
	erase := func(t *testing.T, w *sendWorld, lead int64, domain string) string {
		t.Helper()
		t.Setenv("LEADHOUND_MIGRATE_DATABASE_URL", storetest.MigrateDSN())
		org := w.orgOfLead(t, lead)
		var person int64
		owner := storetest.OwnerConn(t)
		if err := owner.QueryRow(ctx, `INSERT INTO org_person (org_id, role, derived_by, actor) VALUES ($1, 'Owner', 'operator@1', 'operator') RETURNING id`, org).Scan(&person); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `UPDATE contact_route SET person_id = $2 WHERE org_id = $1 AND value = $3`, org, person, "info@"+domain); err != nil {
			t.Fatal(err)
		}
		return w.mustRun(t, "leads", "forget", "org", fmt.Sprint(org), "--reason", "asked to be forgotten")
	}
	t.Run("hourly send", func(t *testing.T) {
		w := newSendWorld(t, true, 10)
		gone, kept := w.lead(t, "alpha.example"), w.lead(t, "bravo.example")
		w.batch(t, false, map[int64]string{gone: "alpha.example", kept: "bravo.example"})
		wantLines(t, "forget", erase(t, w, gone, "alpha.example"), "1 batch re-hashed")
		out := w.mustRun(t, "leads", "send")
		if got := w.smtp.recipients(); !slices.Equal(got, []string{"info@bravo.example"}) {
			t.Errorf("sent to %v\n%s", got, out)
		}
	})
	t.Run("Send now", func(t *testing.T) {
		w, leads := manualWorld(t, 10, "alpha.example", "bravo.example")
		var gone int64
		for id, d := range leads {
			if d == "alpha.example" {
				gone = id
			}
		}
		id, old := w.manualBatch(t, w.mailbox, false, leads)
		erase(t, w, gone, "alpha.example")
		w.confirm(t, id, old)
		if code, out := w.sendFromRequest(t); code == 0 || w.smtp.count() != 0 {
			t.Errorf("the hash he confirmed before the erasure: exit %d, sent %d\n%s", code, w.smtp.count(), out)
		}
		b, err := w.lq.BatchByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		w.clock.set(w.clock.now().Add(time.Minute))
		w.confirm(t, id, hex.EncodeToString(b.ContentSHA256)[:16])
		_, out := w.sendFromRequest(t)
		if got := w.smtp.recipients(); !slices.Equal(got, []string{"info@bravo.example"}) {
			t.Errorf("sent to %v\n%s", got, out)
		}
	})
	t.Run("a later change is still refused", func(t *testing.T) {
		w := newSendWorld(t, true, 10)
		gone, kept := w.lead(t, "alpha.example"), w.lead(t, "bravo.example")
		w.batch(t, false, map[int64]string{gone: "alpha.example", kept: "bravo.example"})
		erase(t, w, gone, "alpha.example")
		if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE outreach_message SET body_text = 'changed' WHERE lead_id = $1 AND step = 0`, kept); err != nil {
			t.Fatal(err)
		}
		w.run(t, "leads", "send")
		if w.smtp.count() != 0 || w.firstState(t, kept) != leadstore.MsgRefused {
			t.Errorf("sent %d, state %s", w.smtp.count(), w.firstState(t, kept))
		}
	})
}
