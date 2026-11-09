//go:build integration

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// campaignOf is the view of slug in views, failing the test when there is none.
func campaignOf(t *testing.T, views []console.CampaignView, slug string) console.CampaignView {
	t.Helper()
	i := slices.IndexFunc(views, func(v console.CampaignView) bool { return v.Slug == slug })
	if i < 0 {
		t.Fatalf("no campaign %s in %+v", slug, views)
	}
	return views[i]
}

// examplesOf is the first five rows of p as a card's examples.
func examplesOf(p console.LeadListPage) []console.CampaignExample {
	var out []console.CampaignExample
	for _, r := range p.Rows[:min(len(p.Rows), 5)] {
		out = append(out, console.CampaignExample{LeadID: r.LeadID, Name: r.Name, Line: r.Summary})
	}
	return out
}

// TestCampaignCardsMatchLeadList: a card's examples are the first five rows of the Leads list
// for that campaign under its default filter, so the café campaign's card shows coffee shops
// and never its roastery. Its open count is the list's tab and its email-ready count the
// list's Email tab. A campaign with no lead has no examples and says it holds none; a retired
// campaign reads nothing for its card.
func TestCampaignCardsMatchLeadList(t *testing.T) {
	b, cafeID := cafeListScene(t)
	ctx := context.Background()
	if _, err := storetest.OwnerConn(t).Exec(ctx,
		`INSERT INTO campaign (account_id, slug, name) VALUES (1, 'fictive-empty', 'Fictive Empty')`); err != nil {
		t.Fatal(err)
	}
	views, err := b.Campaigns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{cafeListSlug, "drupal-eol"} {
		v := campaignOf(t, views, slug)
		p := listOf(t, b, console.LeadListQuery{Campaign: slug})
		want := examplesOf(p)
		if len(want) == 0 || !slices.Equal(v.Examples, want) {
			t.Errorf("%s: examples %+v, want the list's first rows %+v", slug, v.Examples, want)
		}
		tab := slices.IndexFunc(p.CampaignTabs, func(c console.CampaignTab) bool { return c.Slug == slug })
		if tab < 0 || v.Open != p.CampaignTabs[tab].Open || v.EmailReady != p.ReachCounts.Email || !v.HasLeads {
			t.Errorf("%s: open %d, email ready %d, has leads %v; the list's tabs %+v, email %d", slug, v.Open, v.EmailReady, v.HasLeads, p.CampaignTabs, p.ReachCounts.Email)
		}
		for _, e := range v.Examples {
			if e.Line == "" || e.Name == "" {
				t.Errorf("%s: an example with no name or line: %+v", slug, e)
			}
		}
	}
	cafes := campaignOf(t, views, cafeListSlug)
	if cafes.Open != 4 || len(cafes.Examples) != 3 || slices.ContainsFunc(cafes.Examples, func(e console.CampaignExample) bool { return e.LeadID == leadIDOf(t, "cpandl.example") }) {
		t.Errorf("the café card: open %d, examples %+v", cafes.Open, cafes.Examples)
	}
	if e := campaignOf(t, views, "fictive-empty"); e.HasLeads || e.Open != 0 || e.EmailReady != 0 || len(e.Examples) != 0 {
		t.Errorf("a campaign with no lead: %+v", e)
	}

	ownerExec(t, `UPDATE campaign SET retired_at = now() WHERE id = $1`, cafeID)
	if views, err = b.Campaigns(ctx); err != nil {
		t.Fatal(err)
	}
	if r := campaignOf(t, views, cafeListSlug); !r.Retired || r.Open != 0 || r.EmailReady != 0 || r.HasLeads || r.Examples != nil {
		t.Errorf("a retired campaign read its card: %+v", r)
	}
}
