//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The Instagram channel through cmd/leadhound (CP4d): the build picks it for a café lead only,
// Draft it and the morning batch write one DM, Mark sent moves the lead to contacted, the roastery
// hold refuses it, and erasure leaves nothing a later import could re-attach. Every handle is
// invented (.example).

// igCafeWorld is cafeScene with a mailbox, the clock on a weekday and the batch drawing from
// every lead, so Draft it and batch new can run on the café campaign.
func igCafeWorld(t *testing.T) int64 {
	t.Helper()
	cid := cafeScene(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })
	mb := seedMailbox(t, leadstoretest.Q(t), testUser, withDNSPass())
	if _, err := leadstoretest.Q(t).SetCampaignMailbox(context.Background(), cid, mb.ID, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	return cid
}

// igRouteOn gives lead's org handle as an instagram route, as a website's link would.
func igRouteOn(t *testing.T, lead int64, handle string) {
	t.Helper()
	if _, err := leadstoretest.Q(t).InsertRoute(context.Background(), leadstore.RouteRow{OrgID: igOrgOf(t, lead),
		Kind: leadstore.RouteInstagram, Value: handle, DerivedBy: leadstore.DerivedByInstagram}); err != nil {
		t.Fatal(err)
	}
}

func leadChannel(t *testing.T, lead int64) string {
	t.Helper()
	l, err := leadstoretest.Q(t).LeadByID(context.Background(), lead)
	if err != nil {
		t.Fatal(err)
	}
	return l.Channel
}

// igLead is Harbour Lane, a café lead with no route but its Instagram account, rebuilt onto the
// instagram channel; it returns the lead and the account's URL.
func igLead(t *testing.T) (int64, string) {
	t.Helper()
	lead := leadIDOf(t, "harbour-lane-coffee.example")
	igRouteOn(t, lead, "harbour_lane.example")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	if got := leadChannel(t, lead); got != leadstore.RouteInstagram {
		t.Fatalf("Harbour Lane is on %q after the build, want instagram", got)
	}
	return lead, enrich.InstagramURL("harbour_lane.example")
}

// TestInstagramChannelPick: the build puts a café lead whose only route is its Instagram account
// on the instagram channel and keeps a café with an email address on email, and the summary
// names it; a Drupal lead whose only route is an Instagram account stays on none, at the build
// and when the operator adds the account by hand, while the same hand-added account moves a café
// lead to instagram (the re-pick reads each lead's kind).
func TestInstagramChannelPick(t *testing.T) {
	ctx := context.Background()
	igCafeWorld(t)
	harbour, ro := leadIDOf(t, "harbour-lane-coffee.example"), leadIDOf(t, "cafeneaua-exemplu.example")
	igRouteOn(t, harbour, "harbour_lane.example")
	igRouteOn(t, ro, "cafeneaua.exemplu")
	out := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	wantLines(t, "café build", out, "  cafe_no_digital_loyalty: email 1 · instagram 1 · none 1")
	if got := leadChannel(t, harbour); got != "instagram" {
		t.Errorf("a café lead with only its Instagram account is on %q, want instagram", got)
	}
	if got := leadChannel(t, ro); got != "email" {
		t.Errorf("a café lead with an email address and an Instagram account is on %q, want email", got)
	}

	// The operator's own Instagram contact on a café with none re-picks it onto instagram.
	b := leadsBackend(t)
	bean := leadIDOf(t, "bean-house.example")
	if _, err := b.AddContact(ctx, console.NewContact{LeadID: bean, Kind: console.ContactInstagram, Value: "@bean_house.example"}); err != nil {
		t.Fatal(err)
	}
	if d := detailOf(t, b, bean); d.Channel != "instagram" || d.Reach != console.ReachManual || !d.HasInstagram {
		t.Errorf("café with the operator's Instagram: %s on %s (has instagram %v)", d.Reach, d.Channel, d.HasInstagram)
	}
}

// TestInstagramChannelNotForDrupal: a Drupal lead whose only route left is an Instagram account
// (the 00031 backfill gives many such leads one) stays on none at the build and at the operator's
// re-pick, and Draft it refuses an Instagram DM for it.
func TestInstagramChannelNotForDrupal(t *testing.T) {
	ctx := context.Background()
	b, _ := outreachWorld(t)
	lead := deadEndLead(t, b)
	if _, err := b.AddContact(ctx, console.NewContact{LeadID: lead, Kind: console.ContactInstagram, Value: "litware.example"}); err != nil {
		t.Fatal(err)
	}
	if d := detailOf(t, b, lead); d.Channel != "none" || !d.HasInstagram {
		t.Errorf("Drupal lead with only an Instagram account: on %q (has instagram %v), want none", d.Channel, d.HasInstagram)
	}
	mustRun(t, "leads", "build")
	if got := leadChannel(t, lead); got != "none" {
		t.Errorf("the build put a Drupal lead with only an Instagram account on %q, want none", got)
	}
	code, _, errOut := leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--channel", "instagram", "--decider", "fake")
	wantRefused(t, "an Instagram DM for a Drupal lead", code, errOut, "An Instagram DM is for a café lead only.")
}

// TestDraftItColdStartInstagram: the first Draft it on an Instagram lead is a live, manual
// draft_it batch of one DM at step 0, to the account, written by Claude (the fake), its
// generation on the instagram channel, within the DM limit and holding no link; batch show names
// it as an Instagram DM. The lead is batched, and on the Manual tab until Mark sent moves it to
// contacted, where the timeline says it was marked sent.
func TestDraftItColdStartInstagram(t *testing.T) {
	ctx := context.Background()
	igCafeWorld(t)
	lead, url := igLead(t)
	b := leadsBackend(t)
	if d := detailOf(t, b, lead); d.Reach != console.ReachManual || d.DMMax != 500 {
		t.Fatalf("before Draft it: reach %s, DM max %d", d.Reach, d.DMMax)
	}
	batch := draftIt(t, lead)
	bt, err := leadstoretest.Q(t).BatchByID(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if bt.Origin != leadstore.BatchOriginDraftIt || !bt.Manual || bt.Shadow || bt.ApprovedAt != nil {
		t.Fatalf("batch %+v", bt)
	}
	rows := batchRows(t, batch)
	if len(rows) != 1 || rows[0].Step != 0 || rows[0].Channel != "instagram" || rows[0].To != url {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].Writer != campaign.WriterClaude || rows[0].GenerationRow == 0 {
		t.Fatalf("row %+v, want Claude's", rows[0])
	}
	if n := len([]rune(strings.TrimSpace(rows[0].Body))); n == 0 || n > 500 || dmHoldsLink(rows[0].Body) {
		t.Fatalf("the DM (%d characters):\n%s", n, rows[0].Body)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM draft_generation WHERE channel = 'instagram'`); n != 1 {
		t.Fatalf("%d instagram generations", n)
	}
	wantLines(t, "show", mustRun(t, "leads", "batch", "show", fmt.Sprint(batch)), "Instagram DM · message", "To: "+url)
	if got := draftLeadState(t, lead); got != leadstore.LeadBatched {
		t.Fatalf("lead %s", got)
	}
	d := detailOf(t, b, lead)
	if d.Reach != console.ReachManual || len(d.Drafts) != 1 || !d.Drafts[0].CanMarkSent {
		t.Fatalf("after Draft it: reach %s, drafts %+v", d.Reach, d.Drafts)
	}
	if _, err := b.MarkSent(ctx, console.MarkSentRequest{LeadID: lead, MessageID: rows[0].ID, Hash: d.Drafts[0].BatchHash}); err != nil {
		t.Fatal(err)
	}
	if got := draftLeadState(t, lead); got != leadstore.LeadContacted {
		t.Fatalf("Mark sent left the lead %s, want contacted", got)
	}
	if d := detailOf(t, b, lead); d.Reach != console.ReachContacted {
		t.Fatalf("after Mark sent: reach %s", d.Reach)
	}
	if lines := timelineLines(t, b, lead); !strings.Contains(lines, "Marked sent · Instagram, step 1") {
		t.Fatalf("timeline:\n%s", lines)
	}
}

// TestBatchNewInstagramLead: the morning batch drafts an Instagram lead from the campaign's
// instagram.txt when Claude is off (one row at step 0, to the account, saying why), counts it as
// an Instagram DM, and a batch holding none reads as before.
func TestBatchNewInstagramLead(t *testing.T) {
	igCafeWorld(t)
	lead, url := igLead(t)
	out := mustRun(t, "leads", "batch", "new", "--campaign", "cafes-loyalty")
	if strings.Contains(out, "has no drafts") {
		t.Fatalf("batch new:\n%s", out)
	}
	wantLines(t, "batch new", out, ", 1 Instagram DM")
	var mine []draftMsg
	for _, r := range batchRows(t, latestBatch(t, lead)) {
		if r.To == url {
			mine = append(mine, r)
		}
	}
	if len(mine) != 1 || mine[0].Step != 0 || mine[0].Channel != "instagram" || mine[0].Writer != campaign.WriterTemplate ||
		!strings.Contains(mine[0].Body, "Demo Loyalty") || dmHoldsLink(mine[0].Body) {
		t.Fatalf("instagram lead rows %+v", mine)
	}
	if got := (BatchCounts{Emails: 2, Forms: 1}).String(); got != "2 emails, 1 contact form, 0 LinkedIn notes" {
		t.Errorf("a batch without Instagram DMs reads %q", got)
	}
}

// TestDraftItInstagramRoasteryHeld: an Instagram lead tagged a roastery, with roasteries off in
// café outreach, is refused by Draft it like every other road, and drafted once it is corrected.
func TestDraftItInstagramRoasteryHeld(t *testing.T) {
	igCafeWorld(t)
	lead, _ := igLead(t)
	tagRoastery(t, lead)
	code, _, errOut := leadsRun(t, "leads", "drafts", "make", fmt.Sprint(lead), "--decider", "fake")
	wantRefused(t, "a held Instagram lead", code, errOut, heldWords)
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM outreach_message WHERE lead_id = %d`, lead)); n != 0 {
		t.Fatalf("a held lead got %d messages", n)
	}
	markCoffeeShop(t, lead)
	if rows := batchRows(t, draftIt(t, lead)); len(rows) != 1 || rows[0].Channel != "instagram" {
		t.Fatalf("corrected lead rows %+v", rows)
	}
}

// TestForgetThenReimportInstagram: erasing a café org removes its Instagram route and every
// Instagram row of its handle, and says so; importing the same capture again re-attaches nothing
// and writes no route (the org's domain is suppressed), and the lead stays off instagram.
func TestForgetThenReimportInstagram(t *testing.T) {
	s := newIGScene(t)
	igSceneFake(t)
	t.Setenv("LEADHOUND_MIGRATE_DATABASE_URL", storetest.MigrateDSN()) // forget runs as the owner
	lines := []string{igProfileLine(igA), igProfileLine(igB)}
	igImport(t, lines)
	org := igOrgOf(t, s.B)
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d AND kind = 'instagram'`, org)); n != 1 {
		t.Fatalf("the domain match wrote %d Instagram routes on Bean House, want 1", n)
	}
	if ig := igFor(t, s.B); ig == nil {
		t.Fatal("no Instagram card on Bean House after the import")
	}
	out := mustRun(t, "leads", "forget", "org", fmt.Sprint(org), "--reason", "asked to be forgotten")
	wantLines(t, "forget", out, "1 Instagram account deleted", "Instagram profile reading", "Instagram match")
	for _, q := range []string{
		fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d AND kind = 'instagram'`, org),
		`SELECT count(*) FROM ig_profile WHERE handle = 'bean_house.example'`,
		`SELECT count(*) FROM ig_match WHERE handle = 'bean_house.example'`,
		fmt.Sprintf(`SELECT count(*) FROM ig_match WHERE lead_id = %d`, s.B),
	} {
		if n := dbCount(t, q); n != 0 {
			t.Errorf("%s: %d after forget", q, n)
		}
	}

	igImport(t, lines)
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d AND kind = 'instagram'`, org)); n != 0 {
		t.Errorf("the re-import wrote %d Instagram routes on the erased org", n)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM ig_match WHERE lead_id = %d`, s.B)); n != 0 {
		t.Errorf("the re-import matched the erased org's lead %d times", n)
	}
	if ig := igFor(t, s.B); ig != nil {
		t.Errorf("the erased lead shows an Instagram card again: %+v", ig)
	}
	if got := leadChannel(t, s.B); got == "instagram" {
		t.Errorf("the erased lead is on instagram")
	}
	// The other café's handle match is untouched.
	if ig := igFor(t, s.A); ig == nil {
		t.Error("Harbour Lane lost its Instagram card")
	}
}
