//go:build integration

package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// cafeListSlug is the café campaign of cafeListScene.
const cafeListSlug = "fictive-cafes"

// cafeListScene is the order scene with four of its leads moved into a café campaign of their
// own and made café leads: lucerne and margie in Exampleton, humongous in Otherby, and cpandl a
// roastery by the operator's word. lucerne's place has a category and an address and its
// finding a reason; margie has a description. The other four stay Drupal leads of
// drupal-eol. It returns the backend and the café campaign's id.
func cafeListScene(t *testing.T) (*backend, int64) {
	t.Helper()
	orderScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	var campaignID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`INSERT INTO campaign (account_id, slug, name) VALUES (1, $1, 'Fictive Cafes') RETURNING id`, cafeListSlug).Scan(&campaignID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ domain, town string }{
		{"lucerne.example", "Exampleton"}, {"margie.example", "Exampleton"}, {"humongous.example", "Otherby"}, {"cpandl.example", "Otherby"},
	} {
		id := setLeadKind(t, c.domain, cafe.KindNoDigitalLoyalty)
		ownerExec(t, `UPDATE lead SET campaign_id = $2 WHERE id = $1`, id, campaignID)
		addr := places.Address{Locality: c.town, Country: "GB"}
		if c.domain == "lucerne.example" {
			addr.Freeform, addr.Postcode = "1 Fictive Road", "AB1 2CD"
		}
		p := places.Place{SourceID: c.domain, Name: "Fictive " + c.domain, Category: "coffee_shop", Confidence: 0.9,
			Websites: []string{"https://" + c.domain + "/"}, Address: addr}
		if _, err := lq.ImportPlaces(ctx, leadstore.PlaceImport{CampaignID: campaignID, Release: "2026-10-21.0", FileName: "places.ndjson",
			Stats: places.Stats{Lines: 1, Kept: 1}}, []leadstore.PlaceRow{{Place: p}}); err != nil {
			t.Fatalf("place of %s: %v", c.domain, err)
		}
	}
	ownerExec(t, `UPDATE finding SET subject = subject || '{"reason": "A fictive reason"}'::jsonb
		WHERE id = (SELECT finding_id FROM lead WHERE id = $1)`, leadIDOf(t, "lucerne.example"))
	var orgID, fetchID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT l.org_id, (SELECT min(id) FROM http_fetch) FROM lead l WHERE l.id = $1`, leadIDOf(t, "margie.example")).Scan(&orgID, &fetchID); err != nil {
		t.Fatal(err)
	}
	if _, err := lq.InsertFact(ctx, leadstore.FactRow{OrgID: orgID, Kind: leadstore.FactDescription, Value: "A fictive  café\nby the river.",
		Normalized: "a fictive café by the river.", FetchID: fetchID, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
	if err := lq.SetPlaceType(ctx, leadIDOf(t, "cpandl.example"), leadstore.PlaceRoastery, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	return leadsBackend(t), campaignID
}

// listOf is the list under q, failing the test on an error.
func listOf(t *testing.T, b *backend, q console.LeadListQuery) console.LeadListPage {
	t.Helper()
	p, err := b.LeadList(context.Background(), q)
	if err != nil {
		t.Fatalf("list %+v: %v", q, err)
	}
	return p
}

// rowDomains is the domains of p's rows, sorted.
func rowDomains(p console.LeadListPage) []string {
	var out []string
	for _, r := range p.Rows {
		out = append(out, r.Domain)
	}
	slices.Sort(out)
	return out
}

// rowOf is the row of domain in p.
func rowOf(t *testing.T, p console.LeadListPage, domain string) console.LeadListRow {
	t.Helper()
	i := slices.IndexFunc(p.Rows, func(r console.LeadListRow) bool { return r.Domain == domain })
	if i < 0 {
		t.Fatalf("%s is not listed: %v", domain, rowDomains(p))
	}
	return p.Rows[i]
}

// TestLeadListCampaignTabs: the campaign tabs count each campaign's open leads, both place
// types, from their own read; a café campaign lists coffee shops by default, place_type=all
// lists both and roastery the roasteries; only a chosen campaign reads its progress, a café
// campaign its towns and chips; campaign=all is every campaign with none of those; and a
// Drupal lead has no chip.
func TestLeadListCampaignTabs(t *testing.T) {
	b, _ := cafeListScene(t)

	p := listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug})
	want := []console.CampaignTab{{Slug: "drupal-eol", Name: "Drupal end-of-life", Open: 4}, {Slug: cafeListSlug, Name: "Fictive Cafes", Open: 4, Cafe: true}}
	if !slices.Equal(p.CampaignTabs, want) {
		t.Errorf("tabs %+v, want %+v", p.CampaignTabs, want)
	}
	if got := rowDomains(p); !slices.Equal(got, []string{"humongous.example", "lucerne.example", "margie.example"}) || p.Filter.PlaceType != "cafe" {
		t.Errorf("café default: %v, place type %q", got, p.Filter.PlaceType)
	}
	if p.PlaceTypeCounts == nil || *p.PlaceTypeCounts != (console.PlaceTypeCounts{Cafe: 3, Roastery: 1}) {
		t.Errorf("chips %+v", p.PlaceTypeCounts)
	}
	if p.Progress == nil || *p.Progress != (console.CampaignProgress{Reviewed: 0, Total: 4}) {
		t.Errorf("progress %+v", p.Progress)
	}
	if !slices.Equal(p.Towns, []console.FilterOption{{Value: "Exampleton", Label: "Exampleton (2)"}, {Value: "Otherby", Label: "Otherby (2)"}}) {
		t.Errorf("towns %+v", p.Towns)
	}
	if r := rowOf(t, p, "lucerne.example"); r.PlaceType != "cafe" || r.PlaceTypeSource != "" || r.Town != "Exampleton" {
		t.Errorf("lucerne: %q from %q in %q", r.PlaceType, r.PlaceTypeSource, r.Town)
	}

	if got := rowDomains(listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug, PlaceType: "all"})); len(got) != 4 {
		t.Errorf("place_type=all lists %v", got)
	}
	p = listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug, PlaceType: "roastery"})
	if r := rowOf(t, p, "cpandl.example"); len(p.Rows) != 1 || r.PlaceType != "roastery" || r.PlaceTypeSource != leadstore.PlaceFromOperator {
		t.Errorf("roasteries: %v, cpandl %q from %q", rowDomains(p), r.PlaceType, r.PlaceTypeSource)
	}
	if p = listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug, PlaceType: "unclear"}); len(p.Notes) != 1 || len(p.Rows) != 3 {
		t.Errorf("an unknown place type: notes %q, %v", p.Notes, rowDomains(p))
	}

	p = listOf(t, b, console.LeadListQuery{Campaign: "drupal-eol"})
	if len(p.Rows) != 4 || p.Filter.PlaceType != "" || p.PlaceTypeCounts != nil || p.Towns != nil || p.Progress == nil {
		t.Errorf("Drupal campaign: %v, place type %q, chips %v, towns %v, progress %v", rowDomains(p), p.Filter.PlaceType, p.PlaceTypeCounts, p.Towns, p.Progress)
	}
	if r := rowOf(t, p, "proseware.example"); r.PlaceType != "" || r.PlaceTypeSource != "" || r.Town != "" {
		t.Errorf("a Drupal lead has a chip: %q from %q in %q", r.PlaceType, r.PlaceTypeSource, r.Town)
	}

	p = listOf(t, b, console.LeadListQuery{Campaign: "all"})
	if len(p.Rows) != 8 || p.Filter.Campaign != "all" || p.Progress != nil || p.Towns != nil || p.PlaceTypeCounts != nil || len(p.Notes) != 0 {
		t.Errorf("campaign=all: %v, echo %q, progress %v, towns %v, chips %v, notes %q", rowDomains(p), p.Filter.Campaign, p.Progress, p.Towns, p.PlaceTypeCounts, p.Notes)
	}
}

// TestLeadListTabsMatchTotal: each next-action tab's count is the total the list gives under
// the same filter, with the town and the place type on.
func TestLeadListTabsMatchTotal(t *testing.T) {
	b, _ := cafeListScene(t)
	for _, q := range []console.LeadListQuery{
		{Campaign: cafeListSlug, Town: "exampleton"},
		{Campaign: cafeListSlug, Town: "Otherby", PlaceType: "all"},
		{Campaign: cafeListSlug, PlaceType: "roastery"},
		{Campaign: cafeListSlug},
	} {
		p := listOf(t, b, q)
		sum := 0
		for _, bucket := range console.ReachTabs {
			q := q
			q.Reach = bucket
			if got := listOf(t, b, q).Total; got != p.ReachCounts.Of(bucket) {
				t.Errorf("%+v: the %s tab counts %d, the list totals %d", q, bucket, p.ReachCounts.Of(bucket), got)
			}
			sum += p.ReachCounts.Of(bucket)
		}
		if sum == 0 {
			t.Errorf("%+v: every tab is empty, so the test proves nothing", q)
		}
	}
}

// TestLeadListSummary: a row's summary is the line LeadLine builds from its facts, read from
// the store: lucerne's café finding rests on its read homepage and names a loyalty vendor,
// margie's homepage was read and showed none, and humongous has no café finding, so nothing
// is said of its loyalty. The short line leaves out the town; the description is About and
// Jev's pick of kind is Kind, in words. Jev's number and band are its relevance words; and a
// skipped row offers Undo only when the operator skipped it, the system's skip showing its
// reason instead, both counted by the progress line as the store counts them.
func TestLeadListSummary(t *testing.T) {
	b, _ := cafeListScene(t)
	ctx := context.Background()
	var vendor cafe.Vendor
	for _, v := range cafe.DefaultConfig().Vendors {
		if v.Kind == cafe.SigLoyalty && !v.Own && v.Name != "" {
			vendor = v
			break
		}
	}
	sig := cafe.Signal{Kind: cafe.SigLoyalty, Vendor: vendor.ID, Via: cafe.ViaScript, Match: "lucerne.example"}
	ownerExec(t, `UPDATE finding SET detector = $2, evidence = jsonb_build_array(jsonb_build_object('name', $3::text, 'value', $4::text))
		WHERE id = (SELECT finding_id FROM lead WHERE id = $1)`, leadIDOf(t, "lucerne.example"), cafe.DerivedBy, cafe.SigLoyalty, sig.String())
	ownerExec(t, `UPDATE finding SET detector = $2, evidence = '[]' WHERE id = (SELECT finding_id FROM lead WHERE id = $1)`,
		leadIDOf(t, "margie.example"), cafe.DerivedBy)
	if err := leadstoretest.Q(t).SetBusinessKind(ctx, leadIDOf(t, "margie.example"), "bakery_cafe", "business-kind@1", make([]byte, 32), 0.9); err != nil {
		t.Fatal(err)
	}
	p := listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug, PlaceType: "all"})
	if r := rowOf(t, p, "margie.example"); r.Summary != "Bakery café · Exampleton · no loyalty seen" || r.SummaryShort != "Bakery café · no loyalty seen" ||
		r.Details.About != "A fictive café by the river." || r.Details.Kind != "Bakery café" || r.BusinessKind != "bakery_cafe" || r.Tile != console.TileLetter(r.Name, r.Domain) || r.Tile == "#" {
		t.Errorf("margie: %q / %q, about %q, kind %q / %q, tile %q", r.Summary, r.SummaryShort, r.Details.About, r.Details.Kind, r.BusinessKind, r.Tile)
	}
	r := rowOf(t, p, "lucerne.example")
	if r.Summary != "Coffee shop · Exampleton · on "+vendor.Name || r.FindingReason != "A fictive reason" || r.Details.About != "" || r.Details.Kind != "" {
		t.Errorf("lucerne's summary %q, reason %q, about %q, kind %q", r.Summary, r.FindingReason, r.Details.About, r.Details.Kind)
	}
	if r.Details.Address != "1 Fictive Road, AB1 2CD" || !slices.ContainsFunc(r.Details.Routes, func(l console.RowLink) bool { return l.Label == "info@lucerne.example" && l.URL == "" }) {
		t.Errorf("lucerne's details %+v", r.Details)
	}
	if r.RelevanceWords != r.Relevance {
		t.Errorf("relevance words %q beside %q", r.RelevanceWords, r.Relevance)
	}
	if r := rowOf(t, p, "humongous.example"); r.Summary != "Coffee shop · Otherby" || r.SummaryShort != "Coffee shop" {
		t.Errorf("humongous's summary %q / %q", r.Summary, r.SummaryShort)
	}
	if r := rowOf(t, p, "cpandl.example"); r.Summary != "Roastery or online store · Otherby" {
		t.Errorf("cpandl's summary %q", r.Summary)
	}

	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadSkip, ID: leadIDOf(t, "lucerne.example")}); err != nil {
		t.Fatal(err)
	}
	humongous := leadIDOf(t, "humongous.example")
	ownerExec(t, `UPDATE lead SET state = 'skipped' WHERE id = $1`, humongous)
	ownerExec(t, `INSERT INTO lead_event (lead_id, state, reason, actor) VALUES ($1, 'skipped', 'a duplicate of a fictive place', 'system')`, humongous)
	p = listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug, PlaceType: "all", Status: "all"})
	if r := rowOf(t, p, "lucerne.example"); !r.CanUndo || r.SkipReason != "" {
		t.Errorf("the operator's skip: undo %v, reason %q", r.CanUndo, r.SkipReason)
	}
	if r := rowOf(t, p, "humongous.example"); r.CanUndo || r.SkipReason != "a duplicate of a fictive place" {
		t.Errorf("the system's skip: undo %v, reason %q", r.CanUndo, r.SkipReason)
	}
	if r := rowOf(t, p, "margie.example"); r.CanUndo || r.SkipReason != "" {
		t.Errorf("an open lead: undo %v, reason %q", r.CanUndo, r.SkipReason)
	}
	if p.Progress == nil || *p.Progress != (console.CampaignProgress{Reviewed: 1, Total: 3}) {
		t.Errorf("progress %+v, want 1 of 3", p.Progress)
	}
}

// TestLeadListRememberedCampaign: the cold-start twins of the remembered campaign on the
// backend. A remembered slug applies as a named one; a remembered slug that names no campaign,
// or a retired one, is dropped silently and the list shows every campaign, while the same
// slug named in the query keeps its note (unknown) or its list (retired); and the retired
// campaign keeps its tab only while it is chosen.
func TestLeadListRememberedCampaign(t *testing.T) {
	b, campaignID := cafeListScene(t)
	if p := listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug, CampaignFromCookie: true}); p.Filter.Campaign != cafeListSlug || len(p.Rows) != 3 {
		t.Errorf("a remembered campaign: echo %q, %v", p.Filter.Campaign, rowDomains(p))
	}
	p := listOf(t, b, console.LeadListQuery{Campaign: "closed-fictive", CampaignFromCookie: true})
	if len(p.Notes) != 0 || p.Filter.Campaign != "" || len(p.Rows) != 8 {
		t.Errorf("a gone remembered slug: notes %q, echo %q, %v", p.Notes, p.Filter.Campaign, rowDomains(p))
	}
	if p := listOf(t, b, console.LeadListQuery{Campaign: "closed-fictive"}); len(p.Notes) != 1 || !strings.Contains(p.Notes[0], "No campaign is called") {
		t.Errorf("a gone slug named: notes %q", p.Notes)
	}

	ownerExec(t, `UPDATE campaign SET retired_at = now() WHERE id = $1`, campaignID)
	p = listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug, CampaignFromCookie: true})
	if len(p.Notes) != 0 || p.Filter.Campaign != "" || len(p.Rows) != 8 || len(p.CampaignTabs) != 1 {
		t.Errorf("a retired remembered slug: notes %q, echo %q, %v, tabs %+v", p.Notes, p.Filter.Campaign, rowDomains(p), p.CampaignTabs)
	}
	p = listOf(t, b, console.LeadListQuery{Campaign: cafeListSlug})
	if p.Filter.Campaign != cafeListSlug || len(p.Rows) != 3 || len(p.CampaignTabs) != 2 {
		t.Errorf("a retired slug named: echo %q, %v, tabs %+v", p.Filter.Campaign, rowDomains(p), p.CampaignTabs)
	}
}
