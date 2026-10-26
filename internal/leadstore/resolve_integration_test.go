//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestDiscardBatch: discard cancels its own batch's pending messages only, puts its leads
// back to new, marks the batch, and is refused on an approved batch; an approval of a
// discarded batch is refused; the batch guard's query skips discarded and approved batches.
func TestDiscardBatch(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "discard-a.localhost", nil, 7, "info@discard-a.localhost")
	b := f.siteLead(t, "discard-b.localhost", nil, 7, "info@discard-b.localhost")
	c := f.siteLead(t, "discard-c.localhost", nil, 7, "info@discard-c.localhost")
	first, hash1 := f.batch(t, false, a, b)
	other, _ := f.batch(t, false, c)

	if id, ok, err := f.q.UnapprovedBatch(ctx, false); err != nil || !ok || id != first {
		t.Fatalf("waiting live batch = %d %v %v, want %d", id, ok, err, first)
	}
	if _, ok, err := f.q.UnapprovedBatch(ctx, true); err != nil || ok {
		t.Errorf("a shadow batch waits: %v %v", ok, err)
	}

	d, err := f.q.DiscardBatch(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if d.Already || d.Shadow || d.Cancelled != 4 || d.LeadsBack != 2 {
		t.Errorf("discard = %+v, want 4 cancelled and 2 leads back", d)
	}
	for _, id := range []int64{a, b} {
		if s := f.leadState(t, id); s != leadstore.LeadNew {
			t.Errorf("lead %d is %s after discard, want new", id, s)
		}
		eqStates(t, "discarded batch", f.msgStates(t, first, id), "cancelled", "cancelled")
	}
	var reasons int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM outreach_message WHERE batch_id = $1 AND cancel_reason = 'batch discarded'`, first).Scan(&reasons); err != nil || reasons != 4 {
		t.Errorf("%d messages carry the discard reason (%v)", reasons, err)
	}
	// The other batch is untouched.
	eqStates(t, "other batch", f.msgStates(t, other, c), "draft", "draft")
	if s := f.leadState(t, c); s != leadstore.LeadBatched {
		t.Errorf("the other batch's lead is %s", s)
	}
	got, err := f.q.BatchByID(ctx, first)
	if err != nil || got.DiscardedAt == nil {
		t.Errorf("batch %d not marked discarded: %+v %v", first, got, err)
	}
	if id, ok, _ := f.q.UnapprovedBatch(ctx, false); !ok || id != other {
		t.Errorf("waiting live batch after discard = %d %v, want %d", id, ok, other)
	}

	// Again: a no-op. Approving it: refused, nothing changes.
	if d, err := f.q.DiscardBatch(ctx, first); err != nil || !d.Already {
		t.Errorf("second discard = %+v %v, want Already", d, err)
	}
	if _, err := f.q.ApproveBatch(ctx, first, hash1, nil, dueAt); !errors.Is(err, leadstore.ErrBatchDiscarded) {
		t.Errorf("approve a discarded batch: %v", err)
	}
	eqStates(t, "after refused approve", f.msgStates(t, first, a), "cancelled", "cancelled")

	// An approved batch is never discarded.
	approved, hashA := f.batch(t, false, f.siteLead(t, "discard-e.localhost", nil, 7, "info@discard-e.localhost"))
	f.approve(t, approved, hashA)
	if _, err := f.q.DiscardBatch(ctx, approved); !errors.Is(err, leadstore.ErrBatchApproved) {
		t.Errorf("discard an approved batch: %v", err)
	}
	var approvedN int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM outreach_message WHERE batch_id = $1 AND state = 'approved'`, approved).Scan(&approvedN); err != nil || approvedN != 2 {
		t.Errorf("a refused discard changed the approved batch: %d approved (%v)", approvedN, err)
	}
	if _, err := f.q.DiscardBatch(ctx, 9999); !errors.Is(err, leadstore.ErrBatchNotFound) {
		t.Errorf("discard a missing batch: %v", err)
	}

	// A shadow batch never moved its leads: they stay new.
	s := f.siteLead(t, "discard-s.localhost", nil, 7, "info@discard-s.localhost")
	shadow, _ := f.batch(t, true, s)
	if id, ok, _ := f.q.UnapprovedBatch(ctx, true); !ok || id != shadow {
		t.Errorf("waiting shadow batch = %d %v, want %d", id, ok, shadow)
	}
	if d, err := f.q.DiscardBatch(ctx, shadow); err != nil || !d.Shadow || d.LeadsBack != 0 || d.Cancelled != 2 {
		t.Errorf("shadow discard = %+v %v", d, err)
	}
	if st := f.leadState(t, s); st != leadstore.LeadNew {
		t.Errorf("shadow lead is %s", st)
	}
	if _, ok, _ := f.q.UnapprovedBatch(ctx, true); ok {
		t.Errorf("a discarded shadow batch still waits")
	}
}

// TestWaitingEmails counts the live emails that may still go, follow-ups included: approved,
// and maybe sent or left sending apart. Shadow batches and selftests never count.
func TestWaitingEmails(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	waiting := func() (int, int) {
		t.Helper()
		a, c, err := f.q.WaitingEmails(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return a, c
	}
	if a, c := waiting(); a != 0 || c != 0 {
		t.Fatalf("cold start: %d %d", a, c)
	}
	live, hash := f.batch(t, false, f.siteLead(t, "unsent-a.localhost", nil, 7, "info@unsent-a.localhost"))
	if a, _ := waiting(); a != 0 {
		t.Errorf("drafts count: %d", a)
	}
	f.approve(t, live, hash)
	shadow, shash := f.batch(t, true, f.siteLead(t, "unsent-b.localhost", nil, 7, "info@unsent-b.localhost"))
	f.approve(t, shadow, shash)
	if a, c := waiting(); a != 2 || c != 0 {
		t.Errorf("waiting = %d approved, %d to check; want 2 (both steps of the live lead), 0", a, c)
	}
	rows, err := f.q.SendRows(ctx, dueBase, false)
	if err != nil || len(rows) != 1 {
		t.Fatalf("send rows: %v %v", rows, err)
	}
	if ok, _ := f.q.MarkSending(ctx, rows[0].ID); !ok {
		t.Fatal("mark sending")
	}
	if a, c := waiting(); a != 1 || c != 1 {
		t.Errorf("with one sending: %d approved, %d to check; want 1, 1", a, c)
	}
	if err := f.q.MarkMaybeSent(ctx, rows[0].ID, "connection lost"); err != nil {
		t.Fatal(err)
	}
	if a, c := waiting(); a != 1 || c != 1 {
		t.Errorf("with one maybe sent: %d approved, %d to check; want 1, 1", a, c)
	}
	if _, err := f.q.ResolveSent(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if a, c := waiting(); a != 1 || c != 0 {
		t.Errorf("after it was found sent: %d approved, %d to check; want 1, 0", a, c)
	}
	// A selftest, approved and then maybe sent, never counts.
	f.selftestMaybeSent(t)
	if a, c := waiting(); a != 1 || c != 0 {
		t.Errorf("a selftest counts: %d approved, %d to check", a, c)
	}
}

// selftestMaybeSent writes one selftest and leaves its message maybe_sent.
func (f outreachFixture) selftestMaybeSent(t *testing.T) int64 {
	t.Helper()
	ctx := context.Background()
	st, err := f.q.CreateSelftest(ctx, leadstore.SelftestDraft{To: "operator@sender.localhost", Subject: "selftest", Body: "selftest",
		MessageID: "<selftest.1@shadow.invalid>", Due: dueBase, MailboxID: f.mailbox(t)})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.q.MarkSending(ctx, st.MessageID); !ok {
		t.Fatal("mark sending")
	}
	if err := f.q.StampAttempt(ctx, st.MessageID, dueBase); err != nil {
		t.Fatal(err)
	}
	if err := f.q.MarkMaybeSent(ctx, st.MessageID, "connection lost"); err != nil {
		t.Fatal(err)
	}
	return st.MessageID
}

// TestResolveRefusesSelftest: a selftest's message is never checked by hand, either way.
func TestResolveRefusesSelftest(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	id := f.selftestMaybeSent(t)
	if _, err := f.q.ResolveSent(ctx, id); !errors.Is(err, leadstore.ErrSelftestMessage) {
		t.Errorf("resolve sent: %v", err)
	}
	if _, err := f.q.ResolveNotSent(ctx, id); !errors.Is(err, leadstore.ErrSelftestMessage) {
		t.Errorf("resolve not sent: %v", err)
	}
	var state string
	if err := f.owner.QueryRow(ctx, `SELECT state FROM outreach_message WHERE id = $1`, id).Scan(&state); err != nil || state != leadstore.MsgMaybeSent {
		t.Errorf("a refused resolve changed the selftest: %s %v", state, err)
	}
}

// maybeSent makes a live lead's first email maybe_sent at at (or leaves it sending), and
// returns the message id, its batch and its lead.
func (f outreachFixture) maybeSent(t *testing.T, domain string, at time.Time, leaveSending bool) (msg, batch, lead int64) {
	t.Helper()
	ctx := context.Background()
	lead = f.siteLead(t, domain, nil, 7, "info@"+domain)
	batch, hash := f.batch(t, false, lead)
	if _, err := f.q.ApproveBatch(ctx, batch, hash[:12], nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return dueBase
		}
		return time.Time{} // a follow-up gets its time when the first email goes
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT id FROM outreach_message WHERE batch_id = $1 AND step = 0`, batch).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.q.MarkSending(ctx, msg); !ok {
		t.Fatal("mark sending")
	}
	if err := f.q.StampAttempt(ctx, msg, at); err != nil {
		t.Fatal(err)
	}
	if !leaveSending {
		if err := f.q.MarkMaybeSent(ctx, msg, "connection lost after DATA"); err != nil {
			t.Fatal(err)
		}
	}
	return msg, batch, lead
}

// TestResolveSent: a maybe_sent or sending email checked by hand as sent becomes sent at its
// attempt time and its lead contacted; once only.
func TestResolveSent(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	at := dueBase.Add(5 * time.Minute)
	for _, leaveSending := range []bool{false, true} {
		domain := "resolve-maybe.localhost"
		if leaveSending {
			domain = "resolve-sending.localhost"
		}
		msg, batch, lead := f.maybeSent(t, domain, at, leaveSending)
		r, err := f.q.ResolveSent(ctx, msg)
		if err != nil {
			t.Fatalf("%s: %v", domain, err)
		}
		if r.BatchID != batch || r.LeadID != lead || r.Step != 0 || !r.SentAt.Equal(at) || !r.LeadMoved {
			t.Errorf("%s: resolved = %+v", domain, r)
		}
		eqStates(t, domain, f.msgStates(t, batch, lead), "sent", "approved")
		if s := f.leadState(t, lead); s != leadstore.LeadContacted {
			t.Errorf("%s: lead is %s, want contacted", domain, s)
		}
		if _, err := f.q.ResolveSent(ctx, msg); !errors.Is(err, leadstore.ErrMessageState) {
			t.Errorf("%s: second resolve: %v", domain, err)
		}
	}
	if first, err := f.q.FirstLiveSend(ctx); err != nil || first == nil || !first.Equal(at) {
		t.Errorf("first live send = %v %v, want the attempt time", first, err)
	}
	if _, err := f.q.ResolveSent(ctx, 9999); !errors.Is(err, leadstore.ErrMessageNotFound) {
		t.Errorf("missing message: %v", err)
	}
}

// TestResolveNotSent: a maybe_sent or sending email checked by hand as not sent goes back to
// approved with no attempt time, so the cap counts it on the day it really goes.
func TestResolveNotSent(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	at := dueBase.Add(5 * time.Minute)
	for _, leaveSending := range []bool{false, true} {
		domain := "notsent-maybe.localhost"
		if leaveSending {
			domain = "notsent-sending.localhost"
		}
		msg, batch, lead := f.maybeSent(t, domain, at, leaveSending)
		r, err := f.q.ResolveNotSent(ctx, msg)
		if err != nil {
			t.Fatalf("%s: %v", domain, err)
		}
		if r.BatchID != batch || r.LeadID != lead || r.LeadMoved {
			t.Errorf("%s: resolved = %+v", domain, r)
		}
		eqStates(t, domain, f.msgStates(t, batch, lead), "approved", "approved")
		var sentAt *time.Time
		if err := f.owner.QueryRow(ctx, `SELECT sent_at FROM outreach_message WHERE id = $1`, msg).Scan(&sentAt); err != nil || sentAt != nil {
			t.Errorf("%s: sent_at = %v (%v), want NULL", domain, sentAt, err)
		}
		if s := f.leadState(t, lead); s != leadstore.LeadBatched {
			t.Errorf("%s: lead is %s, want batched", domain, s)
		}
		if _, err := f.q.ResolveNotSent(ctx, msg); !errors.Is(err, leadstore.ErrMessageState) {
			t.Errorf("%s: resolve an approved message: %v", domain, err)
		}
	}
	if n, err := f.q.SentSince(ctx, dueBase); err != nil || n != 0 {
		t.Errorf("the cap still counts %d resolved-not-sent emails (%v)", n, err)
	}
	if rows, err := f.q.SendRows(ctx, at, false); err != nil || len(rows) != 2 {
		t.Errorf("resolved-not-sent emails due again: %d (%v), want 2", len(rows), err)
	}
}
