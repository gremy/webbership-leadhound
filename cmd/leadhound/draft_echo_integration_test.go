//go:build integration

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The org's own words the echo test stores, each a sentinel no other fixture holds.
const (
	echoDescription = "Quillberry heritage trust keeps marmalade archives open"
	echoSocial      = "https://social.example/quillberry-marmalade-archive"
	echoPage        = "Zephyrwick volunteers catalogue the marmalade archives every Thursday"
)

// TestNoOrgTextReachesWriter is G6's reading (internal/outreach/writer.go, the plan's G6):
// the writer is never shown the org's own text, so the echo check has none to hold a draft
// to. A lead whose org has a stored description, a social profile and a page body gets a
// fact sheet, a user turn and a system prompt that carry none of them; and the one road for
// org text onto the sheet (SheetInput.OrgTexts) marks it echo_guard, context only. If
// sheetInputForLead ever adds org text, it must go that way: this test fails on any org text
// that reaches the writer unguarded.
func TestNoOrgTextReachesWriter(t *testing.T) {
	ctx := context.Background()
	leadsDB(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	batchFixture(t)
	lq := leadstoretest.Q(t)
	var orgID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT id FROM org WHERE domain = 'northwind.example'`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-enrich", "echo-fixture")
	if err != nil {
		t.Fatal(err)
	}
	about, err := core.InsertFetch(ctx, runID, "echo-fixture", fetch.Record{Kind: fetch.KindSite, URL: "https://northwind.example/about",
		Status: 200, Body: []byte("<html><body><p>" + echoPage + "</p><p>" + echoDescription + "</p></body></html>"), ContentType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []leadstore.FactRow{
		{OrgID: orgID, Kind: leadstore.FactDescription, Value: echoDescription, Normalized: echoDescription, FetchID: about, DerivedBy: "enrich@1"},
		{OrgID: orgID, Kind: leadstore.FactSocial, Value: echoSocial, Normalized: echoSocial, FetchID: about, DerivedBy: "enrich@1"},
	} {
		if _, err := lq.InsertFact(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, "leads", "build")
	leadID := leadIDOf(t, "northwind.example")

	cfg, err := leadconfig.Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := campaign.Load(filepath.Join(examplesDir, campaign.Dir, "drupal-eol", campaign.FileName))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := filePlan(examplesDir, spec)
	if err != nil {
		t.Fatal(err)
	}
	c, err := leadCandidate(ctx, lq, leadID)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := lq.LeadEvidence(ctx, []int64{leadID})
	if err != nil {
		t.Fatal(err)
	}
	// Every org word the store holds, as the echo check would read them.
	sentinels := []string{"Quillberry", "quillberry", "Zephyrwick", "marmalade"}
	if n := dbCount(t, `SELECT count(*) FROM org_fact WHERE value LIKE '%marmalade%'`); n != 2 {
		t.Fatalf("the fixture stores %d org facts with the org's words, want 2", n)
	}
	holds := func(s string) bool { return containsAny(s, sentinels) }
	for _, phone := range []bool{false, true} {
		in, err := draftCtx{cfg: cfg, plan: plan, campaignID: 1}.writeInput(ctx, lq, plan, c, ev[leadID], phone)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range in.Sheet.Facts {
			if holds(f.Value) && !f.EchoGuard {
				t.Errorf("phone %v: fact %s (%s) carries the org's own text unguarded: %q", phone, f.ID, f.Label, f.Value)
			}
		}
		for _, line := range strings.Split(outreach.UserTurn(in), "\n") {
			if holds(line) && !strings.Contains(line, `"context_only":true`) {
				t.Errorf("phone %v: the user turn shows the org's own text outside a context-only fact: %q", phone, line)
			}
		}
		if holds(outreach.SystemPrompt(in)) {
			t.Errorf("phone %v: the system prompt holds the org's own text", phone)
		}
	}

	// The road for org text, should a writer ever need it, is echo_guard.
	sheet := outreach.BuildFactSheet(outreach.SheetInput{Org: outreach.OrgFacts{Name: "Northwind", Domain: "northwind.example"},
		OrgTexts: []outreach.TextFact{{Label: "description", Value: echoDescription}}})
	guarded := false
	for _, f := range sheet.Facts {
		if f.Value == echoDescription {
			guarded = f.EchoGuard
		}
	}
	if !guarded {
		t.Error("SheetInput.OrgTexts does not reach the sheet as echo_guard")
	}
}

// containsAny reports whether s holds any of subs.
func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
