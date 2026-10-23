//go:build integration

package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

type fetchRow struct {
	RunID       int64
	Source      string
	Kind        string
	URL         string
	FinalURL    string
	Status      int
	Body        []byte
	ContentType string
	SHA         []byte
	Err         string
}

func readFetch(t *testing.T, id int64) fetchRow {
	t.Helper()
	var r fetchRow
	err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT run_id, source, kind, url, final_url, status, body, content_type, body_sha256, error
		 FROM http_fetch WHERE id = $1`, id).
		Scan(&r.RunID, &r.Source, &r.Kind, &r.URL, &r.FinalURL, &r.Status, &r.Body, &r.ContentType, &r.SHA, &r.Err)
	if err != nil {
		t.Fatalf("read http_fetch %d: %v", id, err)
	}
	return r
}

func TestFetchRecorderWritesRows(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	runID, err := s.Q().StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	rec := store.NewFetchRecorder(s.Q(), runID, "linkedin")
	if rec.LastID() != 0 {
		t.Fatalf("LastID before any record = %d, want 0", rec.LastID())
	}

	body := []byte("<html>\x00\xff not utf-8</html>")
	sum := sha256.Sum256(body)
	at := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	const pageURL = "https://jobs.example/search?start=0"

	// Same final URL as requested: stored as ''.
	if err := rec.Record(ctx, fetch.Record{
		Kind: fetch.KindSearch, URL: pageURL, FinalURL: pageURL, Status: 200,
		Body: body, ContentType: "text/html", SHA256: sum[:], FetchedAt: at,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	first := rec.LastID()
	if first == 0 {
		t.Fatal("LastID after a record = 0")
	}
	if id, ok := rec.LastIDFor(pageURL); !ok || id != first {
		t.Fatalf("LastIDFor = %d, %v; want %d, true", id, ok, first)
	}
	got := readFetch(t, first)
	if got.RunID != runID || got.Source != "linkedin" || got.Kind != "search" || got.URL != pageURL ||
		got.FinalURL != "" || got.Status != 200 || got.ContentType != "text/html" || got.Err != "" {
		t.Fatalf("row = %+v", got)
	}
	if !bytes.Equal(got.Body, body) {
		t.Fatalf("body = %q, want the bytes exactly as they arrived", got.Body)
	}
	if !bytes.Equal(got.SHA, sum[:]) {
		t.Fatalf("body_sha256 = %x, want %x", got.SHA, sum)
	}

	// A redirect keeps its final URL; a missing hash on a response is computed.
	const detailURL = "https://jobs.example/job/1"
	if err := rec.Record(ctx, fetch.Record{
		Kind: fetch.KindDetail, URL: detailURL, FinalURL: "https://jobs.example/login", Status: 200, Body: []byte("x"),
	}); err != nil {
		t.Fatalf("Record redirect: %v", err)
	}
	got = readFetch(t, rec.LastID())
	xsum := sha256.Sum256([]byte("x"))
	if got.FinalURL != "https://jobs.example/login" || !bytes.Equal(got.SHA, xsum[:]) {
		t.Fatalf("redirect row = %+v", got)
	}

	// A transport error: status 0, empty body, no hash, the error text kept.
	if err := rec.Record(ctx, fetch.Record{Kind: fetch.KindDetail, URL: detailURL, Err: "connection reset"}); err != nil {
		t.Fatalf("Record transport error: %v", err)
	}
	third := rec.LastID()
	got = readFetch(t, third)
	if got.Status != 0 || len(got.Body) != 0 || got.SHA != nil || got.Err != "connection reset" {
		t.Fatalf("transport error row = %+v", got)
	}
	if id, _ := rec.LastIDFor(detailURL); id != third {
		t.Fatalf("LastIDFor after a second fetch of the URL = %d, want the latest %d", id, third)
	}
	if id, _ := rec.LastIDFor(pageURL); id != first {
		t.Fatalf("LastIDFor the first URL = %d, want %d", id, first)
	}
	if _, ok := rec.LastIDFor("https://jobs.example/never"); ok {
		t.Fatal("LastIDFor an unseen URL reported ok")
	}

	// An unknown kind is refused by the CHECK and returned as an error.
	if err := rec.Record(ctx, fetch.Record{Kind: "bogus", URL: pageURL, Status: 200}); err == nil {
		t.Fatal("Record with an unknown kind succeeded")
	}
}

// TestFetchRecorderThroughClient drives a real fetch.Client over the fixture transport with
// the store recorder: every response, 404 included, lands as a row with its hash.
func TestFetchRecorderThroughClient(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	runID, err := s.Q().StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "page.html"), "<p>page</p>")
	writeFile(t, filepath.Join(dir, "manifest.json"), `{"*/page*": "page.html"}`)
	ft, err := fetch.NewFixtureTransport(dir)
	if err != nil {
		t.Fatalf("NewFixtureTransport: %v", err)
	}
	rec := store.NewFetchRecorder(s.Q(), runID, "linkedin")
	c := fetch.New(fetch.WithFixtures(ft), fetch.WithRecorder(rec),
		fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)), fetch.WithJitter(0))

	for _, u := range []string{"https://jobs.example/page?x=1", "https://jobs.example/missing"} {
		if _, err := c.Get(ctx, fetch.KindSearch, u); err != nil {
			t.Fatalf("Get %s: %v", u, err)
		}
	}
	var n, hashed int
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT count(*), count(body_sha256) FROM http_fetch WHERE run_id = $1`, runID).Scan(&n, &hashed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 || hashed != 2 || ft.Hits() != 2 {
		t.Fatalf("rows = %d, hashed = %d, hits = %d; want 2, 2, 2", n, hashed, ft.Hits())
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
