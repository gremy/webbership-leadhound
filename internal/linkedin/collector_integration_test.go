//go:build integration

package linkedin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const testdata = "testdata"

// ---- helpers ----

// openStore empties the test database and opens a Store as the app role.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	storetest.ResetDB(t)
	s, err := store.Open(context.Background(), storetest.AppDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping integration test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// fixtureDir copies testdata into a temp dir with the given manifest.
func fixtureDir(t *testing.T, manifest map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, file := range manifest {
		b, err := os.ReadFile(filepath.Join(testdata, file)) // #nosec G304 -- the test's own testdata
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fetch.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fullManifest is the shipped manifest plus details for the three cards it leaves at 404,
// so every card gets an observation.
func fullManifest(t *testing.T) map[string]string {
	t.Helper()
	m := shippedManifest(t)
	m["*jobPosting/1000000003"] = "detail_offsite.html"
	m["*jobPosting/1000000009"] = "detail_onsite.html"
	m["*jobPosting/1000000010"] = "detail_simple.html"
	return m
}

func shippedManifest(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testdata, fetch.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func fixtures(t *testing.T, dir string) *fetch.FixtureTransport {
	t.Helper()
	ft, err := fetch.NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ft
}

// guard lets a request through only to a fixture transport, an in-memory stub, or a
// loopback host; anything else fails the test.
type guard struct {
	t    *testing.T
	next http.RoundTripper
}

func (g guard) RoundTrip(r *http.Request) (*http.Response, error) {
	switch g.next.(type) {
	case *fetch.FixtureTransport, *stubTransport, errTransport:
		return g.next.RoundTrip(r)
	}
	if ip := net.ParseIP(r.URL.Hostname()); ip == nil || !ip.IsLoopback() {
		g.t.Errorf("guard: request to %s is neither a fixture nor loopback", r.URL.Host)
		return nil, http.ErrNotSupported
	}
	return g.next.RoundTrip(r)
}

// stubTransport answers in memory with fn; it never dials.
type stubTransport struct {
	mu   sync.Mutex
	hits int
	fn   func(r *http.Request) (int, string)
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.hits++
	s.mu.Unlock()
	status, body := s.fn(r)
	h := http.Header{}
	if status == http.StatusTooManyRequests {
		h.Set("Retry-After", "0")
	}
	return &http.Response{
		StatusCode: status, Header: h, Request: r,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}, nil
}

func (s *stubTransport) Hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

type nopClock struct{}

func (nopClock) Now() time.Time                             { return time.Now() }
func (nopClock) Sleep(context.Context, time.Duration) error { return nil }

var testBudgets = fetch.Budgets{fetch.KindSearch: 60, fetch.KindDetail: 40, fetch.KindCanary: 2}

// newClientFunc builds the run's client over rt. Fixtures go in through WithFixtures, as
// the dry run does; an in-memory stub passes the live gate as LiveAll, since it never dials.
// extra options come last, so they win.
func newClientFunc(t *testing.T, rt http.RoundTripper, extra ...fetch.Option) func(fetch.Recorder) *fetch.Client {
	return func(rec fetch.Recorder) *fetch.Client {
		opts := []fetch.Option{
			fetch.WithRecorder(rec),
			fetch.WithProfile(fetch.LinkedInJobs),
			fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
			fetch.WithJitter(0),
			fetch.WithClock(nopClock{}),
			fetch.WithBreaker(fetch.NewBreaker(testBudgets)),
		}
		if ft, ok := rt.(*fetch.FixtureTransport); ok {
			opts = append(opts, fetch.WithFixtures(ft))
		} else {
			opts = append(opts, fetch.WithTransport(guard{t: t, next: rt}), fetch.WithLiveGate(config.LiveAll))
		}
		return fetch.New(append(opts, extra...)...)
	}
}

var bucharest = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		panic(err)
	}
	return loc
}()

func collector(t *testing.T, s *store.Store, rt http.RoundTripper, now func() time.Time, logs *bytes.Buffer) *linkedin.Collector {
	return &linkedin.Collector{
		Store:     s,
		NewClient: newClientFunc(t, rt),
		Now:       now,
		Loc:       bucharest,
		Log:       slog.New(slog.NewTextHandler(logs, nil)),
		Gates:     gates.CandidateGates{Languages: []string{"en"}},
	}
}

var demo = linkedin.Matrix{Keywords: []string{"go engineer", "platform engineer"}, Locations: []string{"European Union"}, MaxPages: 2}

func count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func latestRun(t *testing.T, command string) store.Run {
	t.Helper()
	var r store.Run
	err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT id, command, source, requests, failures, stop_reason, note, finished_at IS NOT NULL
		 FROM run WHERE command = $1 ORDER BY id DESC LIMIT 1`, command).
		Scan(&r.ID, &r.Command, &r.Source, &r.Requests, &r.Failures, &r.StopReason, &r.Note, new(bool))
	if err != nil {
		t.Fatalf("latest run: %v", err)
	}
	return r
}

func requestedURLs(t *testing.T, kind string) []string {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT url FROM http_fetch WHERE kind = $1 ORDER BY id`, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

// ---- tests ----

func TestCollectorColdStart(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ft := fixtures(t, testdata)
	var logs bytes.Buffer
	res, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "" || !res.ColdStart || res.Paused {
		t.Fatalf("result = %+v, want a clean cold start", res)
	}
	if got := count(t, `SELECT count(*) FROM job`); got != 10 {
		t.Errorf("jobs = %d, want 10", got)
	}
	if got := count(t, `SELECT count(*) FROM company`); got < 1 {
		t.Errorf("companies = %d, want some", got)
	}
	if got := count(t, `SELECT count(*) FROM company WHERE linkedin_slug <> ''`); got < 1 {
		t.Errorf("companies with a slug = %d, want some", got)
	}
	if got := count(t, `SELECT count(*) FROM job_observation WHERE derived_by = 'linkedin-parser@1'`); got != 7 {
		t.Errorf("observations = %d, want 7 (one per detail fixture)", got)
	}
	if got := count(t, `SELECT count(*) FROM status_event WHERE status = 'new' AND actor = 'system'`); got != 7 {
		t.Errorf("new events = %d, want 7", got)
	}
	// Every observation points at the detail fetch it came from.
	if got := count(t, `SELECT count(*) FROM job_observation o JOIN http_fetch f ON f.id = o.fetch_id
		JOIN job j ON j.id = o.job_id WHERE f.kind = 'detail' AND f.url LIKE '%/' || j.source_job_id`); got != 7 {
		t.Errorf("observations linked to their own detail fetch = %d, want 7", got)
	}
	// Every observation has its gate results, at least the language gate.
	if got := count(t, `SELECT count(*) FROM job_observation o WHERE NOT EXISTS (
		SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.job_id = o.job_id AND g.gate = 'language')`); got != 0 {
		t.Errorf("observations without a language gate row = %d, want 0", got)
	}
	// An English posting runs all four gates; one in another language stops at language.
	if got := count(t, `SELECT count(*) FROM job_observation o WHERE o.lang = 'en' AND
		(SELECT count(DISTINCT gate) FROM gate_result g WHERE g.observation_id = o.id AND g.gate <> 'rule') <> 4`); got != 0 {
		t.Errorf("English observations without all four gates = %d, want 0", got)
	}
	if got := count(t, `SELECT count(*) FROM job_observation WHERE lang = 'en'`); got < 1 {
		t.Errorf("English observations = %d, want some", got)
	}
	if got := count(t, `SELECT count(*) FROM job_observation o WHERE o.lang NOT IN ('en', '') AND
		EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.gate IN ('location', 'workplace', 'work_rights'))`); got != 0 {
		t.Errorf("non-English observations with English-pattern gates = %d, want 0", got)
	}
	assertGuardCountsOnly(t)
	// The plain-text description has no markup.
	if got := count(t, `SELECT count(*) FROM job_observation WHERE description_clean = '' OR description_clean LIKE '%<%'`); got != 0 {
		t.Errorf("observations with an empty or tagged description = %d, want 0", got)
	}
	for _, id := range []string{"1000000003", "1000000009", "1000000010"} {
		if got := count(t, `SELECT count(*) FROM job j WHERE j.source_job_id = $1
			AND NOT EXISTS (SELECT 1 FROM job_observation o WHERE o.job_id = j.id)`, id); got != 1 {
			t.Errorf("job %s: want a job row with no observation", id)
		}
	}
	if n := strings.Count(logs.String(), "detail missing"); n != 3 {
		t.Errorf("logged %d \"detail missing\", want 3:\n%s", n, logs.String())
	}

	searches := requestedURLs(t, "search")
	// Two queries, each page 1 (10 cards) then page 2 (empty, stops).
	if len(searches) != 4 {
		t.Fatalf("search requests = %d (%v), want 4", len(searches), searches)
	}
	for _, u := range searches {
		if !strings.Contains(u, "f_TPR=r604800") {
			t.Errorf("cold start search %s lacks f_TPR=r604800", u)
		}
	}
	if !strings.Contains(searches[1], "start=10") {
		t.Errorf("second request %s, want page 2 (start=10)", searches[1])
	}
	// Details are fetched once per new ID across both queries.
	if got := ft.HitsFor("jobPosting"); got != 10 {
		t.Errorf("detail requests = %d, want 10 (deduplicated across queries)", got)
	}
	run := latestRun(t, "collect")
	if run.Source != "linkedin" || run.Requests != 14 || run.StopReason != "" {
		t.Errorf("run = %+v, want linkedin, 14 requests, no stop", run)
	}

	// A second run over the same pages: the three missing details are retried, nothing else.
	before := ft.HitsFor("jobPosting")
	res2, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res2.ColdStart || res2.Observations != 0 {
		t.Errorf("second result = %+v, want steady state with no new observations", res2)
	}
	if got := ft.HitsFor("jobPosting") - before; got != 3 {
		t.Errorf("second run detail requests = %d, want 3 (the 404s retried)", got)
	}
}

func TestCollectorSteadyState(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ft := fixtures(t, fixtureDir(t, fullManifest(t)))
	var logs bytes.Buffer
	if _, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := count(t, `SELECT count(*) FROM job_observation`); got != 10 {
		t.Fatalf("observations after cold start = %d, want 10", got)
	}
	var lastSeen time.Time
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT max(last_seen_at) FROM job`).Scan(&lastSeen); err != nil {
		t.Fatal(err)
	}
	searchesBefore := len(requestedURLs(t, "search"))
	detailBefore := ft.HitsFor("jobPosting")

	res, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.ColdStart || res.StopReason != "" {
		t.Fatalf("result = %+v, want a clean steady-state run", res)
	}
	if got := count(t, `SELECT count(*) FROM job_observation`); got != 10 {
		t.Errorf("observations = %d, want still 10", got)
	}
	if got := count(t, `SELECT count(*) FROM status_event`); got != 10 {
		t.Errorf("status events = %d, want still 10", got)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE last_seen_at > $1`, lastSeen); got != 10 {
		t.Errorf("jobs with last_seen_at bumped = %d, want 10", got)
	}
	if got := ft.HitsFor("jobPosting") - detailBefore; got != 0 {
		t.Errorf("detail requests in steady state = %d, want 0", got)
	}
	searches := requestedURLs(t, "search")[searchesBefore:]
	// Page 1 only: page 1 was full but every card had been seen.
	if len(searches) != 2 {
		t.Fatalf("steady searches = %v, want page 1 of each query", searches)
	}
	for _, u := range searches {
		if !strings.Contains(u, "f_TPR=r86400") || !strings.Contains(u, "start=0") {
			t.Errorf("steady search %s, want f_TPR=r86400 and start=0", u)
		}
	}
}

func TestCollectorSteadyStateFetchesPage2WhenPage1AllNew(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	// A previous clean run makes this a steady-state run, with no job seen yet.
	q := s.Q()
	id, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.FinishRun(ctx, id, store.RunResult{Requests: 1}); err != nil {
		t.Fatal(err)
	}
	ft := fixtures(t, testdata)
	var logs bytes.Buffer
	m := linkedin.Matrix{Keywords: []string{"go engineer"}, Locations: []string{"European Union"}, MaxPages: 5}
	if _, err := collector(t, s, ft, time.Now, &logs).Run(ctx, m); err != nil {
		t.Fatal(err)
	}
	searches := requestedURLs(t, "search")
	if len(searches) != 2 || !strings.Contains(searches[1], "start=10") {
		t.Fatalf("searches = %v, want page 1 then page 2", searches)
	}
}

func TestCollectorThrottledPausesForTheDay(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	throttle := &stubTransport{fn: func(*http.Request) (int, string) { return http.StatusTooManyRequests, "" }}
	var logs bytes.Buffer
	res, err := collector(t, s, throttle, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "throttled" {
		t.Fatalf("stop = %q, want throttled", res.StopReason)
	}
	if throttle.Hits() != 2 {
		t.Errorf("requests = %d, want 2 (a 429 and its one retry)", throttle.Hits())
	}
	if run := latestRun(t, "collect"); run.StopReason != "throttled" || run.Requests != 2 {
		t.Errorf("run = %+v, want throttled with 2 requests", run)
	}

	// The same day: nothing is requested and the run says it is paused.
	ft := fixtures(t, testdata)
	res, err = collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("paused Run: %v", err)
	}
	if !res.Paused || ft.Hits() != 0 {
		t.Fatalf("result = %+v with %d requests, want paused and none", res, ft.Hits())
	}
	run := latestRun(t, "collect")
	if run.Note != "paused today: throttled" || run.Requests != 0 {
		t.Errorf("paused run = %+v", run)
	}
	if got := count(t, `SELECT count(*) FROM run WHERE command = 'collect'`); got != 2 {
		t.Errorf("runs = %d, want 2", got)
	}

	// The next day it runs.
	tomorrow := func() time.Time { return time.Now().Add(24 * time.Hour) }
	res, err = collector(t, s, ft, tomorrow, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("next-day Run: %v", err)
	}
	if res.Paused || res.StopReason != "" || ft.Hits() == 0 {
		t.Fatalf("next-day result = %+v with %d requests, want a full run", res, ft.Hits())
	}
}

func TestCollectorParserBroke(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	m := shippedManifest(t)
	m["*seeMoreJobPostings/search?*&start=0*"] = "search_broken.html"
	ft := fixtures(t, fixtureDir(t, m))
	var logs bytes.Buffer
	res, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "parser_broke" {
		t.Fatalf("stop = %q, want parser_broke", res.StopReason)
	}
	if run := latestRun(t, "collect"); run.StopReason != "parser_broke" {
		t.Errorf("run stop = %q", run.StopReason)
	}
	for _, table := range []string{"job", "company", "job_observation", "status_event"} {
		if got := count(t, `SELECT count(*) FROM `+table); got != 0 {
			t.Errorf("%s rows = %d, want 0", table, got)
		}
	}
	if ft.Hits() != 1 {
		t.Errorf("requests = %d, want 1 (stopped at the broken page)", ft.Hits())
	}
}

// pageStub answers search pages from pages (a URL substring → a testdata file name, or
// the body itself when it names no file), every detail page with a 404, and anything else
// with a test error.
func pageStub(t *testing.T, pages map[string]string) *stubTransport {
	t.Helper()
	return &stubTransport{fn: func(r *http.Request) (int, string) {
		u := r.URL.String()
		if strings.Contains(u, "/jobPosting/") {
			return http.StatusNotFound, ""
		}
		for part, page := range pages {
			if !strings.Contains(u, part) {
				continue
			}
			if !strings.HasSuffix(page, ".html") {
				return http.StatusOK, page
			}
			b, err := os.ReadFile(filepath.Join(testdata, page)) // #nosec G304 -- the test's own testdata
			if err != nil {
				t.Errorf("read %s: %v", page, err)
			}
			return http.StatusOK, string(b)
		}
		t.Errorf("unexpected request %s", u)
		return http.StatusNotFound, ""
	}}
}

// endOfResults is what LinkedIn answers, with a 200, for a search page past the last result.
const endOfResults = "<!DOCTYPE html>\n\n<!---->  "

// The near-empty page past the last result ends that query's pages: no jobs, no stop, and
// the run goes on to the next query and the details.
func TestCollectorEndOfResultsPage(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stub := pageStub(t, map[string]string{"&start=0": "search_link_card.html", "&start=10": endOfResults})
	var logs bytes.Buffer
	res, err := collector(t, s, stub, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "" || res.Searches != 4 || res.NewIDs != 10 {
		t.Fatalf("result = %+v, want no stop, 4 searches, 10 new", res)
	}
	if got := count(t, `SELECT count(*) FROM job`); got != 10 {
		t.Errorf("jobs = %d, want 10", got)
	}
	if got := len(requestedURLs(t, "detail")); got != 10 {
		t.Errorf("detail requests = %d, want 10", got)
	}
	if run := latestRun(t, "collect"); run.StopReason != "" || strings.Contains(run.Note, "parser broke") {
		t.Errorf("run = %+v, want a clean finish", run)
	}
}

// A page that breaks the parser stops the run's requests, but the cards read from the pages
// before it stay saved and are counted as new; their details come on the next run, which is
// still a cold start because a parser_broke run never ends one.
func TestCollectorParserBrokeKeepsEarlierPages(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	stub := pageStub(t, map[string]string{
		"keywords=go+engineer&location=European+Union&f_WT=2&f_TPR=r604800&start=0":       "search.html",
		"keywords=go+engineer&location=European+Union&f_WT=2&f_TPR=r604800&start=10":      endOfResults,
		"keywords=platform+engineer&location=European+Union&f_WT=2&f_TPR=r604800&start=0": "search_broken.html",
	})
	var logs bytes.Buffer
	res, err := collector(t, s, stub, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != "parser_broke" || res.Searches != 2 || res.Cards != 10 || res.NewIDs != 10 || res.Observations != 0 {
		t.Fatalf("result = %+v, want parser_broke after 2 searches with 10 cards, 10 new, none observed", res)
	}
	if stub.Hits() != 3 {
		t.Errorf("requests = %d, want 3 (nothing after the broken page)", stub.Hits())
	}
	if got := len(requestedURLs(t, "detail")); got != 0 {
		t.Errorf("detail requests = %d, want 0 after a parser break", got)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = 'linkedin'`); got != 10 {
		t.Errorf("jobs = %d, want the 10 from the good page", got)
	}
	if got := count(t, `SELECT count(*) FROM company`); got < 1 {
		t.Errorf("companies = %d, want some", got)
	}
	run := latestRun(t, "collect")
	if run.StopReason != "parser_broke" ||
		!strings.Contains(run.Note, "2 searches, 10 cards, 10 new, 0 observed") ||
		!strings.Contains(run.Note, "parser broke on search page") ||
		!strings.Contains(run.Note, "10 new jobs saved without their details") {
		t.Errorf("run = %+v, want parser_broke with the saved jobs counted", run)
	}

	// The next run (the parser fixed) is still a cold start and fetches the details.
	ft := fixtures(t, testdata)
	res, err = collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !res.ColdStart || res.StopReason != "" || res.NewIDs != 10 || res.Observations != 7 {
		t.Fatalf("second result = %+v, want a clean cold start observing the 7 details on hand", res)
	}
	if got := count(t, `SELECT count(*) FROM job`); got != 10 {
		t.Errorf("jobs = %d, want the same 10", got)
	}
}

func TestCollectorRawKept(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ft := fixtures(t, testdata)
	var logs bytes.Buffer
	if _, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo); err != nil {
		t.Fatal(err)
	}
	if got := count(t, `SELECT count(*) FROM http_fetch`); got != ft.Hits() {
		t.Errorf("fetch rows = %d, transport answered %d", got, ft.Hits())
	}
	if got := count(t, `SELECT count(*) FROM http_fetch WHERE body_sha256 IS NULL OR length(body_sha256) <> 32`); got != 0 {
		t.Errorf("fetch rows without a sha256 = %d", got)
	}
	if got := count(t, `SELECT count(*) FROM http_fetch WHERE status = 200 AND length(body) = 0`); got != 0 {
		t.Errorf("200 rows without the body = %d", got)
	}
	if got := count(t, `SELECT count(*) FROM http_fetch f JOIN run r ON r.id = f.run_id WHERE r.command = 'collect' AND f.source = 'linkedin'`); got != ft.Hits() {
		t.Errorf("rows bound to the collect run = %d, want %d", got, ft.Hits())
	}
}

func canary(t *testing.T, s *store.Store, rt http.RoundTripper, extra ...fetch.Option) *linkedin.Canary {
	var logs bytes.Buffer
	return &linkedin.Canary{
		Store: s, NewClient: newClientFunc(t, rt, extra...), Now: time.Now, Loc: bucharest,
		Log: slog.New(slog.NewTextHandler(&logs, nil)),
	}
}

func TestCanary(t *testing.T) {
	ctx := context.Background()

	t.Run("pass, then skip the same day", func(t *testing.T) {
		s := openStore(t)
		ft := fixtures(t, testdata)
		res, err := canary(t, s, ft).Run(ctx, "platform engineer", "European Union")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Passed || res.Cards != 10 || res.StopReason != "" {
			t.Fatalf("result = %+v, want a pass with 10 cards", res)
		}
		urls := requestedURLs(t, "canary")
		if len(urls) != 1 || !strings.Contains(urls[0], "f_TPR=r86400") || !strings.Contains(urls[0], "start=0") {
			t.Errorf("canary requests = %v, want one page-1 r86400 search", urls)
		}
		if run := latestRun(t, "canary"); run.Source != "linkedin" || run.StopReason != "" || run.Requests != 1 {
			t.Errorf("run = %+v", run)
		}

		res, err = canary(t, s, ft).Run(ctx, "platform engineer", "European Union")
		if err != nil {
			t.Fatal(err)
		}
		if !res.Skipped || ft.Hits() != 1 {
			t.Fatalf("second result = %+v with %d requests, want skipped and no request", res, ft.Hits())
		}
		if got := count(t, `SELECT count(*) FROM run WHERE command = 'canary'`); got != 1 {
			t.Errorf("canary runs = %d, want 1 (a skip writes no run)", got)
		}
	})

	t.Run("broken page fails as parser_broke", func(t *testing.T) {
		s := openStore(t)
		m := shippedManifest(t)
		m["*seeMoreJobPostings/search?*&start=0*"] = "search_broken.html"
		ft := fixtures(t, fixtureDir(t, m))
		res, err := canary(t, s, ft).Run(ctx, "platform engineer", "European Union")
		if err != nil {
			t.Fatal(err)
		}
		if res.Passed || res.StopReason != "parser_broke" {
			t.Fatalf("result = %+v, want parser_broke", res)
		}
		if run := latestRun(t, "canary"); run.StopReason != "parser_broke" {
			t.Errorf("run stop = %q", run.StopReason)
		}
	})

	t.Run("too few cards fails as parser_broke", func(t *testing.T) {
		s := openStore(t)
		m := shippedManifest(t)
		m["*seeMoreJobPostings/search?*&start=0*"] = "search_empty.html"
		ft := fixtures(t, fixtureDir(t, m))
		res, err := canary(t, s, ft).Run(ctx, "platform engineer", "European Union")
		if err != nil {
			t.Fatal(err)
		}
		if res.Passed || res.StopReason != "parser_broke" || res.Cards != 0 {
			t.Fatalf("result = %+v, want parser_broke with 0 cards", res)
		}
	})
}

// ---- review fixes ----

// A first run that stops on its search budget still ends the cold start: the second run
// searches the last 24 hours.
func TestColdStartEndsAfterBudgetStop(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ft := fixtures(t, testdata)
	var logs bytes.Buffer
	tight := fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{fetch.KindSearch: 1, fetch.KindDetail: 40}))
	c := collector(t, s, ft, time.Now, &logs)
	c.NewClient = newClientFunc(t, ft, tight)
	res, err := c.Run(ctx, demo)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if !res.ColdStart || res.StopReason != "budget" || res.Searches != 1 {
		t.Fatalf("first result = %+v, want a cold start stopped on budget after one search", res)
	}
	before := len(requestedURLs(t, "search"))

	res, err = collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if res.ColdStart {
		t.Fatalf("second result = %+v, want steady state", res)
	}
	searches := requestedURLs(t, "search")[before:]
	if len(searches) == 0 {
		t.Fatal("second run made no search")
	}
	for _, u := range searches {
		if !strings.Contains(u, "f_TPR=r86400") {
			t.Errorf("second-run search %s, want f_TPR=r86400", u)
		}
	}
}

// insertRun records a finished run directly, as another command would have.
func insertRun(t *testing.T, s *store.Store, command, source, reason string) {
	t.Helper()
	ctx := context.Background()
	id, err := s.Q().StartRun(ctx, command, source)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Q().FinishRun(ctx, id, store.RunResult{Requests: 1, StopReason: reason}); err != nil {
		t.Fatal(err)
	}
}

// A throttled or blocked run of any LinkedIn command pauses every LinkedIn command for the
// day: each records a run with that reason and makes no request.
func TestPauseSharedAcrossCommands(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ by, reason string }{{"probe", "blocked"}, {"canary", "throttled"}, {"collect", "blocked"}} {
		t.Run(tc.by+" "+tc.reason, func(t *testing.T) {
			s := openStore(t)
			insertRun(t, s, tc.by, linkedin.Source, tc.reason)
			ft := fixtures(t, testdata)
			var logs bytes.Buffer

			res, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Paused || res.StopReason != tc.reason {
				t.Errorf("collect = %+v, want paused by %s", res, tc.reason)
			}
			cres, err := canary(t, s, ft).Run(ctx, "platform engineer", "European Union")
			if err != nil {
				t.Fatal(err)
			}
			if !cres.Paused || cres.StopReason != tc.reason || cres.Skipped {
				t.Errorf("canary = %+v, want paused by %s", cres, tc.reason)
			}
			if ft.Hits() != 0 {
				t.Errorf("requests = %d, want 0", ft.Hits())
			}
			for _, cmd := range []string{"collect", "canary"} {
				run := latestRun(t, cmd)
				if run.StopReason != tc.reason || run.Note != "paused today: "+tc.reason || run.Requests != 0 {
					t.Errorf("%s run = %+v, want a paused run", cmd, run)
				}
			}
		})
	}

	t.Run("a throttled canary pauses collect", func(t *testing.T) {
		s := openStore(t)
		throttle := &stubTransport{fn: func(*http.Request) (int, string) { return http.StatusTooManyRequests, "" }}
		cres, err := canary(t, s, throttle).Run(ctx, "platform engineer", "European Union")
		if err != nil {
			t.Fatal(err)
		}
		if cres.StopReason != "throttled" || throttle.Hits() != 2 {
			t.Fatalf("canary = %+v with %d requests, want throttled after a 429 and its retry", cres, throttle.Hits())
		}
		ft := fixtures(t, testdata)
		var logs bytes.Buffer
		res, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Paused || res.StopReason != "throttled" || ft.Hits() != 0 {
			t.Fatalf("collect = %+v with %d requests, want paused", res, ft.Hits())
		}
	})

	t.Run("pausing reasons only, linkedin only", func(t *testing.T) {
		s := openStore(t)
		insertRun(t, s, "collect", linkedin.Source, "parser_broke")
		insertRun(t, s, "collect", linkedin.Source, "failure_rate")
		insertRun(t, s, "canary", linkedin.SourceFixture, "throttled")
		ft := fixtures(t, testdata)
		var logs bytes.Buffer
		res, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
		if err != nil {
			t.Fatal(err)
		}
		if res.Paused || ft.Hits() == 0 {
			t.Fatalf("collect = %+v with %d requests, want a run", res, ft.Hits())
		}
	})
}

// A 429 whose retry the canary's budget refuses is throttled, so it pauses the day.
func TestCanaryThrottledOnBudgetAfter429(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	first := true
	st := &stubTransport{fn: func(*http.Request) (int, string) {
		if first {
			first = false
			return http.StatusTooManyRequests, ""
		}
		return http.StatusOK, ""
	}}
	one := fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{fetch.KindCanary: 1}))
	res, err := canary(t, s, st, one).Run(ctx, "platform engineer", "European Union")
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "throttled" || st.Hits() != 1 {
		t.Fatalf("result = %+v with %d requests, want throttled after one", res, st.Hits())
	}
	if run := latestRun(t, "canary"); run.StopReason != "throttled" {
		t.Errorf("run = %+v, want throttled", run)
	}
}

// A canary that never got a page to read records no stop reason and a "failed" note;
// parser_broke is kept for pages that were read and broke.
func TestCanaryFailuresAreNotParserBroke(t *testing.T) {
	ctx := context.Background()
	body := strings.Repeat("x", 64)
	cases := []struct {
		name  string
		fn    func(*http.Request) (int, string)
		extra []fetch.Option
		note  string
	}{
		{"503", func(*http.Request) (int, string) { return http.StatusServiceUnavailable, "" }, nil, "failed: status 503"},
		{"404", func(*http.Request) (int, string) { return http.StatusNotFound, "" }, nil, "failed: status 404"},
		{"410", func(*http.Request) (int, string) { return http.StatusGone, "" }, nil, "failed: status 410"},
		{"oversized", func(*http.Request) (int, string) { return http.StatusOK, body }, []fetch.Option{fetch.WithMaxBody(8)}, "failed: status 200, body over the size limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			res, err := canary(t, s, &stubTransport{fn: tc.fn}, tc.extra...).Run(ctx, "platform engineer", "European Union")
			if err != nil {
				t.Fatal(err)
			}
			if res.Passed || res.StopReason != "" || res.Note != tc.note {
				t.Fatalf("result = %+v, want no stop reason and note %q", res, tc.note)
			}
			if run := latestRun(t, "canary"); run.StopReason != "" || run.Note != tc.note {
				t.Errorf("run = %+v", run)
			}
		})
	}

	t.Run("transport error", func(t *testing.T) {
		s := openStore(t)
		c := canary(t, s, errTransport{})
		res, err := c.Run(ctx, "platform engineer", "European Union")
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != "" || !strings.HasPrefix(res.Note, "failed: no response: ") {
			t.Fatalf("result = %+v, want a failed note and no stop reason", res)
		}
	})

	t.Run("blocked keeps its reason", func(t *testing.T) {
		s := openStore(t)
		res, err := canary(t, s, &stubTransport{fn: func(*http.Request) (int, string) { return http.StatusForbidden, "" }}).
			Run(ctx, "platform engineer", "European Union")
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != "blocked" {
			t.Fatalf("result = %+v, want blocked", res)
		}
	})
}

// errTransport fails every request without dialling.
type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection reset")
}

// A dry run writes under linkedin-fixture: a dry-run canary today does not make the live
// canary think it already ran, and dry-run jobs are not LinkedIn's.
func TestDryRunSourceIsSeparate(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ft := fixtures(t, testdata)

	dry := canary(t, s, ft)
	dry.Source = linkedin.SourceFixture
	res, err := dry.Run(ctx, "platform engineer", "European Union")
	if err != nil || !res.Passed {
		t.Fatalf("dry canary = %+v, %v; want a pass", res, err)
	}
	res, err = canary(t, s, ft).Run(ctx, "platform engineer", "European Union")
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || !res.Passed {
		t.Fatalf("live canary after a dry one = %+v, want it to run", res)
	}

	s = openStore(t)
	var logs bytes.Buffer
	c := collector(t, s, ft, time.Now, &logs)
	c.Source = linkedin.SourceFixture
	if _, err := c.Run(ctx, demo); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM run WHERE source <> 'linkedin-fixture'`,
		`SELECT count(*) FROM http_fetch WHERE source <> 'linkedin-fixture'`,
		`SELECT count(*) FROM job WHERE source <> 'linkedin-fixture'`,
	} {
		if got := count(t, q); got != 0 {
			t.Errorf("%s = %d, want 0", q, got)
		}
	}
	if got := count(t, `SELECT count(*) FROM job`); got != 10 {
		t.Errorf("fixture jobs = %d, want 10", got)
	}
	// The live collector still starts cold.
	res2, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.ColdStart {
		t.Errorf("live collect after a dry run = %+v, want a cold start", res2)
	}
}

// assertGuardCountsOnly checks every stored guard_findings holds numbers only, at any
// depth: no excerpt of a posting is ever stored.
func assertGuardCountsOnly(t *testing.T) {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(), `SELECT guard_findings FROM job_observation`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
			t.Fatalf("guard_findings %s: %v (want a non-empty object)", raw, err)
		}
		var walk func(path string, v any)
		walk = func(path string, v any) {
			switch v := v.(type) {
			case float64:
			case map[string]any:
				for k, x := range v {
					walk(path+"."+k, x)
				}
			default:
				t.Errorf("guard_findings%s = %v (%T), want a number", path, v, v)
			}
		}
		walk("", m)
		n++
	}
	if n == 0 {
		t.Fatal("no observations to check")
	}
}

// TestCollectSeesKnownJobsAgain is the grid column "collect sees it again": last_seen is
// bumped and nothing else changes, whatever the job's status or listing status.
func TestCollectSeesKnownJobsAgain(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	ft := fixtures(t, fixtureDir(t, fullManifest(t)))
	var logs bytes.Buffer
	if _, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	owner := storetest.OwnerConn(t)
	q := s.Q()
	jobID := func(sourceJobID string) int64 {
		t.Helper()
		j, err := q.JobBySourceID(ctx, linkedin.Source, sourceJobID)
		if err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	// 1 stays new; 2 is applied; 4 is skipped; 5 is a listing found expired.
	applied, skipped, expired, fresh := jobID("1000000002"), jobID("1000000004"), jobID("1000000005"), jobID("1000000001")
	for _, st := range []struct {
		id     int64
		status string
	}{{applied, "shortlisted"}, {applied, "applied"}, {skipped, "skipped"}} {
		if _, err := q.InsertStatusEvent(ctx, st.id, st.status, "", "gremy", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Exec(ctx, `UPDATE job SET listing_status = 'expired', status_checked_at = now() WHERE id = $1`, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `UPDATE job SET last_seen_at = last_seen_at - interval '1 day'`); err != nil {
		t.Fatal(err)
	}
	events := count(t, `SELECT count(*) FROM status_event`)
	observations := count(t, `SELECT count(*) FROM job_observation`)
	gateRows := count(t, `SELECT count(*) FROM gate_result`)

	if _, err := collector(t, s, ft, time.Now, &logs).Run(ctx, demo); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE last_seen_at < now() - interval '1 hour'`); got != 0 {
		t.Errorf("jobs not bumped = %d, want 0", got)
	}
	if got := count(t, `SELECT count(*) FROM status_event`); got != events {
		t.Errorf("status events = %d, want %d (none written)", got, events)
	}
	if got := count(t, `SELECT count(*) FROM job_observation`); got != observations {
		t.Errorf("observations = %d, want %d", got, observations)
	}
	if got := count(t, `SELECT count(*) FROM gate_result`); got != gateRows {
		t.Errorf("gate rows = %d, want %d", got, gateRows)
	}
	for id, want := range map[int64]string{fresh: "new", applied: "applied", skipped: "skipped", expired: "new"} {
		st, err := q.LatestStatus(ctx, id)
		if err != nil || st.Status != want {
			t.Errorf("job %d status = %+v, %v; want %s", id, st, err, want)
		}
	}
	if got := count(t, `SELECT count(*) FROM job WHERE id = $1 AND listing_status = 'expired'`, expired); got != 1 {
		t.Error("the expired listing did not stay expired")
	}
}
