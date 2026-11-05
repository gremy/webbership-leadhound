//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The roastery hold across every send road (the plan's invariant sweep): a café lead whose
// place type is roastery, while roasteries are off in café outreach, is never sent, drafted
// or offered; an operator correction back to coffee shop, or the switch turned on, brings it
// back on every road, except messages the send already cancelled. CandidateLeads, NextUp and
// CallQueue are swept in internal/leadstore (place_hold_integration_test.go).
//
// The send fixtures' leads are Drupal leads, and a Drupal lead is never held. holdAsRoastery
// therefore makes a fixture lead a café lead first, then tags it: what every road reads is
// the lead's kind and place type at the moment it acts, so a tag made after drafting is
// exactly the late tag the hold must catch.

// heldWords is the sentence every road shows for a held lead.
var heldWords = manualStopWords(roasteryHeldWhy)

// holdAsRoastery makes lead a café lead (no digital loyalty) and tags it a roastery on the
// operator's word, with roasteries off (no switch row is off).
func holdAsRoastery(t *testing.T, lead int64) {
	t.Helper()
	ownerExec(t, `UPDATE lead SET kind = $2 WHERE id = $1`, lead, cafe.KindNoDigitalLoyalty)
	tagRoastery(t, lead)
}

// tagRoastery tags café lead a roastery on the operator's word.
func tagRoastery(t *testing.T, lead int64) {
	t.Helper()
	if err := leadstoretest.Q(t).SetPlaceType(context.Background(), lead, leadstore.PlaceRoastery, leadstore.PlaceFromOperator,
		"", nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
}

// markCoffeeShop is the operator's correction of lead back to a coffee shop.
func markCoffeeShop(t *testing.T, lead int64) {
	t.Helper()
	if err := leadstoretest.Q(t).SetPlaceType(context.Background(), lead, leadstore.PlaceCafe, leadstore.PlaceFromOperator,
		"", nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
}

// roasteriesOn turns roasteries on (or off) in café outreach for the default account.
func roasteriesOn(t *testing.T, on bool) {
	t.Helper()
	if _, err := leadstoretest.Q(t).SetRoasteryOutreach(context.Background(), leadstore.DefaultAccountID, on, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
}

// TestRoasteryHoldSendOne: the send run. A lead tagged after batching, with nothing sent, has
// every message cancelled with the hold's reason and goes back to new, and nothing reaches
// SMTP; a contacted lead tagged later keeps its state and its unsent follow-ups are cancelled
// the same way; a lead the operator corrected back to coffee shop is sent. With roasteries
// switched on a tagged lead is sent, and the messages already cancelled stay cancelled.
// Removing the check in sendOne sends to held-new.example and fails this test.
func TestRoasteryHoldSendOne(t *testing.T) {
	w := newSendWorld(t, true, 20)
	ctx := context.Background()
	ids := map[string]int64{}
	leads := map[int64]string{}
	for _, d := range []string{"held-new.example", "held-later.example", "plain-cafe.example", "corrected.example"} {
		id := w.lead(t, d)
		ownerExec(t, `UPDATE lead SET kind = $2 WHERE id = $1`, id, cafe.KindNoDigitalLoyalty)
		ids[d], leads[id] = id, d
	}
	w.batch(t, false, leads)
	// Tagged after batching: held-new by the operator, corrected by its name and then back to
	// coffee shop by the operator.
	holdAsRoastery(t, ids["held-new.example"])
	if err := w.lq.SetPlaceType(ctx, ids["corrected.example"], leadstore.PlaceRoastery, leadstore.PlaceFromName, "roastery", nil, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	markCoffeeShop(t, ids["corrected.example"])

	out := w.mustRun(t, "leads", "send")
	got := w.smtp.recipients()
	slices.Sort(got)
	if want := []string{"info@corrected.example", "info@held-later.example", "info@plain-cafe.example"}; !slices.Equal(got, want) {
		t.Fatalf("SMTP got %v, want %v:\n%s", got, want, out)
	}
	wantLines(t, "held", out, "Not sent: message", "held-new.example, "+leadstore.SkipRoasteryHeld,
		"Switch roasteries on in Controls, or mark it a coffee shop on its row. It is back to new, kept for later.")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE lead_id = $1 AND (state <> 'cancelled' OR cancel_reason <> $2)`,
		ids["held-new.example"], leadstore.SkipRoasteryHeld); n != 0 {
		t.Errorf("%d of held-new's messages not cancelled as held", n)
	}
	if s := w.leadState(t, "held-new.example"); s != leadstore.LeadNew {
		t.Errorf("held-new is %s, want new", s)
	}
	if r := w.str(t, `SELECT reason FROM lead_event WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, ids["held-new.example"]); r != leadstore.SkipRoasteryHeld {
		t.Errorf("held-new's last event %q", r)
	}

	// held-later was contacted; tagged now, its follow-ups never go and it stays contacted.
	holdAsRoastery(t, ids["held-later.example"])
	w.clock.set(time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	out = w.mustRun(t, "leads", "send")
	for _, r := range w.smtp.recipients()[3:] {
		if r == "info@held-later.example" || r == "info@held-new.example" {
			t.Fatalf("a held lead's follow-up went to %s:\n%s", r, out)
		}
	}
	if !slices.Contains(w.smtp.recipients()[3:], "info@plain-cafe.example") {
		t.Errorf("the plain café's follow-up did not go: %v\n%s", w.smtp.recipients(), out)
	}
	if s := w.leadState(t, "held-later.example"); s != leadstore.LeadContacted {
		t.Errorf("held-later is %s, want contacted", s)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE lead_id = $1 AND step > 0 AND (state <> 'cancelled' OR cancel_reason <> $2)`,
		ids["held-later.example"], leadstore.SkipRoasteryHeld); n != 0 {
		t.Errorf("%d of held-later's follow-ups not cancelled as held", n)
	}
	if s := w.msgState(t, "held-later.example", 0); s != leadstore.MsgSent {
		t.Errorf("held-later's first email %s, want it left sent", s)
	}

	// Switched on: a roastery batched now is sent; held-new's old messages stay cancelled.
	roasteriesOn(t, true)
	on := w.lead(t, "switched-on.example")
	holdAsRoastery(t, on)
	w.batch(t, false, map[int64]string{on: "switched-on.example"})
	w.clock.set(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	before := w.smtp.count()
	out = w.mustRun(t, "leads", "send")
	if got := w.smtp.recipients()[before:]; !slices.Contains(got, "info@switched-on.example") {
		t.Errorf("with roasteries on the roastery was not sent: %v\n%s", got, out)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE lead_id = $1 AND state <> 'cancelled'`, ids["held-new.example"]); n != 0 {
		t.Errorf("switching on revived %d cancelled messages", n)
	}
	w.sweep(t)
}

// TestRoasteryHoldSendNow: Send now and the lead page. Tagged after its preview, the plan
// stops the lead (TestSendNowPlanHardStops holds the confirm) and the lead page's Send now
// names the hold; a correction back to coffee shop, or roasteries switched on, lets the same
// plan send again. A held new lead chosen alone is refused with the hold's words and nothing
// is drafted, and Draft it refuses it the same way until it is corrected or switched on.
// (The fixture's batch was drafted from Drupal templates, so a fresh preview of the lead as
// a café would find no café template; the restore is proven on the drafted plan instead, and
// CandidateLeads' restore in TestRoasteryHoldBatchNew and internal/leadstore.)
func TestRoasteryHoldSendNow(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	lead := ids[0]

	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: []int64{lead}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanSend {
		t.Fatalf("the untagged lead cannot be sent: %+v", p)
	}
	plan := func(when string, held bool) {
		t.Helper()
		p, err := b.SendNowPlan(ctx, p.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		l := planLead(p, lead)
		if held && (l.Stop != heldWords || l.OK || p.CanSend) || !held && (l.Stop != "" || !p.CanSend) {
			t.Errorf("%s: stop %q ok %v can send %v", when, l.Stop, l.OK, p.CanSend)
		}
		page, err := b.LeadDetail(ctx, lead)
		if err != nil {
			t.Fatal(err)
		}
		if held != (page.SendNowBlock == heldWords) {
			t.Errorf("%s: Send now on the lead page: %q", when, page.SendNowBlock)
		}
	}
	holdAsRoastery(t, lead)
	plan("tagged after preview", true)
	markCoffeeShop(t, lead)
	plan("corrected", false)
	tagRoastery(t, lead)
	plan("tagged again", true)
	roasteriesOn(t, true)
	plan("switched on", false)
	roasteriesOn(t, false)
	plan("switched off", true)

	// Discarded, the lead is new and held: chosen alone it is refused, and nothing is drafted.
	if _, err := b.lq().DiscardBatch(ctx, p.BatchID); err != nil {
		t.Fatal(err)
	}
	before := batchCount(t, `SELECT count(*)::int FROM outreach_batch`)
	_, err = b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: []int64{lead}})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), heldWords) {
		t.Errorf("a held lead chosen for Send now: %v", err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch`); n != before {
		t.Errorf("a held lead's preview made %d batches", n-before)
	}
	if words, err := b.draftItBlock(ctx, mustLead(t, b, lead), nil); err != nil || words != strings.TrimSuffix(heldWords, ".") {
		t.Errorf("Draft it on the lead page: %q, %v", words, err)
	}
	_, err = b.RequestDraft(ctx, lead, "")
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), heldWords) {
		t.Errorf("Draft it: %v", err)
	}
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM draft_request WHERE lead_id = %d`, lead)); n != 0 {
		t.Errorf("%d draft requests for a held lead", n)
	}
	draftOpen := func(when string) {
		t.Helper()
		if words, err := b.draftItBlock(ctx, mustLead(t, b, lead), nil); err != nil || strings.Contains(words, "roastery") {
			t.Errorf("%s: Draft it: %q, %v", when, words, err)
		}
		if page, err := b.LeadDetail(ctx, lead); err != nil || page.SendNowBlock != "" {
			t.Errorf("%s: Send now on the lead page: %q, %v", when, page.SendNowBlock, err)
		}
	}
	markCoffeeShop(t, lead)
	draftOpen("corrected")
	tagRoastery(t, lead)
	roasteriesOn(t, true)
	draftOpen("switched on")
	if n := approvedCount(t, lead); n != 0 {
		t.Errorf("%d messages approved without a confirm", n)
	}
}

// TestRoasteryHoldApprove: a lead tagged after its batch was drafted is named on the batch
// page before approval, and Approve and send now stops the whole batch with the hold's words;
// once corrected, the page and the plan no longer name it.
func TestRoasteryHoldApprove(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	goLive(t)
	t.Setenv("LEADHOUND_LEADS_TIMERS", "on")
	setCaps(t, dir, "first-send", "[{from_day: 0, per_day: 20}]")
	mustRun(t, "leads", "batch", "new")
	var batchID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT max(id) FROM outreach_batch`).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	b := leadsBackend(t)
	before, err := b.LeadBatch(ctx, batchID)
	if err != nil || len(before.Items) == 0 {
		t.Fatalf("batch %d: %+v, %v", batchID, before, err)
	}
	var lead int64
	for _, it := range before.Items {
		if it.Held != "" {
			t.Errorf("item %d held before any tag: %q", it.N, it.Held)
		}
		if lead == 0 && it.Channel == leadstore.ChannelEmail {
			lead = it.LeadID
		}
	}
	if lead == 0 {
		t.Fatal("no email lead in the batch")
	}
	holdAsRoastery(t, lead)

	page, err := b.LeadBatch(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if want := map[bool]string{true: heldWords}[it.LeadID == lead]; it.Held != want {
			t.Errorf("item %d (lead %d): held %q, want %q", it.N, it.LeadID, it.Held, want)
		}
	}
	if page.HeldCount() != 1 || page.HashPrefix == "" {
		t.Errorf("held %d, hash %q: the batch stays approvable with the hold named", page.HeldCount(), page.HashPrefix)
	}
	plan, err := b.SendNowPlan(ctx, batchID)
	if err != nil {
		t.Fatal(err)
	}
	if l := planLead(plan, lead); l.Stop != heldWords || plan.CanSend {
		t.Errorf("Approve and send now: stop %q, can send %v", l.Stop, plan.CanSend)
	}
	if !slices.ContainsFunc(plan.Notes, func(n string) bool { return strings.Contains(n, heldWords) }) {
		t.Errorf("no note names the held lead: %q", plan.Notes)
	}

	markCoffeeShop(t, lead)
	if page, err = b.LeadBatch(ctx, batchID); err != nil {
		t.Fatal(err)
	}
	if page.HeldCount() != 0 {
		t.Errorf("corrected, still %d held", page.HeldCount())
	}
	if plan, err = b.SendNowPlan(ctx, batchID); err != nil {
		t.Fatal(err)
	}
	if l := planLead(plan, lead); strings.Contains(l.Stop, "roastery") {
		t.Errorf("corrected, the plan still stops it: %q", l.Stop)
	}
}

// TestRoasteryHoldBatchNew: the morning batch (leads batch new) drafts from CandidateLeads,
// so a café lead tagged a roastery gets no message while the other cafés do; corrected back
// to coffee shop, the next batch drafts it.
func TestRoasteryHoldBatchNew(t *testing.T) {
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
	held := leadIDOf(t, "harbour-lane-coffee.example")
	tagRoastery(t, held)

	messages := func(lead int64) int {
		t.Helper()
		return batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM outreach_message WHERE lead_id = %d AND state <> 'cancelled'`, lead))
	}
	out := mustRun(t, "leads", "batch", "new", "--campaign", "cafes-loyalty")
	if n := messages(held); n != 0 {
		t.Fatalf("the roastery got %d messages:\n%s", n, out)
	}
	others := 0
	for _, d := range domains[1:] {
		others += messages(leadIDOf(t, d))
	}
	if others == 0 {
		t.Fatalf("the batch drafted no café at all:\n%s", out)
	}
	var batchID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT max(id) FROM outreach_batch`).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "batch", "discard", fmt.Sprint(batchID))

	markCoffeeShop(t, held)
	out = mustRun(t, "leads", "batch", "new", "--campaign", "cafes-loyalty")
	if n := messages(held); n == 0 {
		t.Errorf("corrected back to coffee shop, it got no message:\n%s", out)
	}
}

// mustLead is lead id as the backend reads it.
func mustLead(t *testing.T, b *backend, id int64) leadstore.Lead {
	t.Helper()
	l, err := b.lq().LeadByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
