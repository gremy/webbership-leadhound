//go:build integration

package leadstore_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// TestCursorForMailbox: a cursor written for a mailbox carries its id and is that mailbox's
// alone; it moves only forward under one UIDVALIDITY, and a new UIDVALIDITY replaces it.
// The folder name stays the row's key until the cursor key is swapped, so the two mailboxes
// here use different folder names.
func TestCursorForMailbox(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := leadstoretest.SeedMailbox(t, f.q, "a@mail-a.example")
	b := leadstoretest.SeedMailbox(t, f.q, "b@mail-b.example")
	if _, ok, err := f.q.MailboxCursorFor(ctx, a.ID, "A-INBOX"); err != nil || ok {
		t.Fatalf("cursor before any sync: %v %v", ok, err)
	}
	at := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		v, uid, wantV, wantLast uint32
	}{
		{7, 10, 7, 10},
		{7, 4, 7, 10}, // never backwards under one UIDVALIDITY
		{7, 12, 7, 12},
		{9, 3, 9, 3}, // a new UIDVALIDITY replaces it
	} {
		if err := f.q.AdvanceCursorFor(ctx, a.ID, "A-INBOX", step.v, step.uid, at); err != nil {
			t.Fatal(err)
		}
		c, ok, err := f.q.MailboxCursorFor(ctx, a.ID, "A-INBOX")
		if err != nil || !ok || c.UIDValidity != step.wantV || c.LastUID != step.wantLast {
			t.Errorf("after (%d, %d): %+v %v %v", step.v, step.uid, c, ok, err)
		}
	}
	if _, ok, err := f.q.MailboxCursorFor(ctx, b.ID, "A-INBOX"); err != nil || ok {
		t.Errorf("b reads a's cursor: %v %v", ok, err)
	}
	if err := f.q.AdvanceCursorFor(ctx, b.ID, "B-INBOX", 5, 2, at); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := f.owner.QueryRow(ctx, `SELECT mailbox_id FROM mailbox_cursor WHERE mailbox = 'A-INBOX'`).Scan(&id); err != nil || id != a.ID {
		t.Errorf("A-INBOX cursor's mailbox_id = %d (%v), want %d", id, err, a.ID)
	}
	if err := f.owner.QueryRow(ctx, `SELECT mailbox_id FROM mailbox_cursor WHERE mailbox = 'B-INBOX'`).Scan(&id); err != nil || id != b.ID {
		t.Errorf("B-INBOX cursor's mailbox_id = %d (%v), want %d", id, err, b.ID)
	}

	// A row the old path wrote (no mailbox) is taken over by the first per-mailbox write,
	// which rescans rather than trusts a place it did not record.
	if err := f.q.AdvanceCursor(ctx, "INBOX", 7, 40, at); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.q.MailboxCursorFor(ctx, a.ID, "INBOX"); err != nil || ok {
		t.Errorf("a mailbox reads the old path's cursor: %v %v", ok, err)
	}
	if err := f.q.AdvanceCursorFor(ctx, a.ID, "INBOX", 7, 3, at); err != nil {
		t.Fatal(err)
	}
	if c, ok, err := f.q.MailboxCursorFor(ctx, a.ID, "INBOX"); err != nil || !ok || c.LastUID != 3 {
		t.Errorf("after the take-over: %+v %v %v, want last UID 3", c, ok, err)
	}
	// The old reader still reads by folder name.
	if c, ok, err := f.q.MailboxCursor(ctx, "B-INBOX"); err != nil || !ok || c.UIDValidity != 5 || c.LastUID != 2 {
		t.Errorf("old reader of B-INBOX: %+v %v %v", c, ok, err)
	}
}

// TestApplyIncomingForMailbox: an event recorded for a mailbox carries its id; a repeat
// changes nothing; a bounce suppresses with a source naming the mailbox, folder and UID;
// SentRefs carries the mailbox each message went from, and stays global.
func TestApplyIncomingForMailbox(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := leadstoretest.SeedMailbox(t, f.q, "a@mail-a.example")
	b := leadstoretest.SeedMailbox(t, f.q, "b@mail-b.example")
	lead, ref := f.sentLead(t, "bounced.example", "info@bounced.example")
	_, _ = f.sentLead(t, "noone.example", "info@noone.example")
	if _, err := f.owner.Exec(ctx, `UPDATE outreach_message SET mailbox_id = $1 WHERE id = $2`, a.ID, ref.ID); err != nil {
		t.Fatal(err)
	}
	refs, err := f.q.SentRefs(ctx)
	if err != nil || len(refs) != 2 {
		t.Fatalf("sent refs = %+v, %v", refs, err)
	}
	for _, r := range refs {
		want := int64(0)
		if r.ID == ref.ID {
			want = a.ID
		}
		if r.MailboxID != want {
			t.Errorf("ref %d mailbox %d, want %d", r.ID, r.MailboxID, want)
		}
		if r.ID == ref.ID {
			ref = r
		}
	}

	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// The bounce of a's message lands in b's mailbox: matching is global.
	in := leadstore.Incoming{Mailbox: "B-INBOX", UIDValidity: 5, UID: 9, Kind: mailbox.KindBounce, FromDomain: "mx.example", Matched: &ref, At: at}
	got, err := f.q.ApplyIncomingFor(ctx, b.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Duplicate || got.LeadState != leadstore.LeadBounced || got.Suppressed != 1 {
		t.Errorf("applied = %+v", got)
	}
	var mbID, eventLead int64
	if err := f.owner.QueryRow(ctx, `SELECT mailbox_id, lead_id FROM mailbox_event WHERE mailbox = 'B-INBOX' AND uid = 9`).Scan(&mbID, &eventLead); err != nil ||
		mbID != b.ID || eventLead != lead {
		t.Errorf("event mailbox %d lead %d (%v), want %d %d", mbID, eventLead, err, b.ID, lead)
	}
	var source string
	if err := f.owner.QueryRow(ctx, `SELECT source FROM suppression WHERE value = 'info@bounced.example'`).Scan(&source); err != nil ||
		source != fmt.Sprintf("mailbox:%d:B-INBOX:9", b.ID) {
		t.Errorf("suppression source %q (%v)", source, err)
	}
	if c, ok, err := f.q.MailboxCursorFor(ctx, b.ID, "B-INBOX"); err != nil || !ok || c.LastUID != 9 || c.UIDValidity != 5 {
		t.Errorf("cursor after the event: %+v %v %v", c, ok, err)
	}
	again, err := f.q.ApplyIncomingFor(ctx, b.ID, in)
	if err != nil || !again.Duplicate || again.Suppressed != 0 {
		t.Errorf("repeat = %+v, %v", again, err)
	}

	// An unmatched message is recorded with its mailbox and nothing else.
	un, err := f.q.ApplyIncomingFor(ctx, a.ID, leadstore.Incoming{Mailbox: "A-INBOX", UIDValidity: 7, UID: 1, Kind: mailbox.KindUnmatched, FromDomain: "x.example", At: at})
	if err != nil || un.Duplicate || un.Moved {
		t.Errorf("unmatched = %+v, %v", un, err)
	}
	var n int
	if err := f.owner.QueryRow(ctx, `SELECT count(*) FROM mailbox_event WHERE mailbox_id IS NULL`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d events without a mailbox (%v)", n, err)
	}
	if _, err := f.q.ApplyIncomingFor(ctx, 0, leadstore.Incoming{Mailbox: "A-INBOX", UIDValidity: 7, UID: 2, Kind: mailbox.KindUnmatched, At: at}); err == nil {
		t.Error("an event with no mailbox was recorded through the per-mailbox path")
	}
}
