//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/places"
)

// listPlace imports one place of campaign for the org of domain, later than every place
// imported before it. Its release also makes its source key unique.
func (f outreachFixture) listPlace(t *testing.T, campaign int64, domain, release, category string, addr places.Address) {
	t.Helper()
	p := places.Place{SourceID: domain + "-" + release, Name: "Fictive " + domain, Category: category, Confidence: 0.9,
		Websites: []string{"https://" + domain + "/"}, Address: addr}
	if _, err := f.q.ImportPlaces(context.Background(), leadstore.PlaceImport{CampaignID: campaign, Release: release,
		FileName: "places.ndjson", Stats: places.Stats{Lines: 1, Kept: 1}}, []leadstore.PlaceRow{{Place: p}}); err != nil {
		t.Fatalf("place of %s: %v", domain, err)
	}
}

// townLead adds a café lead in campaign 1 whose one place is in town.
func (f outreachFixture) townLead(t *testing.T, domain, email, town string) int64 {
	t.Helper()
	id := f.cafeLead(t, domain, nil, email)
	f.listPlace(t, 1, domain, "2026-10-21.0", "coffee_shop", places.Address{Locality: town, Country: "GB"})
	return id
}

// operatorPlaceType writes the operator's answer on lead.
func (f outreachFixture) operatorPlaceType(t *testing.T, lead int64, placeType string) {
	t.Helper()
	if err := f.q.SetPlaceType(context.Background(), lead, placeType, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy); err != nil {
		t.Fatalf("place type of %d: %v", lead, err)
	}
}

// TestLeadListTownFilter: a town filter compares by lower(), diacritics included, leaves out a
// lead with no town, drops blank values, and every row carries its town as stored. ListTowns
// offers the same towns merged, most common spelling first, open leads only.
func TestLeadListTownFilter(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.townLead(t, "town-a.example", "", "Exampleton")
	b := f.townLead(t, "town-b.example", "", "Exampleton")
	c := f.townLead(t, "town-c.example", "", "EXAMPLETON")
	d := f.townLead(t, "town-d.example", "", "Țăndărei-Fictiv")
	d2 := f.townLead(t, "town-d2.example", "", "Țăndărei-Fictiv")
	e := f.townLead(t, "town-e.example", "", "țăndărei-fictiv")
	blank := f.townLead(t, "town-blank.example", "", "")
	noPlace := f.cafeLead(t, "town-none.example", nil, "")
	drupal := f.siteLead(t, "town-drupal.example", nil, 7, "")
	closed := f.townLead(t, "town-closed.example", "", "Closedville")
	f.ownerRun(t, `UPDATE lead SET state = 'skipped' WHERE id = $1`, closed)

	lc := leadstore.ListContext{}
	for name, tc := range map[string]struct {
		towns []string
		want  []int64
	}{
		"any case":            {[]string{"exampleTON"}, []int64{a, b, c}},
		"two towns":           {[]string{"exampleton", "ȚĂNDĂREI-FICTIV"}, []int64{a, b, c, d, d2, e}},
		"diacritics":          {[]string{"ȚĂNDĂREI-FICTIV"}, []int64{d, d2, e}},
		"no such town":        {[]string{"Nowhere"}, []int64{}},
		"blanks filter none":  {[]string{"", " "}, []int64{a, b, c, d, d2, e, blank, noPlace, drupal}},
		"blank beside a town": {[]string{"", "exampleton"}, []int64{a, b, c}},
	} {
		ids, total := listIDs(t, f.q, leadstore.ListFilter{Towns: tc.towns}, lc)
		if !reflect.DeepEqual(sortedIDs(ids...), sortedIDs(tc.want...)) || total != len(tc.want) {
			t.Errorf("%s: got %v (total %d), want %v", name, sortedIDs(ids...), total, sortedIDs(tc.want...))
		}
	}
	rows, _ := listRows(t, f.q, leadstore.ListFilter{}, lc)
	towns := map[int64]string{}
	for _, r := range rows {
		towns[r.LeadID] = r.Town
	}
	for lead, want := range map[int64]string{a: "Exampleton", c: "EXAMPLETON", d: "Țăndărei-Fictiv", blank: "", noPlace: "", drupal: ""} {
		if towns[lead] != want {
			t.Errorf("lead %d: town %q, want %q", lead, towns[lead], want)
		}
	}

	got, err := f.q.ListTowns(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []leadstore.TownCount{{Town: "Exampleton", Count: 3}, {Town: "Țăndărei-Fictiv", Count: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("towns = %+v, want %+v", got, want)
	}
	if every, err := f.q.ListTowns(ctx, 0); err != nil || !reflect.DeepEqual(every, want) {
		t.Errorf("towns of every campaign = %+v, %v", every, err)
	}
	if other, err := f.q.ListTowns(ctx, 999); err != nil || len(other) != 0 {
		t.Errorf("towns of a campaign with no lead = %+v, %v", other, err)
	}
	// Each town's count is the total the list gives under that town, as the filter offers it.
	for _, tw := range got {
		if _, total := listIDs(t, f.q, leadstore.ListFilter{CampaignID: 1, Status: leadstore.ListOpen, Towns: []string{tw.Town}}, lc); total != tw.Count {
			t.Errorf("town %q counts %d, lists %d", tw.Town, tw.Count, total)
		}
	}
}

// TestLeadListPlaceTypeFilter: a place-type filter shows only café leads of that type, a Drupal
// lead has an empty place type even with a stray row and is left out, and a value outside the
// two types is refused by the list and by the tab counts alike.
func TestLeadListPlaceTypeFilter(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	shop := f.cafeLead(t, "pt-shop.example", nil, "")
	roast := f.cafeLead(t, "pt-roast.example", nil, "")
	f.operatorPlaceType(t, roast, leadstore.PlaceRoastery)
	drupal := f.siteLead(t, "pt-drupal.example", nil, 7, "")
	f.ownerRun(t, `INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'roastery', 'operator', 'gremy')`, drupal)

	lc := leadstore.ListContext{}
	for name, tc := range map[string]struct {
		placeType string
		want      []int64
	}{
		"every lead":  {"", []int64{shop, roast, drupal}},
		"coffee shop": {leadstore.PlaceCafe, []int64{shop}},
		"roastery":    {leadstore.PlaceRoastery, []int64{roast}},
	} {
		ids, total := listIDs(t, f.q, leadstore.ListFilter{PlaceType: tc.placeType}, lc)
		if !reflect.DeepEqual(sortedIDs(ids...), sortedIDs(tc.want...)) || total != len(tc.want) {
			t.Errorf("%s: got %v (total %d), want %v", name, ids, total, tc.want)
		}
	}
	d, err := f.q.LeadDetail(ctx, drupal)
	if err != nil || d.PlaceType != "" {
		t.Errorf("Drupal detail: place type %q, %v", d.PlaceType, err)
	}
	if d, err := f.q.LeadDetail(ctx, roast); err != nil || d.PlaceType != leadstore.PlaceRoastery {
		t.Errorf("roastery detail: place type %q, %v", d.PlaceType, err)
	}
	for _, bad := range []string{leadstore.PlaceUnclear, "all", "CAFE", " "} {
		if _, _, err := f.q.LeadList(ctx, leadstore.ListFilter{PlaceType: bad}, lc); !errors.Is(err, leadstore.ErrBadListFilter) {
			t.Errorf("list, place type %q: %v", bad, err)
		}
		if _, err := f.q.ReachCounts(ctx, leadstore.ListFilter{PlaceType: bad}); !errors.Is(err, leadstore.ErrBadListFilter) {
			t.Errorf("counts, place type %q: %v", bad, err)
		}
	}
}

// TestLeadListPlaceTypeMatchesPlaceTypes: the place type the list reads and the one PlaceTypes
// reads agree on a fixture holding every combination of sources, and the filter keeps exactly
// the leads of that type: one SQL fragment, driven from both ends.
func TestLeadListPlaceTypeMatchesPlaceTypes(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	const (
		cafeT, roast, unclear = leadstore.PlaceCafe, leadstore.PlaceRoastery, leadstore.PlaceUnclear
		op, jev, name         = leadstore.PlaceFromOperator, leadstore.PlaceFromJev, leadstore.PlaceFromName
	)
	ids := []int64{f.siteLead(t, "combo-drupal.example", nil, 7, "")}
	for i, rows := range [][]placeAnswer{
		nil,
		{{roast, name}},
		{{roast, jev}},
		{{roast, op}},
		{{unclear, jev}},
		{{roast, name}, {cafeT, jev}},
		{{roast, name}, {unclear, jev}},
		{{roast, jev}, {cafeT, op}},
		{{cafeT, op}, {roast, name}},
		{{unclear, jev}, {roast, op}},
		{{roast, jev}, {unclear, jev}},
		{{cafeT, name}, {roast, jev}, {unclear, jev}},
		{{cafeT, name}, {cafeT, jev}, {roast, op}},
	} {
		lead := f.cafeLead(t, fmt.Sprintf("combo-%d.example", i), nil, "")
		for _, r := range rows {
			var sha []byte
			if r.source == jev {
				sha = placeSHA
			}
			if err := f.q.SetPlaceType(ctx, lead, r.placeType, r.source, "seen", sha, leadstore.ActorSystem); err != nil {
				t.Fatalf("set %v: %v", r, err)
			}
		}
		ids = append(ids, lead)
	}
	want, err := f.q.PlaceTypes(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	rows, total := listRows(t, f.q, leadstore.ListFilter{}, leadstore.ListContext{})
	if total != len(ids) {
		t.Fatalf("listed %d leads, want %d", total, len(ids))
	}
	byType := map[string][]int64{}
	for _, r := range rows {
		if r.PlaceType != want[r.LeadID].Type {
			t.Errorf("lead %d: list reads %q, PlaceTypes %q", r.LeadID, r.PlaceType, want[r.LeadID].Type)
		}
		byType[r.PlaceType] = append(byType[r.PlaceType], r.LeadID)
	}
	if len(byType[cafeT]) == 0 || len(byType[roast]) == 0 || len(byType[""]) != 1 {
		t.Fatalf("the fixture must hold each type: %v", byType)
	}
	for _, pt := range []string{cafeT, roast} {
		got, _ := listIDs(t, f.q, leadstore.ListFilter{PlaceType: pt}, leadstore.ListContext{})
		if !reflect.DeepEqual(sortedIDs(got...), sortedIDs(byType[pt]...)) {
			t.Errorf("filter %s: %v, want %v", pt, sortedIDs(got...), sortedIDs(byType[pt]...))
		}
	}
	// The town rule is shared the same way: the list's town is what the rating question reads.
	lead := f.townLead(t, "combo-town.example", "", "Exampleton")
	r, err := f.q.RateLeadByID(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	listed, _ := listRows(t, f.q, leadstore.ListFilter{LeadIDs: []int64{lead}}, leadstore.ListContext{})
	if len(listed) != 1 || listed[0].Town != r.PlaceLocality {
		t.Errorf("list town %+v, rating town %q", listed, r.PlaceLocality)
	}
}

// TestReachCountsWithTownAndPlaceType: with place type and town on, each tab's count equals the
// total LeadList gives under the same filter with that tab as its Reach.
func TestReachCountsWithTownAndPlaceType(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	email := f.townLead(t, "tabs-email.example", "hi@tabs-email.example", "Exampleton")
	phone := f.townLead(t, "tabs-phone.example", "", "EXAMPLETON")
	f.phoneFact(t, phone, "+441632960101")
	f.townLead(t, "tabs-bare.example", "", "exampleton")
	reached := f.townLead(t, "tabs-reached.example", "", "Exampleton")
	f.ownerRun(t, `UPDATE lead SET state = 'contacted' WHERE id = $1`, reached)
	// Leads each filter must leave out: another town, a roastery, a Drupal lead.
	f.townLead(t, "tabs-other.example", "hi@tabs-other.example", "Otherville")
	roast := f.townLead(t, "tabs-roast.example", "hi@tabs-roast.example", "Exampleton")
	f.operatorPlaceType(t, roast, leadstore.PlaceRoastery)
	f.siteLead(t, "tabs-drupal.example", nil, 7, "hi@tabs-drupal.example")

	both := leadstore.ListFilter{PlaceType: leadstore.PlaceCafe, Towns: []string{"exampleton"}}
	for name, flt := range map[string]leadstore.ListFilter{
		"place type and town": both,
		"place type":          {PlaceType: leadstore.PlaceRoastery},
		"town":                {Towns: []string{"EXAMPLETON"}},
		"both, open":          {PlaceType: leadstore.PlaceCafe, Towns: []string{"exampleton"}, Status: leadstore.ListOpen},
		"both, campaign":      {PlaceType: leadstore.PlaceCafe, Towns: []string{"exampleton"}, CampaignID: 1},
	} {
		c, err := f.q.ReachCounts(ctx, flt)
		if err != nil {
			t.Fatalf("%s: counts: %v", name, err)
		}
		for b, n := range map[string]int{leadstore.ReachContacted: c.Contacted, leadstore.ReachEmail: c.Email,
			leadstore.ReachManual: c.Manual, leadstore.ReachDead: c.Dead, leadstore.ReachOther: c.Other} {
			one := flt
			one.Reach = b
			if _, total := listIDs(t, f.q, one, leadstore.ListContext{}); total != n {
				t.Errorf("%s: %s counts %d, lists %d", name, b, n, total)
			}
		}
	}
	c, err := f.q.ReachCounts(ctx, both)
	if err != nil {
		t.Fatal(err)
	}
	if c.All() != 4 || c.Contacted != 1 || c.Email != 1 {
		t.Errorf("counts under both filters = %+v, want 4 leads with 1 contacted and 1 by email", c)
	}
	if lead, _ := listIDs(t, f.q, leadstore.ListFilter{PlaceType: leadstore.PlaceCafe, Towns: []string{"exampleton"}, Reach: leadstore.ReachEmail}, leadstore.ListContext{}); !slices.Equal(lead, []int64{email}) {
		t.Errorf("the email tab under both filters = %v, want %d", lead, email)
	}
}

// TestListExtras: one lead in full, a lead with two places and with none, retired routes and
// phones left out, the latest event's actor and reason, and an id with no lead.
func TestListExtras(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	page := f.pageFetch(t, "https://full.example/contact", "", "<html>contact</html>")

	full := f.cafeLead(t, "full.example", nil, "live@full.example")
	org := f.orgOf(t, full)
	// The place wins by being latest: an older one here, and a later one in another campaign.
	f.listPlace(t, 1, "full.example", "2026-09-01.0", "bakery", places.Address{Locality: "Oldtown", Freeform: "1 Old Road", Country: "GB"})
	f.listPlace(t, 1, "full.example", "2026-10-21.0", "coffee_shop",
		places.Address{Locality: "Exampleton", Freeform: "5 High Street", Postcode: "EX1 1AA", Country: "GB"})
	f.ownerRun(t, `INSERT INTO campaign (id, account_id, slug, name) VALUES (2, 1, 'other', 'Other')`)
	f.listPlace(t, 2, "full.example", "2026-11-18.0", "bar", places.Address{Locality: "Elsewhere", Freeform: "9 Far Lane", Country: "GB"})

	f.fact(t, full, page, leadstore.FactDescription, "A fictive café", "a fictive café")
	f.fact(t, full, page, leadstore.FactDescription, "A second telling", "a second telling")
	f.fact(t, full, page, leadstore.FactSocial, "https://social.example/full", "https://social.example/full")
	livePhone := "+441632960111"
	f.phoneFact(t, full, livePhone)
	retiredPhone := f.phoneFact(t, full, "+441632960112")
	for _, r := range []leadstore.RouteRow{
		{Kind: leadstore.RouteEmail, Value: "retired@full.example", MXOK: true},
		{Kind: leadstore.RouteForm, Value: "https://full.example/contact"},
		{Kind: leadstore.RouteForm, Value: "https://full.example/retired"},
		{Kind: leadstore.RouteLinkedIn, Value: "https://www.linkedin.com/company/fictive-full"},
		{Kind: leadstore.RouteInstagram, Value: "full_cafe.example"},
		{Kind: leadstore.RouteInstagram, Value: "retired_cafe.example"},
	} {
		r.OrgID, r.FetchID, r.DerivedBy = org, page, "contacts@1"
		if _, err := f.q.InsertRoute(ctx, r); err != nil {
			t.Fatalf("route %s: %v", r.Value, err)
		}
	}
	routes, err := f.q.RoutesFor(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Value == "retired@full.example" || r.Value == "https://full.example/retired" || r.Value == "https://www.instagram.com/retired_cafe.example/" {
			if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: r.ID, Mark: leadstore.MarkRetire}); err != nil {
				t.Fatalf("retire %s: %v", r.Value, err)
			}
		}
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, FactID: retiredPhone, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatalf("retire phone: %v", err)
	}
	f.ownerRun(t, `INSERT INTO lead_event (lead_id, state, reason, actor) VALUES ($1, 'new', 'older reason', 'system'), ($1, 'skipped', 'not a fit', 'gremy')`, full)

	bare := f.cafeLead(t, "bare.example", nil, "")
	// A Drupal lead with a place of its own reads no place, as the list's town does.
	drupal := f.siteLead(t, "drupal-x.example", nil, 7, "")
	f.listPlace(t, 1, "drupal-x.example", "2026-10-21.0", "coffee_shop", places.Address{Locality: "Exampleton", Freeform: "2 Side Road", Country: "GB"})

	got, err := f.q.ListExtras(ctx, []int64{full, bare, drupal, 999_999})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[999_999]; ok || len(got) != 3 {
		t.Errorf("extras hold %d leads, want 3 and no entry for a missing id", len(got))
	}
	want := leadstore.RowExtras{LeadID: full, Description: "A fictive café", Address: "5 High Street, EX1 1AA", Category: "coffee_shop",
		Socials: []string{"https://social.example/full"}, Phones: []string{livePhone},
		Emails: []string{"live@full.example"}, Forms: []string{"https://full.example/contact"},
		LinkedIns:  []string{"https://www.linkedin.com/company/fictive-full"},
		Instagrams: []string{"https://www.instagram.com/full_cafe.example/"}, LastActor: leadstore.ActorGremy, LastReason: "not a fit"}
	if !reflect.DeepEqual(got[full], want) {
		t.Errorf("full lead:\n got %+v\nwant %+v", got[full], want)
	}
	// The town the list reads for the same lead names the same latest place.
	if rows, _ := listRows(t, f.q, leadstore.ListFilter{LeadIDs: []int64{full}}, leadstore.ListContext{}); len(rows) != 1 || rows[0].Town != "Exampleton" {
		t.Errorf("list row of the full lead = %+v, want town Exampleton", rows)
	}
	empty := []string{}
	for name, id := range map[string]int64{"no place": bare, "Drupal": drupal} {
		e := got[id]
		if e.Address != "" || e.Category != "" || e.Description != "" {
			t.Errorf("%s: place or description read: %+v", name, e)
		}
		if !reflect.DeepEqual(e.Socials, empty) || !reflect.DeepEqual(e.Phones, empty) || !reflect.DeepEqual(e.Forms, empty) ||
			!reflect.DeepEqual(e.LinkedIns, empty) || !reflect.DeepEqual(e.Instagrams, empty) {
			t.Errorf("%s: slices must be empty and not nil: %+v", name, e)
		}
	}
	if none, err := f.q.ListExtras(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("no ids: %v, %v", none, err)
	}
}

// TestListExtrasRetiredRouteMatchesDetail: extras hold exactly the live routes and phones
// LeadDetail shows, so the row and the lead page never disagree on a retired one.
func TestListExtrasRetiredRouteMatchesDetail(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "agree.example", nil, 7, "one@agree.example")
	org := f.orgOf(t, lead)
	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: org, Kind: leadstore.RouteEmail, Value: "two@agree.example",
		FetchID: f.fid, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
	routes, err := f.q.RoutesFor(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Value == "one@agree.example" {
			if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: r.ID, Mark: leadstore.MarkRetire}); err != nil {
				t.Fatal(err)
			}
		}
	}
	d, err := f.q.LeadDetail(ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	var detail []string
	for _, r := range d.Routes {
		if r.Kind == leadstore.RouteEmail {
			detail = append(detail, r.Value)
		}
	}
	got, err := f.q.ListExtras(ctx, []int64{lead})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got[lead].Emails, detail) || !reflect.DeepEqual(detail, []string{"two@agree.example"}) {
		t.Errorf("extras %v, detail %v, want only the live address", got[lead].Emails, detail)
	}
}
