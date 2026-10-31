//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

// The Send now fixes after the Checkpoint 5 review: a lead a hard stop leaves out of a manual
// batch goes back to new (most stops hold only today), Send now again asks once more for a
// manual batch whose send never ran, and a preview racing another tab's confirm is a refusal.

// twoMailboxScene is manualScene with a second ready mailbox on another domain, assigned to the
// campaign, and a manual batch of the first two email leads previewed: Assign gives each to the
// mailbox with the most room, so they sit on different mailboxes.
func twoMailboxScene(t *testing.T) (*backend, string, []int64, console.SendPlan) {
	t.Helper()
	ctx := context.Background()
	b, dir, ids := manualScene(t)
	seedMailbox(t, leadstoretest.Q(t), "desk@other.example", withDNSPass(), withSelector("s2"))
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	var mbs int
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT count(DISTINCT mailbox_id)::int FROM outreach_message WHERE batch_id = $1 AND step = 0`, p.BatchID).Scan(&mbs); err != nil || mbs != 2 {
		t.Fatalf("the two leads are on %d mailboxes: %v", mbs, err)
	}
	return b, dir, ids, p
}

func leadState(t *testing.T, lead int64) string {
	t.Helper()
	var s string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT state FROM lead WHERE id = $1`, lead).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func lastLeadReason(t *testing.T, lead int64) string {
	t.Helper()
	var s string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT reason FROM lead_event WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, lead).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSendNowLeftOutGoesBackToNew: a lead a stop that holds only today leaves out of a manual
// batch (its mailbox at the hard ceiling, or paused) is new again after confirm, with the reason
// in its history and its messages skipped, while the other lead is approved; the next preview
// can take it again.
func TestSendNowLeftOutGoesBackToNew(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, lead int64)
	}{
		{"mailbox ceiling", func(t *testing.T, lead int64) { fillMailbox(t, lead, 40) }},
		{"mailbox paused", func(t *testing.T, lead int64) {
			ownerExec(t, `INSERT INTO mailbox_state (mailbox_id, state, reason, actor)
			  SELECT DISTINCT m.mailbox_id, 'paused', 'test', 'operator' FROM outreach_message m WHERE m.lead_id = $1`, lead)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, _, ids, p := twoMailboxScene(t)
			tc.stop(t, ids[0])
			p, err := b.SendNowPlan(ctx, p.BatchID)
			if err != nil {
				t.Fatal(err)
			}
			if l := planLead(p, ids[0]); l.Stop == "" || !planLead(p, ids[1]).OK || !p.CanSend {
				t.Fatalf("plan %+v", p)
			} else {
				t.Logf("stop: %s", l.Stop)
			}
			if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
				t.Fatal(err)
			}
			if s := leadState(t, ids[0]); s != leadstore.LeadNew {
				t.Errorf("the left-out lead is %s, want new", s)
			}
			if want := "left out of manual batch " + strconv.FormatInt(p.BatchID, 10); lastLeadReason(t, ids[0]) != want {
				t.Errorf("its last event says %q, want %q", lastLeadReason(t, ids[0]), want)
			}
			if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE batch_id = `+strconv.FormatInt(p.BatchID, 10)+
				` AND lead_id = `+strconv.FormatInt(ids[0], 10)+` AND state <> 'skipped'`); n != 0 {
				t.Errorf("%d of its messages not skipped", n)
			}
			if n := approvedCount(t, ids[1]); n == 0 {
				t.Error("the other lead was not approved")
			}
			if s := leadState(t, ids[1]); s != leadstore.LeadBatched {
				t.Errorf("the other lead is %s, want batched", s)
			}
			// Back in the queue: a new preview takes it.
			ownerExec(t, `DELETE FROM mailbox_state`)
			ownerExec(t, `DELETE FROM outreach_message WHERE state = 'sent'`)
			q, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
			if err != nil || !planLead(q, ids[0]).OK {
				t.Errorf("the lead again: %+v %v", q, err)
			}
		})
	}
}

// TestDailyBatchSkipStillSkips: the operator's own leave-out on Approve still moves the lead to
// skipped; only a manual batch's hard stops send a lead back to new.
func TestDailyBatchSkipStillSkips(t *testing.T) {
	ctx := context.Background()
	b, _, _ := manualScene(t)
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch})
	if err != nil || out.BatchID == 0 {
		t.Fatalf("daily batch: %+v %v", out, err)
	}
	msgs, err := b.lq().BatchMessages(ctx, out.BatchID)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("messages %v %v", msgs, err)
	}
	first := msgs[0].LeadID // item 1
	if _, err := approveBatch(ctx, b.s, mustSetup(t, b).leads, leadsNow, ApproveRequest{ID: out.BatchID,
		HashPrefix: batchHash(t, out.BatchID), Skip: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if s := leadState(t, first); s != leadstore.LeadSkipped {
		t.Errorf("the lead the operator left out is %s, want skipped", s)
	}
	if r := lastLeadReason(t, first); !strings.HasPrefix(r, "skipped at approval of batch ") {
		t.Errorf("its last event says %q", r)
	}
}

// TestSendNowAgain: a confirmed manual batch whose request expired (the run never came) can be
// asked again: the page shows the plan and its warnings once more, the press writes a fresh
// send_request without approving anything again, and the run sends it once. A batch that went
// cannot be asked again, and the plain confirm still refuses an approved batch.
func TestSendNowAgain(t *testing.T) {
	ctx := context.Background()
	b, dir, ids := manualScene(t)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
		t.Fatal(err)
	}
	var approvedAt time.Time
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT approved_at FROM outreach_batch WHERE id = $1`, p.BatchID).Scan(&approvedAt); err != nil {
		t.Fatal(err)
	}
	// The hourly send held the lock past the request's 15 minutes: when the unit runs at last,
	// the request has expired and nothing goes.
	ownerExec(t, `UPDATE send_request SET created_at = $1`, leadsNow().Add(-16*time.Minute))
	leadsMail.sleep = func(context.Context, time.Duration) error { return nil } // goLive restores leadsMail
	wantLines(t, "expired", mustRun(t, "leads", "send", "--from-request"), "has no confirmed Send now waiting")
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE state = 'sent'`); n != 0 {
		t.Fatalf("an expired request sent %d", n)
	}
	if _, err := os.Stat(filepath.Join(dir, runRequestDir, sendNowAction)); err != nil {
		t.Logf("request file after the run: %v", err)
	}
	lb, err := b.LeadBatch(ctx, p.BatchID)
	if err != nil || !lb.SendAgain {
		t.Fatalf("the batch page does not offer Send now again: %+v %v", lb.SendAgain, err)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "already sent") {
		t.Errorf("the plain confirm on the approved batch: %v", err)
	}
	a, err := b.SendNowAgainPlan(ctx, p.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Again || !a.CanSend || len(a.Warnings) == 0 || !planLead(a, ids[0]).OK || !planLead(a, ids[1]).OK {
		t.Fatalf("again plan %+v", a)
	}
	// The tick box is the console's; the codes must still be every warning shown.
	short := confirmOf(a)
	short.Again, short.Codes = true, short.Codes[1:]
	if _, err := b.SendNowConfirm(ctx, short); !errors.As(err, new(*console.PlanChangedError)) {
		t.Errorf("a warning not posted: %v", err)
	}
	c := confirmOf(a)
	c.Again = true
	out, err := b.SendNowConfirm(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out.Message)
	// The expired request and the fresh one, neither used yet.
	if n := batchCount(t, `SELECT count(*)::int FROM send_request WHERE used_at IS NULL`); n != 2 {
		t.Errorf("%d unused send requests, want 2", n)
	}
	var again time.Time
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT approved_at FROM outreach_batch WHERE id = $1`, p.BatchID).Scan(&again); err != nil || !again.Equal(approvedAt) {
		t.Errorf("approved again: %v (was %v) %v", again, approvedAt, err)
	}
	mustRun(t, "leads", "send", "--from-request")
	if n := batchCount(t, `SELECT count(*)::int FROM send_request WHERE used_at IS NOT NULL AND created_at > now() - interval '1 day'`); n != 1 {
		t.Errorf("%d fresh requests claimed, want 1", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE batch_id = `+strconv.FormatInt(p.BatchID, 10)+` AND step = 0 AND state = 'sent'`); n != 2 {
		t.Errorf("%d first emails sent, want 2", n)
	}
	// Went: never again.
	if lb, err := b.LeadBatch(ctx, p.BatchID); err != nil || lb.SendAgain {
		t.Errorf("a sent batch offers Send now again: %v", err)
	}
	if _, err := b.SendNowAgainPlan(ctx, p.BatchID); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "already went") {
		t.Errorf("the again plan of a sent batch: %v", err)
	}
	if _, err := b.SendNowConfirm(ctx, c); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("again on a sent batch: %v", err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM send_request`); n != 2 {
		t.Errorf("%d send requests, want 2", n)
	}
}

// TestSendNowAgainNotADraft: Send now again reads only an approved manual batch: a batch still
// waiting for its confirm is a refusal there.
func TestSendNowAgainNotADraft(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendNowAgainPlan(ctx, p.BatchID); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("again on an unconfirmed batch: %v", err)
	}
	c := confirmOf(p)
	c.Again = true
	if _, err := b.SendNowConfirm(ctx, c); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("again's press on an unconfirmed batch: %v", err)
	}
	if n := approvedCount(t, ids[0]); n != 0 {
		t.Errorf("%d approved", n)
	}
}

// TestSendNowPreviewRaceIsARefusal: when another tab confirmed the open manual batch between
// the preview's read and its discard, the preview is a refusal asking to start again, never a
// 500; started again, the lead is named taken.
func TestSendNowPreviewRaceIsARefusal(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
		t.Fatal(err)
	}
	err = manualTx(ctx, b, func(_ pgx.Tx, tq leadstore.Queries) error {
		return discardOpenManual(ctx, tq, []int64{p.BatchID})
	})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Start Send now again") {
		t.Fatalf("discarding a batch another tab confirmed: %v", err)
	}
	t.Log(err)
	_, err = b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Its first email is already in batch ") {
		t.Errorf("started again: %v", err)
	}
}
