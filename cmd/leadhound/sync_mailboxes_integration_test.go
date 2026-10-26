//go:build integration

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// The per-mailbox sync (MB-R10, MB-R11). The second mailbox of most tests here names its
// folders B-INBOX and B-Junk (renamedReader), so each folder's rows are told apart by name
// too; TestSyncAllSameFolderTwoMailboxes reads INBOX in both, keyed by mailbox alone
// (migration 00008).

// renamedReader shows a reader's folders under prefix+name.
type renamedReader struct {
	mailbox.Reader
	prefix string
}

func (r renamedReader) ListMailboxes() (mailbox.Mailboxes, error) {
	b, err := r.Reader.ListMailboxes()
	b.Inbox, b.Sent, b.Junk = r.prefix+b.Inbox, r.prefix+b.Sent, r.prefix+b.Junk
	return b, err
}

func (r renamedReader) Select(name string) (uint32, error) {
	return r.Reader.Select(strings.TrimPrefix(name, r.prefix))
}

// syncWorld is a send world (mailbox A: testUser, folders INBOX and Junk) with a second
// mailbox B whose folders are B-INBOX and B-Junk.
type syncWorld struct {
	*sendWorld
	a, b leadstore.Mailbox
	cfg  leadconfig.Config
}

const bLogin = "b@other.example"

func newSyncWorld(t *testing.T) *syncWorld {
	t.Helper()
	w := &syncWorld{sendWorld: newSendWorld(t, true, 20)}
	w.a = w.mailbox
	w.b = seedMailbox(t, w.lq, bLogin)
	plain := w.mail.readerFor
	leadsMail.readerFor = func(ctx context.Context, addr string, user, password config.Secret) (mailbox.Reader, error) {
		r, err := plain(ctx, addr, user, password)
		if err == nil && strings.EqualFold(user.Reveal(), bLogin) {
			return renamedReader{Reader: r, prefix: "B-"}, nil
		}
		return r, err
	}
	cfg, err := leadconfig.Load(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	w.cfg = cfg
	return w
}

// sync runs syncAll with the test key and leads.yaml.
func (w *syncWorld) sync(t *testing.T) map[int64]mailboxSync {
	t.Helper()
	got, err := syncAll(context.Background(), storetestPool(t), currentTestKey(t), w.cfg, 0)
	if err != nil {
		t.Fatalf("syncAll: %v", err)
	}
	return got
}

// sendFrom sends one live first email to each domain and records it as sent from mb.
func (w *syncWorld) sendFrom(t *testing.T, mb leadstore.Mailbox, domains ...string) {
	t.Helper()
	leads := map[int64]string{}
	for _, d := range domains {
		leads[w.lead(t, d)] = d
	}
	w.batch(t, false, leads)
	before := w.smtp.count()
	w.mustRun(t, "leads", "send")
	if got := w.smtp.count() - before; got != len(domains) {
		t.Fatalf("sent %d, want %d", got, len(domains))
	}
	for _, d := range domains {
		ownerExec(t, `UPDATE outreach_message m SET mailbox_id = $1 FROM lead l JOIN org o ON o.id = l.org_id
			WHERE l.id = m.lead_id AND o.domain = $2`, mb.ID, d)
	}
}

// deliverDSN puts a delivery-status report for to in folder of box.
func deliverDSN(box *fakeBox, folder, to string) {
	box.mu.Lock()
	defer box.mu.Unlock()
	uid := uint32(len(box.msgs[folder]) + 1) // #nosec G115 -- a handful of test messages
	box.msgs[folder] = append(box.msgs[folder], mailbox.Fetched{UID: uid,
		Header: []byte("From: Mail Delivery System <mailer-daemon@mx.bounce.example>\r\nReturn-Path: <>\r\nSubject: Undelivered\r\n" +
			"Content-Type: multipart/report; report-type=delivery-status; boundary=\"b1\"\r\n\r\n"),
		Report: [][]byte{[]byte("Reporting-MTA: dns; mx.bounce.example\r\n\r\nFinal-Recipient: rfc822; " + to + "\r\nAction: failed\r\nStatus: 5.1.1\r\n")}})
}

// TestSyncAllReadsEveryMailbox: each mailbox is read with its own login; events and cursors
// carry their mailbox; a reply landing in B to a message sent from A stops A's lead; the
// first sync of a mailbox rescans by date and the next reads from its cursor.
func TestSyncAllReadsEveryMailbox(t *testing.T) {
	w := newSyncWorld(t)
	w.sendFrom(t, w.a, "reply.example", "quiet.example")
	boxB := w.mail.boxOf(bLogin)
	boxB.deliver("INBOX", replyHeader("reply.example", w.firstMessageID(t, "reply.example"), "Re: hello"), "Sounds interesting.")
	// Every folder holds a message: an empty one is rescanned by date on every sync.
	boxB.deliver("Junk", "From: Shop <news@unrelated.example>\r\nSubject: Sale\r\n\r\n", "Big sale.")
	w.box.deliver("INBOX", "From: Shop <news@unrelated.example>\r\nSubject: Sale\r\n\r\n", "Big sale.")
	w.box.deliver("Junk", "From: Shop <news@unrelated.example>\r\nSubject: Sale again\r\n\r\n", "Big sale.")
	aByDate, bByDate := w.box.byDate, boxB.byDate
	// The send in sendFrom synced and filed its Sent copies through the readers too.
	readersBefore := len(w.mail.callsOf("reader"))

	got := w.sync(t)
	if len(got) != 2 || !got[w.a.ID].OK || !got[w.b.ID].OK {
		t.Fatalf("syncAll = %+v", got)
	}
	if s := got[w.b.ID].Summary; !slices.Equal(s.ToRead, []string{"reply.example"}) || s.Messages != 2 || !s.Junk {
		t.Errorf("B summary %+v", s)
	}
	if s := got[w.a.ID].Summary; s.Messages != 2 || s.ByKind[mailbox.KindUnmatched] != 2 || !s.Junk {
		t.Errorf("A summary %+v", s)
	}
	if s := w.leadState(t, "reply.example"); s != leadstore.LeadReplied {
		t.Errorf("reply.example lead %s, want replied", s)
	}
	if s := w.msgState(t, "reply.example", 1); s != "cancelled" {
		t.Errorf("reply.example follow-up %s, want cancelled", s)
	}
	if s := w.leadState(t, "quiet.example"); s != leadstore.LeadContacted {
		t.Errorf("quiet.example lead %s", s)
	}
	for _, c := range []struct {
		folder string
		id     int64
		n      int
	}{{"INBOX", w.a.ID, 1}, {"Junk", w.a.ID, 1}, {"B-INBOX", w.b.ID, 1}, {"B-Junk", w.b.ID, 1}} {
		if n := w.count(t, `SELECT count(*) FROM mailbox_event WHERE mailbox = $1 AND mailbox_id = $2`, c.folder, c.id); n != c.n {
			t.Errorf("%s: %d events of mailbox %d, want %d", c.folder, n, c.id, c.n)
		}
		if n := w.count(t, `SELECT count(*) FROM mailbox_cursor WHERE mailbox = $1 AND mailbox_id = $2`, c.folder, c.id); n != 1 {
			t.Errorf("%s: no cursor of mailbox %d", c.folder, c.id)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM mailbox_event WHERE mailbox_id IS NULL`); n != 0 {
		t.Errorf("%d events without a mailbox", n)
	}
	if w.box.byDate != aByDate+2 || boxB.byDate != bByDate+2 {
		t.Errorf("first sync rescans by date: A %d→%d, B %d→%d", aByDate, w.box.byDate, bByDate, boxB.byDate)
	}
	if calls := w.mail.callsOf("reader")[readersBefore:]; len(calls) != 2 || !calls[0].passwordOK || !calls[1].passwordOK {
		t.Errorf("reader calls %+v", calls)
	}

	// Again: nothing new, each read from its own cursor.
	again := w.sync(t)
	if again[w.a.ID].Summary.Messages != 0 || again[w.b.ID].Summary.Messages != 0 || !again[w.a.ID].OK || !again[w.b.ID].OK {
		t.Errorf("second sync %+v", again)
	}
	if w.box.byDate != aByDate+2 || boxB.byDate != bByDate+2 {
		t.Error("a second sync rescanned by date")
	}
}

// TestSyncAllOneMailboxDown: a mailbox whose IMAP is down, or whose sealed password cannot
// be used, gets an entry that is not OK with a plain note, and never stops the others; a
// paused mailbox still syncs; a retired one is not read.
func TestSyncAllOneMailboxDown(t *testing.T) {
	w := newSyncWorld(t)
	w.sendFrom(t, w.a, "reply.example")
	boxB := w.mail.boxOf(bLogin)
	boxB.mu.Lock()
	boxB.dialErr = &mailbox.IMAPError{Stage: "dial", Text: "connection refused"}
	boxB.mu.Unlock()
	broken := seedMailbox(t, w.lq, "broken@third.example")
	ownerExec(t, `UPDATE sender_mailbox SET imap_addr = 'imap.moved.invalid:993' WHERE id = $1`, broken.ID)
	retired := seedMailbox(t, w.lq, "gone@fourth.example")
	if err := w.lq.SetMailboxState(context.Background(), retired.ID, leadstore.MailboxRetired, "no longer used", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if err := w.lq.SetMailboxState(context.Background(), w.a.ID, leadstore.MailboxPaused, "a break", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	w.box.deliver("INBOX", replyHeader("reply.example", w.firstMessageID(t, "reply.example"), "Re: hello"), "Yes please.")
	// The send in sendFrom synced and filed its Sent copy through the readers too.
	readersBefore := len(w.mail.callsOf("reader"))

	got := w.sync(t)
	if _, ok := got[retired.ID]; ok || len(got) != 3 {
		t.Errorf("entries %+v, want A, B and the broken one", got)
	}
	if a := got[w.a.ID]; !a.OK || a.Summary.Messages != 1 {
		t.Errorf("paused A = %+v, want synced", a)
	}
	if s := w.leadState(t, "reply.example"); s != leadstore.LeadReplied {
		t.Errorf("reply.example lead %s", s)
	}
	if b := got[w.b.ID]; b.OK || !strings.Contains(b.Note, "connection refused") {
		t.Errorf("B = %+v, want not OK naming the refusal", b)
	}
	br := got[broken.ID]
	if br.OK || !strings.Contains(br.Note, "re-enter the password") {
		t.Errorf("broken = %+v", br)
	}
	if rows := mailboxStateRows(t, broken.ID); len(rows) != 1 || !strings.HasPrefix(rows[0], "paused mailbox system") {
		t.Errorf("broken state rows %q", rows)
	}
	for _, e := range got {
		if strings.Contains(e.Note, testPassword) || strings.Contains(e.Note, "@") {
			t.Errorf("a note holds a secret or an address: %q", e.Note)
		}
	}
	if n := len(w.mail.callsOf("reader")) - readersBefore; n != 2 {
		t.Errorf("%d reader calls, want 2 (A and B; never the broken or retired one)", n)
	}
}

// TestSyncAllBounceHealth: a DSN bounce of a message is a hard bounce on that message; the
// second in its mailbox's last 50 live sends pauses that mailbox (system) wherever the
// bounce landed, once; the other mailbox stays active.
func TestSyncAllBounceHealth(t *testing.T) {
	w := newSyncWorld(t)
	w.sendFrom(t, w.a, "one.example", "two.example", "three.example")
	boxB := w.mail.boxOf(bLogin)

	deliverDSN(boxB, "INBOX", "info@one.example")
	got := w.sync(t)
	if got[w.b.ID].Summary.ByKind[mailbox.KindBounce] != 1 {
		t.Fatalf("B summary %+v", got[w.b.ID].Summary)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE hard_bounce`); n != 1 {
		t.Errorf("%d hard bounces, want 1", n)
	}
	if rows := mailboxStateRows(t, w.a.ID); len(rows) != 0 {
		t.Errorf("one bounce paused A: %q", rows)
	}

	deliverDSN(w.box, "INBOX", "info@two.example")
	w.sync(t)
	want := "paused mailbox system: 2 hard bounces in its last 50 live sends"
	if rows := mailboxStateRows(t, w.a.ID); len(rows) != 1 || rows[0] != want {
		t.Errorf("A state rows %q, want [%q]", rows, want)
	}
	if rows := mailboxStateRows(t, w.b.ID); len(rows) != 0 {
		t.Errorf("B state rows %q, want none", rows)
	}

	// A third bounce while paused adds no second pause.
	deliverDSN(boxB, "INBOX", "info@three.example")
	w.sync(t)
	if rows := mailboxStateRows(t, w.a.ID); len(rows) != 1 {
		t.Errorf("A state rows %q, want one pause", rows)
	}
	paused, err := checkBounceHealth(context.Background(), w.lq, w.cfg, w.a.ID)
	if err != nil || !paused {
		t.Errorf("checkBounceHealth of paused A = %v, %v", paused, err)
	}
	paused, err = checkBounceHealth(context.Background(), w.lq, w.cfg, w.b.ID)
	if err != nil || paused {
		t.Errorf("checkBounceHealth of B = %v, %v", paused, err)
	}
}

// TestSyncAllLockHeld: a sync already running makes syncAll return errSyncRunning and read
// nothing.
func TestSyncAllLockHeld(t *testing.T) {
	w := newSyncWorld(t)
	l, err := leadstore.TryLock(context.Background(), storetestPool(t), leadstore.SyncLockKey)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := syncAll(context.Background(), storetestPool(t), currentTestKey(t), w.cfg, 0); !errors.Is(err, errSyncRunning) {
		t.Errorf("syncAll under a held lock: %v", err)
	}
	if n := len(w.mail.callsOf("reader")); n != 0 {
		t.Errorf("%d reader calls under a held lock", n)
	}
}

// TestSyncAllSameFolderTwoMailboxes: two mailboxes that both read INBOX, each holding a
// message with UID 1 under the same UIDVALIDITY, keep one event and one cursor each.
func TestSyncAllSameFolderTwoMailboxes(t *testing.T) {
	w := newSyncWorld(t)
	leadsMail.readerFor = w.mail.readerFor // B reads its folders under their own names
	boxB := w.mail.boxOf(bLogin)
	for _, box := range []*fakeBox{w.box, boxB} {
		box.deliver("INBOX", "From: Shop <news@unrelated.example>\r\nSubject: Sale\r\n\r\n", "Big sale.")
		box.deliver("Junk", "From: Shop <news@unrelated.example>\r\nSubject: Sale again\r\n\r\n", "Big sale.")
	}

	got := w.sync(t)
	if !got[w.a.ID].OK || !got[w.b.ID].OK || got[w.a.ID].Summary.Messages != 2 || got[w.b.ID].Summary.Messages != 2 {
		t.Fatalf("syncAll = %+v", got)
	}
	for _, mb := range []leadstore.Mailbox{w.a, w.b} {
		for _, folder := range []string{"INBOX", "Junk"} {
			if n := w.count(t, `SELECT count(*) FROM mailbox_event WHERE mailbox = $1 AND uid = 1 AND mailbox_id = $2`, folder, mb.ID); n != 1 {
				t.Errorf("%s UID 1 of mailbox %d: %d events, want 1", folder, mb.ID, n)
			}
			c, ok, err := w.lq.MailboxCursorFor(context.Background(), mb.ID, folder)
			if err != nil || !ok || c.LastUID != 1 {
				t.Errorf("%s cursor of mailbox %d = %+v, %v, %v; want UID 1", folder, mb.ID, c, ok, err)
			}
		}
	}
	if n := w.count(t, `SELECT count(*) FROM mailbox_cursor WHERE mailbox = 'INBOX'`); n != 2 {
		t.Errorf("%d INBOX cursors, want 2", n)
	}

	// Again: each reads from its own cursor and records nothing twice.
	again := w.sync(t)
	if again[w.a.ID].Summary.Messages != 0 || again[w.b.ID].Summary.Messages != 0 {
		t.Errorf("second sync %+v", again)
	}
	if n := w.count(t, `SELECT count(*) FROM mailbox_event`); n != 4 {
		t.Errorf("%d events after a second sync, want 4", n)
	}
}

// TestSyncAllPausesOnce: a mailbox whose password cannot be used is paused once, however many
// syncs find it so, and a pause the operator set for its domain keeps its reason and scope.
func TestSyncAllPausesOnce(t *testing.T) {
	w := newSyncWorld(t)
	ctx := context.Background()
	broken := seedMailbox(t, w.lq, "broken@third.example")
	held := seedMailbox(t, w.lq, "held@fourth.example")
	if _, err := w.lq.SetDomainState(ctx, "fourth.example", leadstore.MailboxPaused, "the operator's break", leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{broken.ID, held.ID} {
		ownerExec(t, `UPDATE sender_mailbox SET imap_addr = 'imap.moved.invalid:993' WHERE id = $1`, id)
	}
	for range 2 {
		got := w.sync(t)
		if got[broken.ID].OK || got[held.ID].OK {
			t.Errorf("a mailbox with an unusable password synced: %+v", got)
		}
	}
	if rows := mailboxStateRows(t, broken.ID); len(rows) != 1 || !strings.HasPrefix(rows[0], "paused mailbox system: re-enter the password") {
		t.Errorf("broken state rows %q, want one pause", rows)
	}
	if rows := mailboxStateRows(t, held.ID); len(rows) != 1 || rows[0] != "paused domain operator: the operator's break" {
		t.Errorf("held state rows %q, want the operator's domain pause alone", rows)
	}
}
