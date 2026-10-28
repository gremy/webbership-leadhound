//go:build integration

package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// secondCampaign adds campaign "cafes" to the operator's account and returns its id.
func secondCampaign(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`INSERT INTO campaign (account_id, slug, name) VALUES (1, 'cafes', 'Cafes') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// leadInCampaign writes a lead of campaign campaignID for the org of lead leadID, on the same
// probe and route.
func leadInCampaign(t *testing.T, lq leadstore.Queries, leadID, campaignID int64) int64 {
	t.Helper()
	ctx := context.Background()
	l, err := lq.LeadByID(ctx, leadID)
	if err != nil {
		t.Fatal(err)
	}
	id, created, err := lq.UpsertLead(ctx, leadstore.LeadRow{OrgID: l.OrgID, Kind: l.Kind, ProbeID: l.ProbeID, Channel: l.Channel,
		RouteID: l.RouteID, CampaignID: campaignID})
	if err != nil || !created {
		t.Fatalf("lead in campaign %d: created %v, %v", campaignID, created, err)
	}
	return id
}

// TestCampaignCrossSkip: an org that replied in campaign 1 is never drafted by campaign 2's
// batch, and the batch output names it with why; campaign 2's other lead is drafted, in a
// batch of the operator's account.
func TestCampaignCrossSkip(t *testing.T) {
	w := newSendWorld(t, false, 20)
	drawFromAll(t, w.dir)
	ctx := context.Background()
	c2 := secondCampaign(t)
	replied := w.lead(t, "replied.example")
	if err := w.lq.SetLeadState(ctx, replied, leadstore.LeadReplied, "test", leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	leadInCampaign(t, w.lq, replied, c2)
	fresh := w.lead(t, "fresh.example")
	leadInCampaign(t, w.lq, fresh, c2)
	ownerExecCmd(t, `DELETE FROM lead_event WHERE lead_id = $1`, fresh)
	ownerExecCmd(t, `DELETE FROM lead WHERE id = $1`, fresh) // fresh.example is campaign 2's alone

	out := w.mustRun(t, "leads", "batch", "new", "--campaign", "cafes")
	wantLines(t, "batch new", out,
		"skipped replied.example (unsupported, email): replied in campaign drupal-eol",
		"Batch 1 (shadow): 1 email, 0 contact forms, 0 LinkedIn notes.")
	if n := w.count(t, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id
		WHERE l.campaign_id = $1 AND l.org_id = (SELECT org_id FROM lead WHERE id = $2)`, c2, replied); n != 0 {
		t.Errorf("the replied org has %d drafts in campaign 2", n)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_batch WHERE account_id = 1`); n != 1 {
		t.Errorf("%d batches of account 1, want 1", n)
	}

	// The default campaign is drupal-eol: its batch waits behind batch 1, so it says so.
	ownerExecCmd(t, `UPDATE outreach_batch SET discarded_at = now()`)
	out = w.mustRun(t, "leads", "batch", "new")
	if strings.Contains(out, "skipped") || strings.Contains(out, "fresh.example") {
		t.Errorf("campaign 1's batch reached campaign 2:\n%s", out)
	}

	// The Leads list filters by campaign and names each lead's; an unknown campaign lists
	// no lead, with a note, never an error.
	b := testBackend(t)
	b.cfg.DataDir = w.dir
	page, err := b.LeadList(ctx, console.LeadListQuery{Campaign: "cafes", Status: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Filter.Campaign != "cafes" || len(page.Rows) != 2 || page.Rows[0].Campaign != "Cafes" ||
		!slices.ContainsFunc(page.Options.Campaigns, func(o console.FilterOption) bool { return o.Value == "cafes" && o.Label == "Cafes" }) {
		t.Errorf("campaign 2's list = %d rows %+v, options %+v", len(page.Rows), page.Rows, page.Options.Campaigns)
	}
	page, err = b.LeadList(ctx, console.LeadListQuery{Campaign: "nope", Status: "all"})
	if err != nil || len(page.Rows) != 0 || page.Total != 0 || len(page.Notes) != 1 || !strings.Contains(page.Notes[0], "No campaign is called") {
		t.Errorf("unknown campaign: %d rows, notes %q, %v", len(page.Rows), page.Notes, err)
	}

	// An unknown or retired campaign is a usage error, on every command that takes one.
	ownerExecCmd(t, `UPDATE campaign SET retired_at = now() WHERE id = $1`, c2)
	for _, args := range [][]string{
		{"leads", "batch", "new", "--campaign", "nope"}, {"leads", "batch", "new", "--campaign", "cafes"},
		{"leads", "build", "--campaign", "nope"}, {"leads", "rate", "--campaign", "cafes", "--decider", "fake"},
	} {
		if code, msg := w.run(t, args...); code != 2 || !strings.Contains(msg, "campaign") {
			t.Errorf("%q exited %d: %s", args, code, msg)
		}
	}
}

// TestCampaignOptOutBlocksBatch: an opt-out recorded for campaign 1's lead blocks campaign
// 2's lead on the same address: batch new for campaign 2 drafts it never.
func TestCampaignOptOutBlocksBatch(t *testing.T) {
	w := newSendWorld(t, false, 20)
	drawFromAll(t, w.dir)
	ctx := context.Background()
	c2 := secondCampaign(t)
	optout := w.lead(t, "optout.example")
	if _, err := w.lq.Suppress(ctx, leadstore.DefaultAccountID, leadstore.SuppressAddress, "info@optout.example", leadstore.ReasonOptOut, "test"); err != nil {
		t.Fatal(err)
	}
	if s := w.leadState(t, "optout.example"); s != leadstore.LeadSuppressed {
		t.Fatalf("campaign 1's lead %s, want suppressed", s)
	}
	blocked := leadInCampaign(t, w.lq, optout, c2)
	other := w.lead(t, "other.example")
	leadInCampaign(t, w.lq, other, c2)

	out := w.mustRun(t, "leads", "batch", "new", "--campaign", "cafes")
	wantLines(t, "batch new", out, "Batch 1 (shadow): 1 email")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE lead_id = $1`, blocked); n != 0 {
		t.Errorf("campaign 2's lead on the opted-out address has %d drafts", n)
	}
}

// TestCampaignOptOutBlocksSend: an opt-out recorded during a send for campaign 1's address
// stops campaign 2's approved message to it: send refuses it.
func TestCampaignOptOutBlocksSend(t *testing.T) {
	w := newSendWorld(t, true, 20)
	ctx := context.Background()
	c2 := secondCampaign(t)
	ok := w.lead(t, "ok.example")
	c1 := w.lead(t, "optout.example")
	c2lead := leadInCampaign(t, w.lq, c1, c2)
	w.batch(t, false, map[int64]string{ok: "ok.example", c2lead: "optout.example"})
	if n := w.count(t, `SELECT count(*) FROM outreach_batch WHERE account_id = 1`); n != 1 {
		t.Fatalf("%d batches of account 1", n)
	}
	w.smtp.onSend = func(n int, _ string) {
		if n == 0 {
			if _, err := leadstoretest.Q(t).Suppress(ctx, leadstore.DefaultAccountID, leadstore.SuppressAddress, "info@optout.example",
				leadstore.ReasonOptOut, "test"); err != nil {
				t.Error(err)
			}
		}
	}
	out := w.mustRun(t, "leads", "send")
	if got := w.smtp.recipients(); len(got) != 1 || got[0] != "info@ok.example" {
		t.Errorf("SMTP got %v, want ok.example only:\n%s", got, out)
	}
	if s := w.str(t, `SELECT state FROM outreach_message WHERE lead_id = $1 AND step = 0`, c2lead); s == leadstore.MsgSent || s == "approved" {
		t.Errorf("campaign 2's message to the opted-out address is %s", s)
	}
}

// TestCampaignColdStart: a fresh database builds campaign 1's leads; build for another
// campaign waits for CP7.
func TestCampaignColdStart(t *testing.T) {
	dir := leadsDB(t)
	drawFromAll(t, dir)
	batchFixture(t)
	if n := batchCount(t, `SELECT count(*) FROM lead`); n != 0 {
		t.Fatalf("%d leads before build", n)
	}
	mustRun(t, "leads", "build")
	if n := batchCount(t, `SELECT count(*) FROM lead WHERE campaign_id <> 1`); n != 0 {
		t.Errorf("%d leads outside campaign 1", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM lead WHERE campaign_id = 1`); n != 6 {
		t.Errorf("%d leads of campaign 1, want 6", n)
	}
	secondCampaign(t)
	code, _, errOut := leadsRun(t, "leads", "build", "--campaign", "cafes")
	if code != 2 || !strings.Contains(errOut, "per-campaign build comes in CP7") {
		t.Errorf("build --campaign cafes exited %d: %s", code, errOut)
	}
	if n := batchCount(t, `SELECT count(*) FROM lead WHERE campaign_id <> 1`); n != 0 {
		t.Errorf("a refused build wrote %d leads", n)
	}
}

// TestCampaignsRegister: register writes a version per campaign only when its spec changed;
// list and show read them back; check writes nothing.
func TestCampaignsRegister(t *testing.T) {
	leadsDB(t)
	campaignsDataDir(t, map[string]string{"agencies": specYAML(t, "agencies")})
	t.Setenv("LEADHOUND_DATABASE_URL", storetest.AppDSN())

	mustRun(t, "leads", "campaigns", "check")
	if n := batchCount(t, `SELECT count(*) FROM campaign_version`); n != 0 {
		t.Fatalf("check wrote %d versions", n)
	}
	out := mustRun(t, "leads", "campaigns", "list")
	wantLines(t, "list before", out, "drupal-eol · Drupal end-of-life · no version registered")

	out = mustRun(t, "leads", "campaigns", "register")
	wantLines(t, "register", out, "agencies: new version (", "drupal-eol: new version (")
	out = mustRun(t, "leads", "campaigns", "register")
	wantLines(t, "register again", out, "agencies: unchanged (", "drupal-eol: unchanged (")
	if n := batchCount(t, `SELECT count(*) FROM campaign_version WHERE source = 'file' AND actor = 'operator'`); n != 2 {
		t.Errorf("%d versions, want 2", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM campaign WHERE account_id = 1`); n != 2 {
		t.Errorf("%d campaigns, want 2", n)
	}

	out = mustRun(t, "leads", "campaigns", "list")
	wantLines(t, "list", out, "agencies · Drupal end-of-life · version ", "drupal-eol · Drupal end-of-life · version ")
	out = mustRun(t, "leads", "campaigns", "show", campaign.LegacySlug)
	wantLines(t, "show", out, `"slug": "drupal-eol"`, `"writer": "template"`)
	if code, _, errOut := leadsRun(t, "leads", "campaigns", "show", "nope"); code != 2 || !strings.Contains(errOut, "no campaign is called") {
		t.Errorf("show nope exited %d: %s", code, errOut)
	}

	// A changed spec is a new version; the campaign's name stays the one it was made with.
	campaignsDataDir(t, map[string]string{"agencies": strings.Replace(specYAML(t, "agencies"), "after_business_days: 5", "after_business_days: 6", 1)})
	out = mustRun(t, "leads", "campaigns", "register")
	wantLines(t, "register changed", out, "agencies: new version (", "drupal-eol: unchanged (")

	// The console's Campaigns tab reads them.
	b := testBackend(t)
	b.cfg.DataDir = os.Getenv("LEADHOUND_DATA_DIR")
	views, err := b.Campaigns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views[0].Slug != "drupal-eol" || views[0].SpecSource != "file" || len(views[0].Steps) != 5 ||
		views[1].Slug != "agencies" || len(views[1].Steps) != 5 || views[1].Steps[4].AfterBusinessDays != 6 {
		t.Errorf("console views = %+v", views)
	}
}
