package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const harvestFixtures = "../../internal/boardlist/testdata/harvest"

// runHarvest calls runBoardsHarvest and returns its error and stdout.
func runHarvest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out := captureStdout(t)
	captureNotes(t)
	err := runBoardsHarvest(context.Background(), args)
	return out.String(), err
}

func harvestArgs(out string, extra ...string) []string {
	return append([]string{"--crawl", "CC-MAIN-2099-01", "--out", out, "--dry-run", "--fixtures", harvestFixtures,
		"--pattern", "boards.greenhouse.io/*", "--pattern", "jobs.lever.co/*", "--pattern", "jobs.ashbyhq.com/*"}, extra...)
}

func TestHarvestDryRun(t *testing.T) {
	noDB(t)
	out := filepath.Join(t.TempDir(), "boards.csv")
	stdout, err := runHarvest(t, harvestArgs(out, "--max-pages", "2")...)
	if err != nil {
		t.Fatalf("harvest: %v\n%s", err, stdout)
	}
	for _, want := range []string{
		"boards.greenhouse.io/*: pages 2/3 (capped by --max-pages 2), page errors 0, lines 6, boards 2, refused 2, skipped 0",
		"jobs.lever.co/*: pages 1/1, page errors 0, lines 3, boards 1, refused 1, skipped 0",
		"jobs.ashbyhq.com/*: pages 1/1, page errors 0, lines 3, boards 2, refused 1, skipped 0",
		"harvest: 5 unique boards (ashby 2, greenhouse 2, lever 1) written to " + out,
		"refused by reason: not a board page (embed, API or asset path) 4",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	got, err := os.ReadFile(out) // #nosec G304 -- the test's temp file
	if err != nil {
		t.Fatal(err)
	}
	want := "name,slug,url\n" +
		",demolabs,https://jobs.ashbyhq.com/demolabs\n" +
		",samplesoft,https://jobs.ashbyhq.com/samplesoft\n" +
		",examplecorp,https://job-boards.greenhouse.io/examplecorp\n" +
		",samplecorp,https://job-boards.greenhouse.io/samplecorp\n" +
		",samplesoft,https://jobs.lever.co/samplesoft\n"
	if string(got) != want {
		t.Errorf("csv:\n%s\nwant:\n%s", got, want)
	}

	// Without --force an existing file stays as it is; with it, it is rewritten.
	if err := os.WriteFile(out, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runHarvest(t, harvestArgs(out)...); !errors.Is(err, errUsage) {
		t.Errorf("existing out without --force: %v", err)
	}
	if b, _ := os.ReadFile(out); string(b) != "keep" { // #nosec G304 -- the test's temp file
		t.Errorf("existing out was changed: %q", b)
	}
	if _, err := runHarvest(t, harvestArgs(out, "--force")...); err != nil {
		t.Errorf("--force: %v", err)
	}
	if b, _ := os.ReadFile(out); !strings.HasPrefix(string(b), "name,slug,url\n") { // #nosec G304 -- the test's temp file
		t.Errorf("--force did not rewrite: %q", b)
	}
}

func TestHarvestUsage(t *testing.T) {
	noDB(t)
	out := filepath.Join(t.TempDir(), "x.csv")
	cases := [][]string{
		{"--out", out},
		{"--crawl", "CC-MAIN-25-08", "--out", out},
		{"--crawl", "CC-MAIN-2099-01"},
		{"--crawl", "CC-MAIN-2099-01", "--out", "-"},
		{"--crawl", "CC-MAIN-2099-01", "--out", out, "--pattern", "https://jobs.lever.co/*"},
		{"--crawl", "CC-MAIN-2099-01", "--out", out, "--pattern", "jobs.lever.co/* &x=1"},
		{"--crawl", "CC-MAIN-2099-01", "--out", out, "--max-pages", "0"},
		{"--crawl", "CC-MAIN-2099-01", "--out", out, "extra"},
		{"--crawl", "CC-MAIN-2099-01", "--out", out, "--dry-run"},
	}
	for _, args := range cases {
		if _, err := runHarvest(t, args...); !errors.Is(err, errUsage) {
			t.Errorf("%v: %v, want a usage error", args, err)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a usage error wrote the out file")
	}
}

func TestHarvestLiveOff(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "")
	out := filepath.Join(t.TempDir(), "x.csv")
	stdout, err := runHarvest(t, "--crawl", "CC-MAIN-2099-01", "--out", out)
	if err != nil {
		t.Fatal(err)
	}
	if want := "boards harvest: live fetching is off here (LEADHOUND_LIVE is off); nothing requested\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("live off wrote the out file")
	}
}

// TestHarvestPageErrorSkipped: against a local index, a page that answers 503 is reported
// and skipped, the others are read, every query asks for url lines only, and the pace holds.
func TestHarvestPageErrorSkipped(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "")
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		q := r.URL.Query()
		switch {
		case r.URL.Path != "/CC-MAIN-2099-01-index" || q.Get("url") != "jobs.lever.co/*" || q.Get("output") != "json" || q.Get("fl") != "url":
			http.Error(w, "bad query", http.StatusBadRequest)
		case q.Get("showNumPages") == "true":
			_, _ = fmt.Fprint(w, `{"pages": 3, "pageSize": 5, "blocks": 11}`)
		case q.Get("page") == "1":
			http.Error(w, "busy", http.StatusServiceUnavailable)
		default:
			_, _ = fmt.Fprintf(w, "{\"url\": \"https://jobs.lever.co/samplesoft%s\"}\n", q.Get("page")) // #nosec G705 -- a test server echoing a page number
		}
	}))
	t.Cleanup(srv.Close)
	savedBase, savedInterval, savedJitter := ccIndexBase, harvestInterval, harvestJitter
	ccIndexBase, harvestInterval, harvestJitter = srv.URL, 50*time.Millisecond, 0
	t.Cleanup(func() { ccIndexBase, harvestInterval, harvestJitter = savedBase, savedInterval, savedJitter })

	out := filepath.Join(t.TempDir(), "x.csv")
	start := time.Now()
	stdout, err := runHarvest(t, "--crawl", "CC-MAIN-2099-01", "--out", out, "--pattern", "jobs.lever.co/*")
	if err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Errorf("4 requests took %v; the pace was not held", took)
	}
	for _, want := range []string{
		"jobs.lever.co/*: page 1 failed (status 503); skipped",
		"jobs.lever.co/*: pages 2/3, page errors 1, lines 2, boards 2, refused 0, skipped 0",
		"harvest: 2 unique boards (lever 2)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if len(queries) != 4 {
		t.Errorf("queries %v, want 4", queries)
	}
}

// localIndex points the harvest at srv with a fast pace for the test.
func localIndex(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("LEADHOUND_LIVE", "")
	savedBase, savedInterval, savedJitter := ccIndexBase, harvestInterval, harvestJitter
	ccIndexBase, harvestInterval, harvestJitter = srv.URL, time.Millisecond, 0
	t.Cleanup(func() { ccIndexBase, harvestInterval, harvestJitter = savedBase, savedInterval, savedJitter })
}

// noTempLeft fails if a harvest temp file is left in dir.
func noTempLeft(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}
}

// TestHarvestBreakerStopReplacesWithWhatWasRead: with --force, a run the breaker stops
// still puts what it read in place of the old file, says it stopped early and exits 1.
func TestHarvestBreakerStopReplacesWithWhatWasRead(t *testing.T) {
	noDB(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Get("url") != "jobs.lever.co/*":
			http.Error(w, "busy", http.StatusServiceUnavailable)
		case q.Get("showNumPages") == "true":
			_, _ = fmt.Fprint(w, `{"pages": 1}`)
		default:
			_, _ = fmt.Fprint(w, `{"url": "https://jobs.lever.co/samplesoft"}`+"\n")
		}
	}))
	t.Cleanup(srv.Close)
	localIndex(t, srv)
	dir := t.TempDir()
	out := filepath.Join(dir, "boards.csv")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--crawl", "CC-MAIN-2099-01", "--out", out, "--force", "--pattern", "jobs.lever.co/*"}
	for range 4 {
		args = append(args, "--pattern", "jobs.ashbyhq.com/*")
	}
	stdout, err := runHarvest(t, args...)
	if err == nil || !strings.Contains(err.Error(), "stopped early") {
		t.Fatalf("err %v, want a stopped-early error\n%s", err, stdout)
	}
	got, _ := os.ReadFile(out) // #nosec G304 -- the test's temp file
	if want := "name,slug,url\n,samplesoft,https://jobs.lever.co/samplesoft\n"; string(got) != want {
		t.Errorf("csv %q, want %q", got, want)
	}
	noTempLeft(t, dir)
}

// TestHarvestContextEndLeavesFile: when the context ends mid-run, an existing file stays
// as it was and no temp file is left, with or without --force.
func TestHarvestContextEndLeavesFile(t *testing.T) {
	noDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("showNumPages") == "true" {
			_, _ = fmt.Fprint(w, `{"pages": 2}`)
			return
		}
		cancel()
		_, _ = fmt.Fprint(w, `{"url": "https://jobs.lever.co/samplesoft"}`+"\n")
	}))
	t.Cleanup(srv.Close)
	localIndex(t, srv)
	dir := t.TempDir()
	out := filepath.Join(dir, "boards.csv")
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	captureStdout(t)
	captureNotes(t)
	err := runBoardsHarvest(ctx, []string{"--crawl", "CC-MAIN-2099-01", "--out", out, "--force", "--pattern", "jobs.lever.co/*"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "old" { // #nosec G304 -- the test's temp file
		t.Errorf("existing file changed to %q", got)
	}
	noTempLeft(t, dir)

	// The write itself gives way to an ended context too.
	for _, force := range []bool{false, true} {
		target := filepath.Join(dir, fmt.Sprintf("w%v.csv", force))
		if err := writeHarvest(ctx, target, nil, force); !errors.Is(err, context.Canceled) {
			t.Errorf("force %v: err %v, want context.Canceled", force, err)
		}
		if _, err := os.Stat(target); err == nil {
			t.Errorf("force %v: wrote %s after the context ended", force, target)
		}
	}
	noTempLeft(t, dir)
}

// TestWriteHarvestExclusive: without force an existing file is refused and kept; a new
// one is written whole.
func TestWriteHarvestExclusive(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "boards.csv")
	if err := writeHarvest(context.Background(), out, nil, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "name,slug,url\n" { // #nosec G304 -- the test's temp file
		t.Errorf("new file %q", got)
	}
	if err := os.WriteFile(out, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeHarvest(context.Background(), out, nil, false); !errors.Is(err, errUsage) {
		t.Errorf("existing without force: %v", err)
	}
	if got, _ := os.ReadFile(out); string(got) != "old" { // #nosec G304 -- the test's temp file
		t.Errorf("existing file changed to %q", got)
	}
	noTempLeft(t, dir)
}

// TestHarvestPatternLine: a pattern's line says "capped" only when the index had more pages
// than --max-pages; a pattern the breaker stopped says it stopped and why, and every later
// pattern gets a "not run" line. The CSV still holds what was read.
func TestHarvestPatternLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pages    int    // pages the index says jobs.lever.co/* has
		status   int    // status of every jobs.lever.co/* page; 429 opens the breaker
		maxPages string // --max-pages
		wantErr  bool
		want     []string
		notWant  []string
	}{
		{
			name: "capped", pages: 3, status: http.StatusOK, maxPages: "2",
			want: []string{
				"jobs.lever.co/*: pages 2/3 (capped by --max-pages 2), page errors 0",
				"jobs.ashbyhq.com/*: pages 1/1, page errors 0",
			},
			notWant: []string{"stopped", "not run"},
		},
		{
			name: "stopped on breaker", pages: 1, status: http.StatusTooManyRequests, maxPages: "50", wantErr: true,
			want: []string{
				"jobs.lever.co/*: pages 0/1 (stopped: throttled), page errors 0",
				"jobs.ashbyhq.com/*: not run (the run stopped)\n",
				"harvest: 0 unique boards",
			},
			notWant: []string{"capped", "jobs.ashbyhq.com/*: pages"},
		},
		{
			name: "normal", pages: 1, status: http.StatusOK, maxPages: "50",
			want: []string{
				"jobs.lever.co/*: pages 1/1, page errors 0, lines 1, boards 1, refused 0, skipped 0\n",
				"jobs.ashbyhq.com/*: pages 1/1, page errors 0, lines 1, boards 1, refused 0, skipped 0\n",
			},
			notWant: []string{"capped", "stopped", "not run"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noDB(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				lever := q.Get("url") == "jobs.lever.co/*"
				switch {
				case q.Get("showNumPages") == "true" && lever:
					_, _ = fmt.Fprintf(w, `{"pages": %d}`, tc.pages)
				case q.Get("showNumPages") == "true":
					_, _ = fmt.Fprint(w, `{"pages": 1}`)
				case lever && tc.status != http.StatusOK:
					w.Header().Set("Retry-After", "0")
					http.Error(w, "slow down", tc.status)
				case lever:
					_, _ = fmt.Fprintf(w, "{\"url\": \"https://jobs.lever.co/samplesoft%s\"}\n", q.Get("page")) // #nosec G705 -- a test server echoing a page number
				default:
					_, _ = fmt.Fprint(w, `{"url": "https://jobs.ashbyhq.com/demolabs"}`+"\n")
				}
			}))
			t.Cleanup(srv.Close)
			localIndex(t, srv)
			out := filepath.Join(t.TempDir(), "boards.csv")
			stdout, err := runHarvest(t, "--crawl", "CC-MAIN-2099-01", "--out", out, "--max-pages", tc.maxPages,
				"--pattern", "jobs.lever.co/*", "--pattern", "jobs.ashbyhq.com/*")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v, wantErr %v\n%s", err, tc.wantErr, stdout)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(stdout, nw) {
					t.Errorf("stdout has %q:\n%s", nw, stdout)
				}
			}
			if _, err := os.Stat(out); err != nil {
				t.Errorf("CSV not written: %v", err)
			}
		})
	}
}
