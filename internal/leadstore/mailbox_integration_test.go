//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestAdvisoryLocks(t *testing.T) {
	leadstoretestReset(t)
	ctx := context.Background()
	a, b := appPool(t), appPool(t)
	l, err := leadstore.TryLock(ctx, a, leadstore.SendLockKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leadstore.TryLock(ctx, b, leadstore.SendLockKey); !errors.Is(err, leadstore.ErrLockHeld) {
		t.Fatalf("second send lock: %v, want ErrLockHeld", err)
	}
	// The sync lock is its own.
	s, err := leadstore.TryLock(ctx, b, leadstore.SyncLockKey)
	if err != nil {
		t.Fatalf("sync lock beside the send lock: %v", err)
	}
	s.Release()
	l.Release()
	l.Release() // a second release is a no-op
	l2, err := leadstore.TryLock(ctx, b, leadstore.SendLockKey)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l2.Release()
}

func leadstoretestReset(t *testing.T) { t.Helper(); newSiteFixture(t) }

func TestSenderCheckAndCursor(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	sha := leadstore.SenderDomainSHA256("Mail.Sender.EXAMPLE.")
	if string(sha) != string(leadstore.SenderDomainSHA256("mail.sender.example")) {
		t.Error("the domain hash depends on case or the root dot")
	}
	day := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	if err := f.q.RecordSenderCheck(ctx, sha, mailbox.DNSResult{SPF: true, DKIM: false, DMARC: true, Note: "dkim: not found"}, day.Add(9*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.q.SenderCheckPassedSince(ctx, sha, day); err != nil || ok {
		t.Errorf("a failing check passed: %v %v", ok, err)
	}
	if err := f.q.RecordSenderCheck(ctx, sha, mailbox.DNSResult{SPF: true, DKIM: true, DMARC: true}, day.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.q.SenderCheckPassedSince(ctx, sha, day); !ok {
		t.Error("a passing check today does not count")
	}
	if ok, _ := f.q.SenderCheckPassedSince(ctx, sha, day.AddDate(0, 0, 1)); ok {
		t.Error("yesterday's check counts for tomorrow")
	}
	if _, ok, err := f.q.MailboxCursor(ctx, "INBOX"); err != nil || ok {
		t.Fatalf("cursor before any sync: %v %v", ok, err)
	}
	at := day.Add(11 * time.Hour)
	for _, step := range []struct {
		v, uid   uint32
		wantV    uint32
		wantLast uint32
	}{
		{7, 10, 7, 10},
		{7, 4, 7, 10}, // never backwards under one UIDVALIDITY
		{7, 12, 7, 12},
		{9, 3, 9, 3}, // a new UIDVALIDITY replaces it
	} {
		if err := f.q.AdvanceCursor(ctx, "INBOX", step.v, step.uid, at); err != nil {
			t.Fatal(err)
		}
		c, ok, err := f.q.MailboxCursor(ctx, "INBOX")
		if err != nil || !ok || c.UIDValidity != step.wantV || c.LastUID != step.wantLast {
			t.Errorf("after (%d, %d): %+v %v %v", step.v, step.uid, c, ok, err)
		}
	}
}

// sentLead makes a live batch for one lead, approves it and marks step 0 sent; it returns
// the lead and step 0's SentRef.
func (f outreachFixture) sentLead(t *testing.T, domain, email string) (int64, leadstore.SentRef) {
	t.Helper()
	ctx := context.Background()
	lead := f.siteLead(t, domain, nil, 7, email)
	batchID, hash := f.batch(t, false, lead)
	f.approve(t, batchID, hash)
	ms, err := f.q.BatchMessages(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.q.MarkSending(ctx, ms[0].ID); err != nil || !ok {
		t.Fatalf("mark sending: %v %v", ok, err)
	}
	if err := f.q.MarkSent(ctx, ms[0].ID, "250 ok", time.Now()); err != nil {
		t.Fatal(err)
	}
	refs, err := f.q.SentRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.ID == ms[0].ID {
			return lead, r
		}
	}
	t.Fatalf("no sent ref for message %d", ms[0].ID)
	return 0, leadstore.SentRef{}
}

func (f outreachFixture) suppressions(t *testing.T) map[string]string {
	t.Helper()
	rows, err := f.owner.Query(context.Background(), `SELECT kind || ' ' || value, reason FROM suppression`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, r string
		if err := rows.Scan(&k, &r); err != nil {
			t.Fatal(err)
		}
		out[k] = r
	}
	return out
}

func TestApplyIncomingEffects(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	at := time.Now()
	apply := func(uid uint32, kind mailbox.MessageKind, ref *leadstore.SentRef) leadstore.Applied {
		t.Helper()
		a, err := f.q.ApplyIncoming(ctx, leadstore.Incoming{Mailbox: "INBOX", UIDValidity: 1, UID: uid, Kind: kind,
			FromDomain: "reply.localhost", Matched: ref, At: at})
		if err != nil {
			t.Fatalf("apply %s: %v", kind, err)
		}
		return a
	}

	replyLead, replyRef := f.sentLead(t, "reply.localhost", "info@reply.localhost")
	a := apply(1, mailbox.KindReply, &replyRef)
	if !a.Moved || a.LeadState != leadstore.LeadReplied || a.Cancelled != 1 {
		t.Errorf("reply: %+v", a)
	}
	eqStates(t, "reply lead", f.msgStates(t, 1, replyLead), "sent", "cancelled")
	if again := apply(1, mailbox.KindReply, &replyRef); !again.Duplicate || again.Moved {
		t.Errorf("the same UID again: %+v", again)
	}
	if again := apply(2, mailbox.KindReply, &replyRef); again.Moved || again.LeadState != leadstore.LeadReplied {
		t.Errorf("a second reply: %+v", again)
	}

	bounceLead, bounceRef := f.sentLead(t, "bounce.localhost", "info@bounce.localhost")
	a = apply(3, mailbox.KindBounce, &bounceRef)
	if a.LeadState != leadstore.LeadBounced || a.Suppressed != 1 {
		t.Errorf("bounce: %+v", a)
	}
	eqStates(t, "bounce lead", f.msgStates(t, 2, bounceLead), "sent", "cancelled")

	optLead, optRef := f.sentLead(t, "optout.localhost", "info@optout.localhost")
	a = apply(4, mailbox.KindOptOut, &optRef)
	if a.LeadState != leadstore.LeadOptedOut || a.Suppressed != 2 {
		t.Errorf("opt-out: %+v", a)
	}
	_ = optLead

	autoLead, autoRef := f.sentLead(t, "auto.localhost", "info@auto.localhost")
	if a := apply(5, mailbox.KindAutoReply, &autoRef); a.Moved || a.Cancelled != 0 {
		t.Errorf("auto reply changed something: %+v", a)
	}
	if s := f.leadState(t, autoLead); s != leadstore.LeadContacted {
		t.Errorf("auto reply lead = %s", s)
	}
	if a := apply(6, mailbox.KindUnmatched, nil); a.Moved || a.Duplicate {
		t.Errorf("unmatched: %+v", a)
	}

	got := f.suppressions(t)
	want := map[string]string{
		"address info@bounce.localhost": "bounce",
		"address info@optout.localhost": "opt_out",
		"domain optout.localhost":       "opt_out",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("suppression %q = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("suppressions %v, want %v", got, want)
	}
	// mailbox_event keeps the domain, never an address.
	var events, withAt int
	if err := f.owner.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE from_domain LIKE '%@%') FROM mailbox_event`).Scan(&events, &withAt); err != nil {
		t.Fatal(err)
	}
	if events != 6 || withAt != 0 {
		t.Errorf("events %d, with an address %d", events, withAt)
	}
	if c, _, _ := f.q.MailboxCursor(ctx, "INBOX"); c.LastUID != 6 || c.UIDValidity != 1 {
		t.Errorf("cursor %+v", c)
	}
}

// RA24: a selftest opt-out suppresses the address only.
func TestCreateSelftestAndOptOut(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	to := "operator" + "@" + "sender.localhost"
	due := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	st, err := f.q.CreateSelftest(ctx, leadstore.SelftestDraft{To: to, Subject: "Leadhound selftest", Body: "Hello", MessageID: "abc@sender.localhost", Due: due})
	if err != nil {
		t.Fatal(err)
	}
	if st.N != 1 {
		t.Errorf("first selftest is %d", st.N)
	}
	var domain, kind, source, leadKind, state, msgState string
	var shadow bool
	var approved *time.Time
	var dueAt time.Time
	if err := f.owner.QueryRow(ctx, `SELECT o.domain, o.kind, o.source, l.kind, l.state, b.shadow, b.approved_at, m.state, m.due_at
		FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE m.id = $1`, st.MessageID).Scan(&domain, &kind, &source, &leadKind, &state, &shadow, &approved, &msgState, &dueAt); err != nil {
		t.Fatal(err)
	}
	if domain != "selftest-1" || kind != "selftest" || source != "selftest" || leadKind != "selftest" || state != "batched" ||
		shadow || approved == nil || msgState != "approved" || !dueAt.Equal(due) {
		t.Errorf("selftest rows: %s %s %s %s %s shadow=%v approved=%v %s %s", domain, kind, source, leadKind, state, shadow, approved, msgState, dueAt)
	}
	var gremy int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM lead_event WHERE lead_id = $1 AND actor = 'gremy'`, st.LeadID).Scan(&gremy); err != nil || gremy != 1 {
		t.Errorf("approval by gremy recorded %d times (%v)", gremy, err)
	}
	st2, err := f.q.CreateSelftest(ctx, leadstore.SelftestDraft{To: to, Subject: "s", Body: "b", MessageID: "def@sender.localhost", Due: due})
	if err != nil || st2.N != 2 {
		t.Fatalf("second selftest: %+v %v", st2, err)
	}

	if ok, err := f.q.MarkSending(ctx, st.MessageID); err != nil || !ok {
		t.Fatal(err)
	}
	if err := f.q.MarkSent(ctx, st.MessageID, "250", due); err != nil {
		t.Fatal(err)
	}
	refs, err := f.q.SentRefs(ctx)
	if err != nil || len(refs) != 1 || !refs[0].Selftest() {
		t.Fatalf("refs %+v %v", refs, err)
	}
	a, err := f.q.ApplyIncoming(ctx, leadstore.Incoming{Mailbox: "INBOX", UIDValidity: 1, UID: 1, Kind: mailbox.KindOptOut,
		FromDomain: "sender.localhost", Matched: &refs[0], At: due})
	if err != nil {
		t.Fatal(err)
	}
	if a.LeadState != leadstore.LeadOptedOut || a.Suppressed != 1 {
		t.Errorf("selftest opt-out: %+v", a)
	}
	if got := f.suppressions(t); len(got) != 1 || got["address "+to] != "opt_out" {
		t.Errorf("selftest opt-out suppressions %v, want the address only", got)
	}
}

func TestSendRowsAndMarks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "rows.localhost", nil, 7, "info@rows.localhost")
	batchID, hash := f.batch(t, false, lead)
	f.approve(t, batchID, hash) // step 0 due dueBase, step 1 three days later
	rows, err := f.q.SendRows(ctx, dueBase, false)
	if err != nil || len(rows) != 1 || rows[0].Step != 0 || rows[0].LeadState != leadstore.LeadBatched ||
		rows[0].OrgDomain != "rows.localhost" || rows[0].Shadow {
		t.Fatalf("send rows: %+v %v", rows, err)
	}
	if shadow, _ := f.q.SendRows(ctx, dueBase, true); len(shadow) != 0 {
		t.Errorf("a live batch is due for a shadow send")
	}
	id := rows[0].ID
	if ok, _ := f.q.MarkSending(ctx, id); !ok {
		t.Fatal("mark sending")
	}
	later := dueBase.Add(time.Hour)
	if err := f.q.Requeue(ctx, id, later, "421 busy"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := f.q.SendRows(ctx, dueBase, false); len(rows) != 0 {
		t.Errorf("a requeued message is due before its new time")
	}
	if rows, _ := f.q.SendRows(ctx, later, false); len(rows) != 1 {
		t.Errorf("a requeued message is not due at its new time")
	}
	if ok, _ := f.q.MarkSending(ctx, id); !ok {
		t.Fatal("mark sending again")
	}
	if err := f.q.MarkMaybeSent(ctx, id, "connection lost"); err != nil {
		t.Fatal(err)
	}
	if err := f.q.StampAttempt(ctx, id, later); err != nil {
		t.Fatal(err)
	}
	if n, err := f.q.SentSince(ctx, dueBase); err != nil || n != 1 {
		t.Errorf("maybe_sent counts %d toward the day (%v)", n, err)
	}
	if n, _ := f.q.SentSince(ctx, later.Add(time.Minute)); n != 0 {
		t.Errorf("counted before since: %d", n)
	}
	if rows, _ := f.q.SendRows(ctx, later.AddDate(0, 0, 30), false); len(rows) != 1 || rows[0].Step != 1 {
		t.Errorf("maybe_sent came back, or step 1 is missing: %+v", rows)
	}
	// ScheduleFollowup only fills a missing due time.
	if ok, err := f.q.ScheduleFollowup(ctx, batchID, lead, 1, later); err != nil || ok {
		t.Errorf("step 1 already has a due time, yet scheduled: %v %v", ok, err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE outreach_message SET due_at = NULL WHERE batch_id = $1 AND step = 1`, batchID); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.q.ScheduleFollowup(ctx, batchID, lead, 1, later); err != nil || !ok {
		t.Errorf("schedule follow-up: %v %v", ok, err)
	}
	if _, err := f.q.SendRowByID(ctx, 9999); !errors.Is(err, leadstore.ErrMessageNotFound) {
		t.Errorf("missing row: %v", err)
	}
}
