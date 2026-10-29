//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Mailboxes assigned per campaign (Task 1.4): a live batch is drafted only onto its
// campaign's assigned mailboxes, the unassigned ones still counting toward their domain's
// cap; a live send holds a message whose mailbox is not (or no longer) assigned, without
// spending its room and without moving it to another mailbox.

// assignedSweep fails for every message of batch whose mailbox is not assigned to the
// batch's campaign, and returns how many messages it read.
func assignedSweep(t *testing.T, lq leadstore.Queries, batch int64) int {
	t.Helper()
	ctx := context.Background()
	ms, err := lq.BatchMessages(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	var campaign int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT campaign_id FROM outreach_batch WHERE id = $1`, batch).Scan(&campaign); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		ok, err := lq.MailboxAssigned(ctx, campaign, m.MailboxID)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("batch %d (campaign %d): message %d on mailbox %d, not assigned to it", batch, campaign, m.ID, m.MailboxID)
		}
	}
	return len(ms)
}

// TestCampaignMailboxBatch: with no mailbox assigned a live batch is refused with its own
// sentence (before any DNS reason), CLI and console alike; with two campaigns and two
// mailboxes on two domains every message of each campaign's batch is on one of its own.
func TestCampaignMailboxBatch(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	lq := leadstoretest.Q(t)
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	a, err := lq.MailboxByID(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	desk := seedMailbox(t, lq, "desk@other.example", withDNSPass(), withSelector("s2"), unassigned())
	cafes := secondCampaign(t)
	for _, id := range []int64{1, 2, 3} {
		leadInCampaign(t, lq, id, cafes)
	}

	// None assigned: refused first, even with today's DNS check missing.
	wantLines(t, "unassign", mustRun(t, "leads", "mailboxes", "unassign", "1", "drupal-eol"), "Mailbox 1 is unassigned from drupal-eol")
	ownerExec(t, `DELETE FROM sender_check`)
	var name string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT name FROM campaign WHERE id = 1`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	words := "No mailbox is assigned to " + name + ": assign one on its Campaign page."
	code, out, errOut := leadsRun(t, "leads", "batch", "new")
	if code != 2 || !strings.Contains(out+errOut, words) || strings.Contains(out+errOut, "domain check") {
		t.Errorf("live batch new with none assigned exited %d:\n%s%s", code, out, errOut)
	}
	ce := openCoreEnv(t)
	if _, err := makeBatch(ctx, ce.s, newBatchSetup(ce.setup, ce.loc), leadsNow(), NewBatchRequest{LiveOnly: true}); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), words) {
		t.Errorf("console with none assigned: %v", err)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 0 {
		t.Fatalf("%d batches written", n)
	}

	// drupal-eol on A, cafes on desk.
	leadsMail.resolver = passingDNS()
	mustRun(t, "leads", "mailboxes", "assign", "1", "drupal-eol")
	wantLines(t, "again", mustRun(t, "leads", "mailboxes", "assign", "1", "drupal-eol"), "Mailbox 1 is already assigned to drupal-eol.")
	mustRun(t, "leads", "mailboxes", "assign", fmt.Sprint(desk.ID), "cafes")
	ownerExec(t, `INSERT INTO sender_check (domain_sha256, selector, spf, dkim, dmarc, checked_at)
		SELECT sha256(convert_to('other.example', 'UTF8')), 's2', true, true, true, $1`, leadsNow())
	list := mustRun(t, "leads", "mailboxes", "list")
	wantLines(t, "list", list, a.Address+" · ", "campaigns drupal-eol", "desk@other.example", "campaigns cafes")

	wantLines(t, "drupal-eol", mustRun(t, "leads", "batch", "new"), "Batch 1 (live): ")
	if n := assignedSweep(t, lq, 1); n == 0 {
		t.Fatal("batch 1 has no message")
	}
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*) FROM outreach_message WHERE batch_id = 1 AND mailbox_id = %d`, desk.ID)); n != 0 {
		t.Errorf("drupal-eol's batch put %d messages on cafes' mailbox", n)
	}
	mustRun(t, "leads", "batch", "discard", "1")

	wantLines(t, "cafes", mustRun(t, "leads", "batch", "new", "--campaign", "cafes"), "Batch 2 (live): ")
	if n := assignedSweep(t, lq, 2); n == 0 {
		t.Fatal("batch 2 has no message")
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE batch_id = 2 AND mailbox_id = 1`); n != 0 {
		t.Errorf("cafes' batch put %d messages on drupal-eol's mailbox", n)
	}
	mustRun(t, "leads", "batch", "discard", "2")

	// Assigned to both: the sweep still holds for every batch.
	mustRun(t, "leads", "mailboxes", "assign", fmt.Sprint(desk.ID), "drupal-eol")
	wantLines(t, "both", mustRun(t, "leads", "batch", "new"), "Batch 3 (live): ")
	assignedSweep(t, lq, 3)

	// Plain refusals.
	for _, args := range [][]string{
		{"leads", "mailboxes", "assign", "1", "nope"},
		{"leads", "mailboxes", "assign", "999", "cafes"},
		{"leads", "mailboxes", "assign", "1"},
	} {
		if code, out, errOut := leadsRun(t, args...); code != 2 {
			t.Errorf("%q exited %d:\n%s%s", args, code, out, errOut)
		}
	}
}

// TestCampaignMailboxDomainCap: an unassigned mailbox gets no room, but its sends today
// still count toward its domain's cap.
func TestCampaignMailboxDomainCap(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	lq := leadstoretest.Q(t)
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	setDomainCap(t, dir, 4)
	second := seedMailbox(t, lq, "second@"+testSenderDomain, withDNSPass(), unassigned())
	ce := openCoreEnv(t)
	bs := newBatchSetup(ce.setup, ce.loc)
	now := leadsNow()
	mbs, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(now, bs.loc))
	if err != nil {
		t.Fatal(err)
	}
	assigned := map[int64]bool{1: true}
	size := func(sentBySecond int) (BatchSizing, map[int64]int) {
		t.Helper()
		in := make([]leadstore.MailboxStatus, len(mbs))
		copy(in, mbs)
		for i := range in {
			if in[i].ID == second.ID {
				in[i].SentToday = sentBySecond
			}
		}
		z, rooms, err := liveSizing(ctx, lq, bs, now, 10, 3, in, assigned)
		if err != nil {
			t.Fatal(err)
		}
		return z, rooms
	}
	idle, idleRooms := size(0)
	busy, busyRooms := size(3)
	if idleRooms[second.ID] != 0 || busyRooms[second.ID] != 0 || idle.Mailboxes != 1 || busy.Mailboxes != 1 {
		t.Errorf("the unassigned mailbox got room: idle %+v %v, busy %+v %v", idle, idleRooms, busy, busyRooms)
	}
	if busyRooms[1] >= idleRooms[1] {
		t.Errorf("the first mailbox's room is %d with the unassigned one's 3 sends today, %d without: they did not count toward the domain",
			busyRooms[1], idleRooms[1])
	}

	// Through batch new: the same domain room, whoever sent.
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, "Batch 1 (live): ") || strings.Contains(out, "mailboxes") {
		t.Errorf("batch new, one mailbox assigned:\n%s", out)
	}
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*) FROM outreach_message WHERE mailbox_id = %d`, second.ID)); n != 0 {
		t.Errorf("the unassigned mailbox got %d messages", n)
	}
	assignedSweep(t, lq, 1)
}

// TestCampaignMailboxSend: a message approved on an unassigned mailbox is held without
// using its room, then sends once the mailbox is assigned; an unassign during a run holds
// the rest at the claim; follow-ups of an unassigned mailbox are held and keep their
// mailbox; a selftest needs no assignment.
func TestCampaignMailboxSend(t *testing.T) {
	ctx := context.Background()
	w := newMultiWorld(t, 1)
	b := w.add(t, "b@"+"other.example")
	if _, err := w.lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, b.ID, false, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	w.batchFrom(t, b, false, w.leads(t, "b1.example", "b2.example"))

	out := w.mustRun(t, "leads", "send")
	wantLines(t, "unassigned", out, "Held for a later run: mailbox not assigned to the campaign 2.")
	if strings.Contains(out, "today's cap") || len(w.sentFrom(b.Address)) != 0 {
		t.Errorf("an unassigned mailbox's messages spent room or sent:\n%s", out)
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "unassigned 2") {
		t.Errorf("run note %q", note)
	}

	w.mustRun(t, "leads", "mailboxes", "assign", fmt.Sprint(b.ID), "drupal-eol")
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "assigned", out, "Sent 1: ", "Held for a later run: today's cap 1.")
	if got := w.sentFrom(b.Address); len(got) != 1 {
		t.Errorf("B sent %v, want 1 (its cap)", got)
	}

	// Follow-ups of an unassigned mailbox: held, on their own mailbox, never moved.
	w.mustRun(t, "leads", "mailboxes", "unassign", fmt.Sprint(b.ID), "drupal-eol")
	w.clock.set(w.clock.now().Add(24 * time.Hour))
	ownerExec(t, `UPDATE outreach_message SET due_at = $1 WHERE mailbox_id = $2 AND state = 'approved' AND due_at IS NOT NULL`, w.clock.now(), b.ID)
	before := w.count(t, `SELECT count(*) FROM outreach_message WHERE mailbox_id = $1 AND state = 'approved'`, b.ID)
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE mailbox_id = $1 AND state = 'approved' AND step = 1 AND due_at <= $2`, b.ID, w.clock.now()); n != 1 {
		t.Fatalf("%d follow-ups due on B, want b1's", n)
	}
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "follow-ups", out, "mailbox not assigned to the campaign 2")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE mailbox_id = $1 AND state = 'approved'`, b.ID); n != before || n == 0 {
		t.Errorf("B's approved messages %d, were %d: a held follow-up moved or went", n, before)
	}
	if got := w.sentFrom(b.Address); len(got) != 1 {
		t.Errorf("B sent %v after its unassign", got)
	}
	if len(w.sentFrom(w.a.Address)) != 0 {
		t.Error("a held message went from A")
	}
	w.sweep(t, b.Address)
}

// TestCampaignMailboxUnassignMidRun: an unassign during a run holds the messages not yet
// claimed (the claim reads the assignment again); they keep their mailbox.
func TestCampaignMailboxUnassignMidRun(t *testing.T) {
	ctx := context.Background()
	w := newMultiWorld(t, 20)
	w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example", "a3.example"))
	inner := w.smtp.onSend
	w.smtp.onSend = func(n int, to string) {
		inner(n, to)
		if n == 0 {
			if _, err := leadstoretest.Q(t).SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, w.a.ID, false, leadstore.ActorGremy); err != nil {
				t.Error(err)
			}
		}
	}
	out := w.mustRun(t, "leads", "send")
	wantLines(t, "mid-run", out, "Sent 1: ", "mailbox not assigned to the campaign 2")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE mailbox_id = $1 AND step = 0 AND state = 'approved'`, w.a.ID); n != 2 {
		t.Errorf("%d first emails still approved on A, want the 2 held", n)
	}
}

// TestCampaignMailboxSelftest: a selftest goes with no mailbox assigned to any campaign.
func TestCampaignMailboxSelftest(t *testing.T) {
	w := newMultiWorld(t, 20)
	w.mustRun(t, "leads", "mailboxes", "unassign", fmt.Sprint(w.a.ID), "drupal-eol")
	out := w.mustRun(t, "leads", "selftest", "--to", testOperator)
	wantLines(t, "selftest", out, "Sent 1: your selftest address")
}
