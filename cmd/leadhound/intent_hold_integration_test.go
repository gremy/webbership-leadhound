//go:build integration

package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/intent"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
)

// The loyalty intent hold on the cmd roads (D8). The order itself and CallQueue are swept in
// internal/leadstore (intent_order_integration_test.go).

// TestIntentHoldSendOne (the grid row): leads batched, then one marked Out by your call and
// one newly written just chose (as a sync writes it): at send their messages are cancelled
// with the hold's reason, nothing reaches SMTP for them, both are back to new with the
// hold's event, and the batch's other lead is sent. Removing the check in sendOne sends to
// both held leads and fails this test.
func TestIntentHoldSendOne(t *testing.T) {
	w := newSendWorld(t, true, 20)
	ctx := context.Background()
	ids := map[string]int64{}
	leads := map[int64]string{}
	for _, d := range []string{"out-call.example", "just-chose.example", "plain-cafe.example"} {
		id := w.lead(t, d)
		ownerExec(t, `UPDATE lead SET kind = $2 WHERE id = $1`, id, cafe.KindNoDigitalLoyalty)
		ids[d], leads[id] = id, d
	}
	w.batch(t, false, leads)
	out := intent.Out
	if err := w.lq.SetIntentCall(ctx, ids["out-call.example"], &out, ""); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `INSERT INTO lead_intent (lead_id, class, overlap, score, reason, signal_at, held_until)
		VALUES ($1, 'just_chose', 1, 0, 'chose a vendor', now(), now() + interval '60 days')`, ids["just-chose.example"])

	got := w.mustRun(t, "leads", "send")
	if r := w.smtp.recipients(); !slices.Equal(r, []string{"info@plain-cafe.example"}) {
		t.Fatalf("SMTP got %v, want the plain café only:\n%s", r, got)
	}
	for _, d := range []string{"out-call.example", "just-chose.example"} {
		wantLines(t, d, got, d+", "+leadstore.SkipIntentHeld, "It is back to new, kept for later.")
		if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE lead_id = $1 AND (state <> 'cancelled' OR cancel_reason <> $2)`,
			ids[d], leadstore.SkipIntentHeld); n != 0 {
			t.Errorf("%s: %d messages not cancelled as held (its call step included)", d, n)
		}
		if s := w.leadState(t, d); s != leadstore.LeadNew {
			t.Errorf("%s is %s, want new", d, s)
		}
		if r := w.str(t, `SELECT reason FROM lead_event WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, ids[d]); r != leadstore.SkipIntentHeld {
			t.Errorf("%s's last event %q", d, r)
		}
	}
	if s := w.leadState(t, "plain-cafe.example"); s != leadstore.LeadContacted {
		t.Errorf("the batch's other lead is %s, want contacted", s)
	}
}

// TestIntentHoldBatchNew: a café batch (leads batch new) syncs first, so a stale derived Just
// chose (one the café's signals no longer give) is released and drafted, while your call Out
// (set by `leads intent set`) is held and gets no message. Send now on chosen leads
// (manualQueue) still offers the held lead; the batch's own queue does not.
func TestIntentHoldBatchNew(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	cafesDataDir(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })
	seedMailbox(t, leadstoretest.Q(t), testUser, withDNSPass())
	importDemoPlaces(t, dir)
	sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", cafeFixtures(t))
	domains := []string{"harbour-lane-coffee.example", "bean-house.example", "cafeneaua-exemplu.example"}
	for _, d := range domains {
		cafeRoute(t, d)
	}
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	held, stale := leadIDOf(t, domains[0]), leadIDOf(t, domains[1])
	mustRun(t, "leads", "intent", "set", strconv.FormatInt(held, 10), intent.Out)
	ownerExec(t, `INSERT INTO lead_intent (lead_id, class, overlap, score, reason, signal_at, held_until)
		VALUES ($1, 'just_chose', 1, 0, 'chose a vendor', now(), now() + interval '60 days')`, stale)

	// The queues before the build: the batch's holds both, Send now's offers the held one.
	b := leadsBackend(t)
	lq := leadstoretest.Q(t)
	bs := newBatchSetup(mustSetup(t, b), b.loc)
	c, err := activeCampaign(ctx, lq, "cafes-loyalty")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := resolveBatchPlan(ctx, lq, bs, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	queue, _, _, _, err := batchQueue(ctx, lq, bs, plan, c.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range queue {
		if x.LeadID == held || x.LeadID == stale {
			t.Errorf("the batch queue offers held lead %d", x.LeadID)
		}
	}
	manual, dropped, err := manualQueue(ctx, lq, bs, plan, c.ID, now, []int64{held})
	if err != nil || len(manual) != 1 || manual[0].LeadID != held {
		t.Errorf("Send now on the held lead: %v dropped %v, %v", manual, dropped, err)
	}

	messages := func(lead int64) int {
		t.Helper()
		return batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM outreach_message WHERE lead_id = %d AND state <> 'cancelled'`, lead))
	}
	out := mustRun(t, "leads", "batch", "new", "--campaign", "cafes-loyalty")
	if n := messages(held); n != 0 {
		t.Errorf("the lead you marked Out got %d messages:\n%s", n, out)
	}
	if n := messages(stale); n == 0 {
		t.Errorf("the stale just chose was not released by the batch's sync:\n%s", out)
	}
	if got := intentOf(t, stale); got.Class != intent.Cold || got.Held {
		t.Errorf("after the build the stale lead reads %+v, want cold and not held", got)
	}
}
