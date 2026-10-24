//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func intp(n int) *int { return &n }

// siteFixture is a reset database with the lead queries, the core queries and one run.
type siteFixture struct {
	q     leadstore.Queries
	core  store.Queries
	runID int64
}

func newSiteFixture(t *testing.T) siteFixture {
	t.Helper()
	leadstoretest.Reset(t)
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(context.Background(), "leads-sites", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	return siteFixture{q: leadstoretest.Q(t), core: core, runID: runID}
}

// fetchRow writes one site fetch for the fixture's run and returns its id.
func (f siteFixture) fetchRow(t *testing.T, url string) int64 {
	t.Helper()
	id, err := f.core.InsertFetch(context.Background(), f.runID, "leads", fetch.Record{
		Kind: fetch.KindSite, URL: url, Status: 200, Body: []byte("<html></html>"), ContentType: "text/html",
	})
	if err != nil {
		t.Fatalf("insert fetch: %v", err)
	}
	return id
}

// siteID returns the id of the site row for domain.
func siteID(t *testing.T, domain string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		"SELECT id FROM site WHERE domain = $1", domain).Scan(&id); err != nil {
		t.Fatalf("site %s: %v", domain, err)
	}
	return id
}

func TestImportSitesColdStartTwin(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	rows := []leadstore.SiteImport{
		{Domain: "northwind.example", Country: "de", Rank: intp(10)},
		{Domain: "Contoso.Example ", Country: "", Rank: nil},
		{Domain: "fabrikam.example", Country: "at", Rank: intp(3)},
		{Domain: "northwind.example", Country: "de", Rank: intp(10)}, // repeat in the input
	}
	got, err := f.q.ImportSites(ctx, leadstore.OriginTranco, rows)
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if want := (leadstore.ImportTally{Added: 3, Known: 1}); got != want {
		t.Errorf("first import = %+v, want %+v", got, want)
	}
	got, err = f.q.ImportSites(ctx, leadstore.OriginTranco, rows)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if want := (leadstore.ImportTally{Added: 0, Known: 4}); got != want {
		t.Errorf("re-import = %+v, want %+v", got, want)
	}
	got, err = f.q.ImportSites(ctx, leadstore.OriginManual, nil)
	if err != nil || got != (leadstore.ImportTally{}) {
		t.Errorf("empty import = %+v, %v; want zero, nil", got, err)
	}
	// The domain is stored trimmed and lower case.
	siteID(t, "contoso.example")
	// The country is stored upper case.
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: "tailspin.example", Country: " gb "}}); err != nil {
		t.Fatalf("import gb: %v", err)
	}
	var country string
	if err := storetest.OwnerConn(t).QueryRow(ctx, "SELECT country FROM site WHERE domain = 'tailspin.example'").Scan(&country); err != nil || country != "GB" {
		t.Errorf("country = %q, %v; want GB", country, err)
	}
}

func TestImportSitesChunks(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	const n = 5_000*2 + 7 // three chunks
	rows := make([]leadstore.SiteImport, n)
	for i := range rows {
		rows[i] = leadstore.SiteImport{Domain: "s" + strconv.Itoa(i) + ".example", Rank: intp(i + 1)}
	}
	got, err := f.q.ImportSites(ctx, leadstore.OriginMajestic, rows)
	if err != nil || got != (leadstore.ImportTally{Added: n}) {
		t.Fatalf("import = %+v, %v; want %d added", got, err, n)
	}
	got, err = f.q.ImportSites(ctx, leadstore.OriginMajestic, rows)
	if err != nil || got != (leadstore.ImportTally{Known: n}) {
		t.Fatalf("re-import = %+v, %v; want %d known", got, err, n)
	}
}

func TestImportSitesRankFillKeepsOrigin(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{
		{Domain: "northwind.example"},
		{Domain: "contoso.example", Rank: intp(50)},
	}); err != nil {
		t.Fatalf("manual import: %v", err)
	}
	got, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{
		{Domain: "northwind.example", Country: "de", Rank: intp(7)},
		{Domain: "contoso.example", Rank: intp(2)},
	})
	if err != nil {
		t.Fatalf("tranco import: %v", err)
	}
	if want := (leadstore.ImportTally{Known: 2}); got != want {
		t.Errorf("tranco import = %+v, want %+v (a rank fill is known, not added)", got, want)
	}
	sites, err := f.q.DueSites(ctx, 0, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("due sites: %v", err)
	}
	if len(sites) != 2 {
		t.Fatalf("due sites = %d, want 2", len(sites))
	}
	byDomain := map[string]leadstore.Site{}
	for _, s := range sites {
		byDomain[s.Domain] = s
	}
	nw := byDomain["northwind.example"]
	if nw.Origin != leadstore.OriginManual || nw.ListRank == nil || *nw.ListRank != 7 || nw.Country != "" {
		t.Errorf("northwind = %+v, want origin manual, rank 7 filled, country kept ''", nw)
	}
	co := byDomain["contoso.example"]
	if co.ListRank == nil || *co.ListRank != 50 {
		t.Errorf("contoso rank = %v, want 50 kept", co.ListRank)
	}
}

func TestImportSitesRejectsBadInput(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, "crawl", []leadstore.SiteImport{{Domain: "northwind.example"}}); !errors.Is(err, leadstore.ErrUnknownOrigin) {
		t.Errorf("unknown origin: err = %v, want ErrUnknownOrigin", err)
	}
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: "  "}}); !errors.Is(err, leadstore.ErrEmptyDomain) {
		t.Errorf("empty domain: err = %v, want ErrEmptyDomain", err)
	}
}

func okProbe(fetchID int64, major int) leadstore.ProbeRow {
	return leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: "www.northwind.example", IsDrupal: major > 0, Major: major,
		ExactVersion: "", Ambiguous: "",
		Signals:   []leadstore.Signal{{Signal: "x-generator", Value: "Drupal", FetchID: fetchID}},
		FetchIDs:  []int64{fetchID},
		SiteName:  "Northwind",
		DerivedBy: "drupal@1",
	}
}

func TestDueSitesOrderAndWindow(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{
		{Domain: "norank-b.example"},
		{Domain: "rank20.example", Rank: intp(20)},
		{Domain: "norank-a.example"},
		{Domain: "rank5.example", Rank: intp(5)},
		{Domain: "probed.example", Rank: intp(1)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	fid := f.fetchRow(t, "https://probed.example/")
	if _, err := f.q.RecordProbe(ctx, siteID(t, "probed.example"), f.runID, okProbe(fid, 0)); err != nil {
		t.Fatalf("record probe: %v", err)
	}
	domains := func(now time.Time, limit int) []string {
		t.Helper()
		sites, err := f.q.DueSites(ctx, limit, 30*24*time.Hour, now)
		if err != nil {
			t.Fatalf("due sites: %v", err)
		}
		var out []string
		for _, s := range sites {
			out = append(out, s.Domain)
		}
		return out
	}
	now := time.Now()
	// Inside the window the probed site is not due; unranked sites come last, by id.
	if got, want := domains(now.Add(29*24*time.Hour), 0), []string{"rank5.example", "rank20.example", "norank-b.example", "norank-a.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inside window = %v, want %v", got, want)
	}
	// Past the window it is due again, first by rank.
	if got, want := domains(now.Add(31*24*time.Hour), 3), []string{"probed.example", "rank5.example", "rank20.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("past window, limit 3 = %v, want %v", got, want)
	}
}

func TestRecordProbeUpdatesPointers(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: "northwind.example"}}); err != nil {
		t.Fatalf("import: %v", err)
	}
	sid := siteID(t, "northwind.example")
	fid := f.fetchRow(t, "https://northwind.example/")
	first, err := f.q.RecordProbe(ctx, sid, f.runID, okProbe(fid, 9))
	if err != nil {
		t.Fatalf("record probe: %v", err)
	}
	second, err := f.q.RecordProbe(ctx, sid, f.runID, leadstore.ProbeRow{Outcome: leadstore.OutcomeTransportError, DerivedBy: "drupal@1"})
	if err != nil {
		t.Fatalf("record second probe: %v", err)
	}
	if second <= first {
		t.Fatalf("probe ids %d then %d", first, second)
	}
	owner := storetest.OwnerConn(t)
	var state string
	var last int64
	if err := owner.QueryRow(ctx, "SELECT state, last_probe_id FROM site WHERE id = $1", sid).Scan(&state, &last); err != nil {
		t.Fatalf("read site: %v", err)
	}
	if state != leadstore.SiteProbed || last != second {
		t.Errorf("site state %q last_probe_id %d, want probed %d", state, last, second)
	}
	var signals string
	var fetchIDs []int64
	if err := owner.QueryRow(ctx, "SELECT signals::text, fetch_ids FROM site_probe WHERE id = $1", first).Scan(&signals, &fetchIDs); err != nil {
		t.Fatalf("read probe: %v", err)
	}
	if want := `[{"value": "Drupal", "signal": "x-generator", "fetch_id": ` + strconv.FormatInt(fid, 10) + `}]`; signals != want {
		t.Errorf("signals = %s, want %s", signals, want)
	}
	if !reflect.DeepEqual(fetchIDs, []int64{fid}) {
		t.Errorf("fetch_ids = %v, want [%d]", fetchIDs, fid)
	}
	var emptySignals string
	if err := owner.QueryRow(ctx, "SELECT signals::text FROM site_probe WHERE id = $1", second).Scan(&emptySignals); err != nil || emptySignals != "[]" {
		t.Errorf("empty signals = %q, %v; want []", emptySignals, err)
	}
	if _, err := f.q.RecordProbe(ctx, sid, f.runID, leadstore.ProbeRow{Outcome: "maybe", DerivedBy: "drupal@1"}); err == nil {
		t.Error("unknown outcome: want an error")
	}
}

func TestInsertFetchHeader(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	fid := f.fetchRow(t, "https://northwind.example/")
	h := http.Header{}
	h.Set("X-Generator", "Drupal 10 (https://www.drupal.org)")
	h.Set("Content-Type", "text/html; charset=UTF-8")
	if err := f.q.InsertFetchHeader(ctx, fid, h); err != nil {
		t.Fatalf("insert header: %v", err)
	}
	h2 := http.Header{}
	h2.Set("Server", "other")
	if err := f.q.InsertFetchHeader(ctx, fid, h2); err != nil {
		t.Fatalf("insert header again: %v", err)
	}
	var got string
	if err := storetest.OwnerConn(t).QueryRow(ctx, "SELECT headers->'x-generator'->>0 FROM fetch_header WHERE fetch_id = $1", fid).Scan(&got); err != nil {
		t.Fatalf("read header: %v", err)
	}
	if got != "Drupal 10 (https://www.drupal.org)" {
		t.Errorf("x-generator = %q, want the first write kept", got)
	}
}

func TestProbeSummaryAndEvalRows(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{
		{Domain: "northwind.example"}, {Domain: "contoso.example"}, {Domain: "fabrikam.example"}, {Domain: "unprobed.example"},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	fid := f.fetchRow(t, "https://northwind.example/")
	record := func(domain string, p leadstore.ProbeRow) {
		t.Helper()
		if _, err := f.q.RecordProbe(ctx, siteID(t, domain), f.runID, p); err != nil {
			t.Fatalf("record %s: %v", domain, err)
		}
	}
	record("northwind.example", leadstore.ProbeRow{Outcome: leadstore.OutcomeTransportError, DerivedBy: "drupal@1"})
	record("northwind.example", okProbe(fid, 7)) // the latest
	record("contoso.example", okProbe(fid, 10))
	record("fabrikam.example", leadstore.ProbeRow{Outcome: leadstore.OutcomeBotCheck, DerivedBy: "drupal@1"})

	sum, err := f.q.ProbeSummary(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("probe summary: %v", err)
	}
	want := leadstore.ProbeCounts{
		Total:     4,
		Drupal:    2,
		ByOutcome: map[string]int{leadstore.OutcomeOK: 2, leadstore.OutcomeTransportError: 1, leadstore.OutcomeBotCheck: 1},
		ByMajor:   map[int]int{7: 1, 10: 1},
	}
	if !reflect.DeepEqual(sum, want) {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
	later, err := f.q.ProbeSummary(ctx, time.Now().Add(time.Hour))
	if err != nil || later.Total != 0 || len(later.ByOutcome) != 0 || len(later.ByMajor) != 0 {
		t.Errorf("summary since the future = %+v, %v; want empty", later, err)
	}

	rows, err := f.q.EvalRows(ctx, []string{"Northwind.example", "fabrikam.example", "unprobed.example", "missing.example"})
	if err != nil {
		t.Fatalf("eval rows: %v", err)
	}
	wantRows := []leadstore.EvalRow{
		{Domain: "fabrikam.example", Outcome: leadstore.OutcomeBotCheck, Signals: []leadstore.Signal{}},
		{Domain: "northwind.example", Outcome: leadstore.OutcomeOK, IsDrupal: true, Major: 7,
			Signals: []leadstore.Signal{{Signal: "x-generator", Value: "Drupal", FetchID: fid}}},
	}
	if !reflect.DeepEqual(rows, wantRows) {
		t.Errorf("eval rows = %+v, want %+v", rows, wantRows)
	}
}
