//go:build integration

package main

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/console"
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

	// A page past the end, however large, is the last page with a note, never an error: a
	// page near the largest int used to overflow the offset and fail the query.
	for _, p := range []string{"9223372036854775807", "184467440737095517", "1000001"} {
		got = list("/leads?page=" + p)
		if note := "There is no page "; !strings.Contains(got, note) || !strings.Contains(got, "this is the last page.") {
			t.Errorf("page=%s lacks the last-page note:\n%s", p, got)
		}
		if ids := listedLeads(got); len(ids) < 3 {
			t.Errorf("page=%s lists %v, want the last page's leads", p, ids)
		}
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

// TestConsoleLeadPageCafe: a café lead's page, through the real backend and the console
// handler. The demo café import is probed with the Bean Example House homepage carrying a
// Stamp Me loyalty script and an Uber Eats ordering link; the campaign spec's vendor_links
// give Stamp Me its comparison page and a default page. That café's page shows the place
// (category, address, its website), the loyalty and ordering vendors by name and the
// Stamp Me comparison link, with no report and no Drupal anywhere; a café with nothing seen
// gets the default comparison link, and the café with no website says so. A Drupal site
// lead keeps its report.
func TestConsoleLeadPageCafe(t *testing.T) {
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	importDemoPlaces(t, dir)
	fixtures := cafeFixtures(t)
	page, err := os.ReadFile("../../internal/cafe/testdata/loyalty_vendor.html")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "bean-house.example.html"), page, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	if out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", fixtures); !strings.Contains(out, "· 4 sites probed") {
		t.Fatalf("probe:\n%s", out)
	}
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")

	ctx := context.Background()
	b := leadsBackend(t)
	bean := leadIDOf(t, "bean-house.example")
	d, err := b.LeadDetail(ctx, bean)
	if err != nil {
		t.Fatal(err)
	}
	if d.ShowReport || d.Report.Ready || d.Report.Why != "" {
		t.Errorf("a café lead has a report: %v %+v", d.ShowReport, d.Report)
	}
	if d.Place == nil || d.Place.Category != "cafe" || d.Place.Address != "3 Example Street, 000000 Springfield, United States" ||
		d.Place.Website != "https://bean-house.example/" {
		t.Errorf("the place %+v", d.Place)
	}
	if c := d.Cafe; c == nil || !slices.Equal(c.Loyalty, []string{"Stamp Me"}) || !slices.Contains(c.Ordering, "Uber Eats") || !c.Instagram ||
		c.VendorName != "Stamp Me" || c.VendorLink != "https://compare.example/vs/stamp-me" {
		t.Errorf("the café view %+v", d.Cafe)
	}
	if d.ReachTitle != "From their listing and site" || d.OriginWords != "a map listing (Overture)" || d.RType != leadstore.TypeCafe {
		t.Errorf("reach %q, origin %q, type %q", d.ReachTitle, d.OriginWords, d.RType)
	}

	c := newConsoleClient(t, b, nil)
	show := func(id int64) string {
		t.Helper()
		w := c.get(fmt.Sprintf("/leads/%d", id), false)
		if w.Code != http.StatusOK {
			t.Fatalf("lead %d: %d\n%s", id, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	body := show(bean)
	for _, want := range []string{
		"<h2>The place</h2>", "3 Example Street, 000000 Springfield, United States", "<dt>Digital loyalty</dt><dd>Stamp Me</dd>",
		`<a href="https://compare.example/vs/stamp-me" rel="noopener noreferrer nofollow">Compare with Stamp Me</a>`,
		"<h2>From their listing and site</h2>", "<dt>Found through</dt><dd>a map listing (Overture)</dd>", "at least 30 café leads",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the café page lacks %q", want)
		}
	}
	cafePageClean(t, "Bean Example House", body)

	// A café with nothing seen gets the campaign's default comparison page.
	harbour := leadIDOf(t, "harbour-lane-coffee.example")
	if d, err := b.LeadDetail(ctx, harbour); err != nil || d.Cafe == nil || d.Cafe.VendorName != "" || d.Cafe.VendorLink != "https://compare.example/vs" ||
		len(d.Cafe.Loyalty) != 0 || d.Place == nil {
		t.Errorf("the café with nothing seen: %+v %+v %v", d.Cafe, d.Place, err)
	}
	body = show(harbour)
	if want := `<a href="https://compare.example/vs" rel="noopener noreferrer nofollow">Compare loyalty apps</a>`; !strings.Contains(body, want) {
		t.Errorf("the café with nothing seen lacks %q", want)
	}
	cafePageClean(t, "Harbour Lane Coffee", body)

	// The café with no website of its own.
	var noSite int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT id FROM lead WHERE kind = 'cafe_no_website'`).Scan(&noSite); err != nil {
		t.Fatalf("the café with no website: %v", err)
	}
	if d, err := b.LeadDetail(ctx, noSite); err != nil || d.Place == nil || d.Place.Website != "" || d.ShowReport {
		t.Errorf("the café with no website: %+v %v", d.Place, err)
	}
	body = show(noSite)
	if !strings.Contains(body, "<dt>Website</dt><dd>none of its own</dd>") {
		t.Error("the café with no website does not say so")
	}
	cafePageClean(t, "the café with no website", body)
}

// cafePageClean fails when a café lead's page shows the upgrade-risk report, a list rank or
// the word Drupal anywhere.
func cafePageClean(t *testing.T, what, body string) {
	t.Helper()
	for _, bad := range []string{"Upgrade-risk", "List rank", "The site</h2>"} {
		if strings.Contains(body, bad) {
			t.Errorf("%s's page shows %q", what, bad)
		}
	}
	if i := strings.Index(strings.ToLower(body), "drupal"); i >= 0 {
		t.Errorf("%s's page says Drupal: …%s…", what, body[max(i-200, 0):min(i+100, len(body))])
	}
}

// TestConsoleCallsByType: the "Your call" block of a lead counts the calls of its own type
// and offers its own type's help, and the list header shows each type's count. Two leads
// are made cafés; two café calls and one Drupal call are given through the console.
func TestConsoleCallsByType(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	b := leadsBackend(t)
	c1 := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	c2 := setLeadKind(t, "proseware.example", cafe.KindHasLoyalty)
	margie := leadIDOf(t, "margie.example")
	for id, band := range map[int64]string{c1: "likely", c2: "no", margie: "maybe"} {
		if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLabel, ID: id, Value: band}); err != nil {
			t.Fatal(err)
		}
	}
	cd, err := b.LeadDetail(ctx, c1)
	if err != nil {
		t.Fatal(err)
	}
	dd, err := b.LeadDetail(ctx, margie)
	if err != nil {
		t.Fatal(err)
	}
	if cd.YourCall.Labelled != 2 || dd.YourCall.Labelled != 1 || cd.YourCall.LabelGoal != labelGoal {
		t.Errorf("café card %d of %d, Drupal card %d, want 2 and 1", cd.YourCall.Labelled, cd.YourCall.LabelGoal, dd.YourCall.Labelled)
	}
	for i, band := range cd.YourCall.Bands {
		if band.Help == dd.YourCall.Bands[i].Help || strings.Contains(band.Help, "Drupal") {
			t.Errorf("café band %s has the Drupal help %q", band.Value, band.Help)
		}
	}
	if cd.ShowReport || cd.Report.Ready {
		t.Errorf("a café lead has a report: %+v", cd.Report)
	}

	p, err := b.LeadList(ctx, console.LeadListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := []console.TypeCount{{Type: "Drupal leads", Labelled: 1, Goal: labelGoal}, {Type: "Café leads", Labelled: 2, Goal: labelGoal}}
	if !slices.Equal(p.Labelled, want) {
		t.Errorf("the list header counts %+v, want %+v", p.Labelled, want)
	}
	c := newConsoleClient(t, b, nil)
	body := c.get("/leads", false).Body.String()
	if line := "<strong>Drupal leads: 1 of 30 labelled · Café leads: 2 of 30 labelled.</strong>"; !strings.Contains(body, line) {
		t.Errorf("the list header lacks %q", line)
	}
	if body := c.get(fmt.Sprintf("/leads/%d", c1), false).Body.String(); !strings.Contains(body, "2 of 30 labelled") ||
		!strings.Contains(body, "at least 30 café leads") {
		t.Error("the café lead's call block does not count café calls")
	}
}
