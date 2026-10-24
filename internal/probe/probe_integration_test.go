//go:build integration

package probe

import (
	"context"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

var fixtureDomains = []string{
	"northwind.example", "contoso.example", "fabrikam.example", "tailspin.example", "wingtip.example", "adatum.example",
}

// probeDB runs one fixture probe of every due site on the test database and returns the
// store, the summary and the run's id.
func probeDB(t *testing.T, s *store.Store) (Summary, int64) {
	t.Helper()
	ctx := context.Background()
	lq := leadstore.NewQueries(s.Pool())
	due, err := lq.DueSites(ctx, 0, 30*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runID, err := s.Q().StartRun(ctx, "leads-sites-probe", "sites-fixture")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.NewFetchRecorder(s.Q(), runID, "sites-fixture").SizeOnly(fetch.KindSite, fetch.KindRobots)
	sum, err := Run(ctx, Deps{NewClient: fixtureFactory(t), Recorder: rec, Save: SaveTx(s.Pool(), runID)}, due, Options{Concurrency: 4})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return sum, runID
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	leadstoretest.Reset(t)
	s, err := store.Open(context.Background(), storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rows := make([]leadstore.SiteImport, len(fixtureDomains))
	for i, d := range fixtureDomains {
		rows[i] = leadstore.SiteImport{Domain: d}
	}
	if _, err := leadstore.NewQueries(s.Pool()).ImportSites(context.Background(), leadstore.OriginManual, rows); err != nil {
		t.Fatal(err)
	}
	return s
}

func count(t *testing.T, s *store.Store, sql string) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestProbeStoresWhatItSaw(t *testing.T) {
	s := openStore(t)
	sum, _ := probeDB(t, s)
	if sum.Sites != len(fixtureDomains) || sum.Drupal != 3 {
		t.Fatalf("summary %+v", sum)
	}

	// The invariant sweep: every kept site or robots body is cited by a Drupal probe.
	if n := count(t, s, `SELECT count(*) FROM http_fetch f
		WHERE f.kind IN ('site', 'robots') AND length(f.body) > 0
		  AND NOT EXISTS (SELECT 1 FROM site_probe p WHERE f.id = ANY(p.fetch_ids) AND p.is_drupal)`); n != 0 {
		t.Errorf("%d bodies kept outside a Drupal probe", n)
	}
	if n := count(t, s, `SELECT count(*) FROM http_fetch WHERE kind = 'site' AND length(body) > 0`); n != 3 {
		t.Errorf("%d site bodies kept, want 3 (the Drupal homepages)", n)
	}
	// Every signal cites a fetch its probe lists.
	if n := count(t, s, `SELECT count(*) FROM site_probe p, jsonb_array_elements(p.signals) s
		WHERE NOT ((s->>'fetch_id')::bigint = ANY(p.fetch_ids))`); n != 0 {
		t.Errorf("%d signals cite a fetch outside their probe's fetch_ids", n)
	}
	if n := count(t, s, `SELECT count(*) FROM site_probe p, jsonb_array_elements(p.signals) s`); n == 0 {
		t.Errorf("no signals stored")
	}
	// A disallowed robots.txt means no site fetch for that domain.
	if n := count(t, s, `SELECT count(*) FROM http_fetch WHERE kind = 'site' AND url LIKE 'https://adatum.example/%'`); n != 0 {
		t.Errorf("robots-disallowed site: %d site fetches", n)
	}
	if n := count(t, s, `SELECT count(*) FROM site_probe p JOIN site s ON s.id = p.site_id
		WHERE s.domain = 'adatum.example' AND p.outcome = 'robots_disallowed'`); n != 1 {
		t.Errorf("adatum probes: %d robots_disallowed, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM site_probe p JOIN site s ON s.id = p.site_id
		WHERE s.domain = 'contoso.example' AND p.is_drupal AND p.major = 10 AND p.exact_version = '10.6.14'`); n != 1 {
		t.Errorf("contoso: no D10 probe with its exact version")
	}
	if n := count(t, s, `SELECT count(*) FROM fetch_header`); n == 0 {
		t.Errorf("no fetch headers kept")
	}
	if n := count(t, s, `SELECT count(*) FROM site WHERE state = 'probed' AND last_probe_id IS NOT NULL`); n != len(fixtureDomains) {
		t.Errorf("%d sites point at a probe, want %d", n, len(fixtureDomains))
	}
}

func TestProbeColdStartTwin(t *testing.T) {
	s := openStore(t)
	first, _ := probeDB(t, s)
	if first.Sites != len(fixtureDomains) {
		t.Fatalf("first run probed %d", first.Sites)
	}
	second, _ := probeDB(t, s)
	if second.Sites != 0 || second.Requests != 0 {
		t.Errorf("second run within 30 days: %d sites, %d requests, want none", second.Sites, second.Requests)
	}
	if n := count(t, s, `SELECT count(*) FROM site_probe`); n != len(fixtureDomains) {
		t.Errorf("%d probes after two runs, want %d", n, len(fixtureDomains))
	}
}
