//go:build integration

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// orderSites are eight reachable leads (GB, email, unsupported Drupal), seeded so that
// neither their ids nor their points give the order the operator sets. Points under the
// example profile: unsupported 20 + email 10 + major (7: 30, 8: 20, 9: 15).
var orderSites = []struct {
	domain string
	major  int
}{
	{"proseware.example", 9},    // unstarred, 45
	{"fourthcoffee.example", 8}, // unstarred, 50
	{"lucerne.example", 7},      // unstarred, 60
	{"margie.example", 7},
	{"humongous.example", 8},
	{"cpandl.example", 9},
	{"woodgrove.example", 7},
	{"bellows.example", 8},
}

// orderScene is a fresh leads database holding the eight orderSites as leads, shadow mode
// and the clock at leadsTuesday. It returns the data directory.
func orderScene(t *testing.T) string {
	t.Helper()
	dir := leadsDB(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	t.Setenv("LEADHOUND_TZ", "Europe/Bucharest")
	saved := leadsNow
	leadsNow = func() time.Time { return leadsTuesday }
	t.Cleanup(func() { leadsNow = saved })

	ctx := context.Background()
	lq := leadstoretest.Q(t)
	seedMailbox(t, lq, testUser, withDNSPass())
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-sites-probe", "order-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range orderSites {
		home, err := core.InsertFetch(ctx, runID, "order-fixture", fetch.Record{Kind: fetch.KindSite, URL: "https://" + s.domain + "/",
			Status: 200, Body: []byte("<html></html>"), ContentType: "text/html"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{{Domain: s.domain, Country: "GB"}}); err != nil {
			t.Fatal(err)
		}
		siteID, err := lq.SiteIDByDomain(ctx, s.domain)
		if err != nil {
			t.Fatal(err)
		}
		probeID, err := lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{
			Outcome: leadstore.OutcomeOK, FinalHost: s.domain, IsDrupal: true, Major: s.major,
			Signals:  []leadstore.Signal{{Signal: "meta-generator", Value: strconv.Itoa(s.major), FetchID: home}},
			FetchIDs: []int64{home}, DerivedBy: "drupal@1",
		})
		if err != nil {
			t.Fatal(err)
		}
		orgID, _, err := lq.UpsertSiteOrg(ctx, probeID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteEmail, Value: "info@" + s.domain,
			FetchID: home, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
			t.Fatal(err)
		}
	}
	if out := mustRun(t, "leads", "build"); !strings.Contains(out, "8 leads (8 new") {
		t.Fatalf("build:\n%s", out)
	}
	return dir
}

// leadIDOf is the lead id of the lead on domain.
func leadIDOf(t *testing.T, domain string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = $1`, domain).Scan(&id); err != nil {
		t.Fatalf("lead of %s: %v", domain, err)
	}
	return id
}

// batchItem is one item heading of the console's batch page: "<h2>N. domain</h2>".
var batchItem = regexp.MustCompile(`<h2>(\d+)\. ([^<]+)</h2>`)

// batchDomains is batch id's items as the console's batch page lists them, in its order.
func batchDomains(t *testing.T, h http.Handler, cookie string, id int64) []string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/leads/batches/"+strconv.FormatInt(id, 10), nil)
	r.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("batch %d page: %d", id, w.Code)
	}
	var out []string
	for i, m := range batchItem.FindAllStringSubmatch(w.Body.String(), -1) {
		if m[1] != strconv.Itoa(i+1) {
			t.Errorf("batch %d item %d is numbered %s", id, i+1, m[1])
		}
		out = append(out, m[2])
	}
	return out
}

// TestLeadsBatchFollowsStarOrder (Task 3.4, D5 and D6): stars set through the console
// decide what a batch drafts and in which order. With batches drawn from starred leads (the
// default) a batch holds exactly the five starred leads in the operator's order after a
// move down and a pin to top; the three unstarred are absent. Nothing starred is a refusal
// in plain words, on the console and the CLI alike, and writes no batch. With
// starred_then_rest the three follow the five, by points.
func TestLeadsBatchFollowsStarOrder(t *testing.T) {
	ctx := context.Background()
	dir := orderScene(t)
	b := leadsBackend(t)
	h, cookie := jobsHandler(t, b)

	// Nothing starred: the CLI says so and exits 0 (the morning timer must not fail on it);
	// the console refuses with the same words.
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, noStarredWords) {
		t.Errorf("batch new with no stars:\n%s", out)
	}
	func() {
		goLive(t)
		defer t.Setenv("LEADHOUND_OUTREACH", "")
		_, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch})
		if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "No starred leads are ready: star some on Leads.") {
			t.Errorf("console new batch with no stars: %v", err)
		}
	}()
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch`); n != 0 {
		t.Fatalf("%d batches with nothing starred", n)
	}

	post := func(domain, do string) {
		t.Helper()
		path := "/leads/" + strconv.FormatInt(leadIDOf(t, domain), 10) + "/" + do
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"back": {"/leads?sort=order"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Cookie", cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads?sort=order" {
			t.Fatalf("POST %s (%s): %d to %q %s", path, domain, w.Code, w.Header().Get("Location"), w.Body.String())
		}
	}
	for _, d := range []string{"cpandl.example", "bellows.example", "margie.example", "woodgrove.example", "humongous.example"} {
		post(d, console.LeadStar)
	}
	post("bellows.example", console.LeadDown)  // cpandl, margie, bellows, woodgrove, humongous
	post("humongous.example", console.LeadTop) // humongous, cpandl, margie, bellows, woodgrove
	starred := []string{"humongous.example", "cpandl.example", "margie.example", "bellows.example", "woodgrove.example"}

	out := mustRun(t, "leads", "batch", "new")
	if !strings.Contains(out, "Batch 1 (shadow)") || strings.Contains(out, "dropped") {
		t.Fatalf("batch new:\n%s", out)
	}
	if got := batchDomains(t, h, cookie, 1); !slices.Equal(got, starred) {
		t.Errorf("batch 1 lists %v, want the starred leads in order %v", got, starred)
	}
	if n := batchCount(t, `SELECT count(DISTINCT m.lead_id)::int FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain IN ('proseware.example', 'fourthcoffee.example', 'lucerne.example')`); n != 0 {
		t.Errorf("%d unstarred leads were drafted", n)
	}

	// Batches drawn from starred_then_rest: the five in the operator's order, then the three
	// by points (lucerne 60, fourthcoffee 50, proseware 45), not by id.
	mustRun(t, "leads", "batch", "discard", "1")
	drawFromAll(t, dir)
	out = mustRun(t, "leads", "batch", "new")
	if !strings.Contains(out, "Batch 2 (shadow)") || strings.Contains(out, "dropped") {
		t.Fatalf("batch new, starred then the rest:\n%s", out)
	}
	want := append(slices.Clone(starred), "lucerne.example", "fourthcoffee.example", "proseware.example")
	if got := batchDomains(t, h, cookie, 2); !slices.Equal(got, want) {
		t.Errorf("batch 2 lists %v, want %v", got, want)
	}
}
