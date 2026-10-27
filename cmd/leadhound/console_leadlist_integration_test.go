//go:build integration

package main

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// leadListRowID is one lead's row in the list's table.
var leadListRowID = regexp.MustCompile(`<tr id="lead-w-(\d+)">`)

// listedLeads is the ids of the leads a list page shows, in its order.
func listedLeads(page string) []string {
	var ids []string
	for _, m := range leadListRowID.FindAllStringSubmatch(page, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// TestConsoleLeadList: the Leads list and a lead's page end to end, through the real
// backend and the console handler against the test database. The batch fixture gains a
// second Drupal 10 site without a phone; the first one and an out-of-support site get a
// phone, and the first one a description and a social link, each read from its own page.
// /leads?kind=d10&has_phone=1 lists only the Drupal 10 lead with a phone, kind=d10 alone
// lists both, and the lead's page shows the phone, the description, the social link and the
// fetch its finding was read from. A value the form does not offer is ignored with a note;
// a lead that does not exist is a 404.
func TestConsoleLeadList(t *testing.T) {
	leadsScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-enrich", "leadlist-fixture")
	if err != nil {
		t.Fatal(err)
	}
	page := func(u string) int64 {
		id, err := core.InsertFetch(ctx, runID, "leadlist-fixture", fetch.Record{Kind: fetch.KindSite, URL: u, Status: 200,
			Body: []byte("<html></html>"), ContentType: "text/html"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// A second Drupal 10 site, reachable by email, with no phone.
	if _, err := lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{{Domain: "woodgrove.example", Country: "GB"}}); err != nil {
		t.Fatal(err)
	}
	siteID, err := lq.SiteIDByDomain(ctx, "woodgrove.example")
	if err != nil {
		t.Fatal(err)
	}
	home := page("https://woodgrove.example/")
	probeID, err := lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: "woodgrove.example", IsDrupal: true, Major: 10, ExactVersion: "10.5.2",
		Signals: []leadstore.Signal{{Signal: "x-generator", Value: "10", FetchID: home}}, FetchIDs: []int64{home}, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	woodOrg, _, err := lq.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: woodOrg, Kind: leadstore.RouteEmail, Value: "info@woodgrove.example",
		FetchID: home, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "build")

	lead := func(domain string) (leadID, orgID int64) {
		t.Helper()
		if err := storetest.OwnerConn(t).QueryRow(ctx,
			`SELECT l.id, l.org_id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = $1`, domain).Scan(&leadID, &orgID); err != nil {
			t.Fatalf("lead of %s: %v", domain, err)
		}
		return leadID, orgID
	}
	contoso, contosoOrg := lead("contoso.example")
	woodgrove, _ := lead("woodgrove.example")
	northwind, northwindOrg := lead("northwind.example")

	// Phones on contoso (Drupal 10) and northwind (out of support), in the drama range; a
	// description and a social link on contoso.
	fact := func(org int64, kind, value, norm, from string) {
		t.Helper()
		if _, err := lq.InsertFact(ctx, leadstore.FactRow{OrgID: org, Kind: kind, Value: value, Normalized: norm,
			FetchID: page(from), DerivedBy: "enrich@1"}); err != nil {
			t.Fatal(err)
		}
	}
	const phone, desc = "+44 20 7946 0123", "Contoso makes demo widgets for the example economy."
	fact(contosoOrg, leadstore.FactPhone, phone, "+442079460123", "https://contoso.example/kontakt")
	fact(contosoOrg, leadstore.FactDescription, desc, desc, "https://contoso.example/about")
	fact(contosoOrg, leadstore.FactSocial, "https://www.social.example/contoso", "https://www.social.example/contoso", "https://contoso.example/")
	fact(northwindOrg, leadstore.FactPhone, "+44 20 7946 0456", "+442079460456", "https://northwind.example/contact")

	c := newConsoleClient(t, leadsBackend(t), nil)
	list := func(path string) string {
		t.Helper()
		w := c.get(path, false)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d\n%s", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	// The filter: only the Drupal 10 lead with a phone.
	got := list("/leads?kind=d10&has_phone=1")
	if ids := listedLeads(got); !slices.Equal(ids, []string{fmt.Sprint(contoso)}) {
		t.Errorf("kind=d10&has_phone=1 lists leads %v, want only %d:\n%s", ids, contoso, got)
	}
	for _, want := range []string{`name="kind" value="d10" checked`, `name="has_phone" value="1" checked`, "Drupal 10.6.14, security support ends 9 December 2026"} {
		if !strings.Contains(got, want) {
			t.Errorf("kind=d10&has_phone=1 lacks %q", want)
		}
	}
	// Each filter alone: kind=d10 lists both Drupal 10 leads, has_phone=1 both leads with a
	// phone, so the combined filter above is not empty by accident.
	if ids := listedLeads(list("/leads?kind=d10")); len(ids) != 2 || !slices.Contains(ids, fmt.Sprint(woodgrove)) || !slices.Contains(ids, fmt.Sprint(contoso)) {
		t.Errorf("kind=d10 lists %v, want %d and %d", ids, contoso, woodgrove)
	}
	if ids := listedLeads(list("/leads?has_phone=1")); len(ids) != 2 || !slices.Contains(ids, fmt.Sprint(northwind)) || !slices.Contains(ids, fmt.Sprint(contoso)) {
		t.Errorf("has_phone=1 lists %v, want %d and %d", ids, contoso, northwind)
	}

	// A kind the form does not offer is ignored with a note, and the list is unfiltered.
	got = list("/leads?kind=bogus")
	if note := template.HTMLEscapeString(`Ignored the kind "bogus": not one the list offers.`); !strings.Contains(got, note) {
		t.Errorf("kind=bogus lacks the note %q:\n%s", note, got)
	}
	if ids := listedLeads(got); len(ids) < 3 {
		t.Errorf("kind=bogus lists %v, want every open lead", ids)
	}

	// The lead's page: the phone, the description, the social link and the fetch behind
	// the finding.
	detail := list(fmt.Sprintf("/leads/%d", contoso))
	for _, want := range []string{
		strings.ReplaceAll(phone, "+", "&#43;"), template.HTMLEscapeString(desc), // html/template writes + as &#43;
		`<a href="https://contoso.example/" rel="noopener noreferrer nofollow">https://contoso.example/</a>`,
		`<a href="https://contoso.example/kontakt" rel="noopener noreferrer nofollow">`,
		"social.example: ", "200 OK", "Drupal 10.6.14, security support ends 9 December 2026",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("lead page lacks %q:\n%s", want, detail)
		}
	}
	if w := c.get("/leads/999999", false); w.Code != http.StatusNotFound {
		t.Errorf("no such lead: %d", w.Code)
	}
}
