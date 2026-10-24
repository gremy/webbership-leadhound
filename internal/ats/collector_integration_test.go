//go:build integration

package ats_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const (
	dayOne = "testdata"
	dayTwo = "testdata/later"
)

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

func fixtures(t *testing.T, dir string) *fetch.FixtureTransport {
	t.Helper()
	ft, err := fetch.NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ft
}

// router answers a request whose host has an override in memory and every other one from
// the fixtures; it never dials.
type router struct {
	ft       *fetch.FixtureTransport
	override map[string]func() (int, string) // host → status and body
}

func (r router) RoundTrip(req *http.Request) (*http.Response, error) {
	fn, ok := r.override[req.URL.Hostname()]
	if !ok {
		return r.ft.RoundTrip(req)
	}
	status, body := fn()
	h := http.Header{}
	if status == http.StatusTooManyRequests {
		h.Set("Retry-After", "0")
	}
	return &http.Response{
		StatusCode: status, Header: h, Request: req,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}, nil
}

type nopClock struct{}

func (nopClock) Now() time.Time                             { return time.Now() }
func (nopClock) Sleep(context.Context, time.Duration) error { return nil }

// collector builds a dry-run collector over the fixtures in dir. Hosts in override are
// answered by the stub instead; the stub passes the live gate as LiveATS, since it never
// dials.
func collector(t *testing.T, s *store.Store, dir string, override map[string]func() (int, string), logs *bytes.Buffer) *ats.Collector {
	t.Helper()
	ft := fixtures(t, dir)
	return &ats.Collector{
		Store: s,
		NewClient: func(_ string, rec fetch.Recorder) *fetch.Client {
			opts := []fetch.Option{
				fetch.WithRecorder(rec),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
				fetch.WithJitter(0),
				fetch.WithClock(nopClock{}),
				fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{fetch.KindBoard: 10})),
			}
			if override == nil {
				opts = append(opts, fetch.WithFixtures(ft))
			} else {
				opts = append(opts, fetch.WithTransport(router{ft: ft, override: override}), fetch.WithLiveGate(config.LiveATS))
			}
			return fetch.New(opts...)
		},
		Loc:     time.UTC,
		Fixture: true,
		Gates:   gates.CandidateGates{Languages: []string{"en"}},
		Log:     slog.New(slog.NewTextHandler(logs, nil)),
	}
}

// classicBoards are the demo boards of the ATSs that list a board in one answer
// (Greenhouse, Lever, Ashby); the tests in this file pin their day-one and day-two
// fixtures. The paged, retried and detailed ATSs have their own tests
// (collector_more_integration_test.go).
func classicBoards() []ats.Board {
	var out []ats.Board
	for _, b := range ats.DemoBoards() {
		switch b.ATS {
		case ats.Greenhouse, ats.Lever, ats.Ashby:
			out = append(out, b)
		}
	}
	return out
}

func run(t *testing.T, c *ats.Collector) ats.Result {
	t.Helper()
	res, err := c.Run(context.Background(), classicBoards())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func byATS(t *testing.T, res ats.Result, name string) ats.ATSResult {
	t.Helper()
	for _, a := range res.ATS {
		if a.ATS == name {
			return a
		}
	}
	t.Fatalf("no result for %s in %+v", name, res)
	return ats.ATSResult{}
}

// listed is the source_job_ids the fixtures in dir list for board b, read through Parse.
func listed(t *testing.T, dir string, b ats.Board) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, fetch.ManifestFile)) // #nosec G304 -- the test's own testdata
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(b.APIURL())) {
		return nil
	}
	file := b.ATS + "_" + b.Board + ".json"
	body, err := os.ReadFile(filepath.Join(dir, file)) // #nosec G304 -- the test's own testdata
	if err != nil {
		t.Fatal(err)
	}
	ps, err := ats.Parse(b, body)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, ats.SourceJobID(b, p))
	}
	return ids
}

func listedAll(t *testing.T, dir string) map[string][]string {
	t.Helper()
	m := map[string][]string{}
	for _, b := range classicBoards() {
		m[b.String()] = listed(t, dir, b)
	}
	return m
}

func status(t *testing.T, src, id string) string {
	t.Helper()
	var s string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT listing_status FROM job WHERE source = $1 AND source_job_id = $2`, src, id).Scan(&s); err != nil {
		t.Fatalf("status %s %s: %v", src, id, err)
	}
	return s
}

// diff is what is in a but not in b.
func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

// TestCollectColdStartAndSteadyState: the first run saves every posting with an
// observation, its gate rows and a `new` event, all under the -fixture sources; the same
// fixtures again write nothing new and expire nothing.
func TestCollectColdStartAndSteadyState(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	want := 0
	for _, ids := range listedAll(t, dayOne) {
		want += len(ids)
	}
	if want == 0 {
		t.Fatal("fixtures list no postings")
	}

	res := run(t, collector(t, s, dayOne, nil, &logs))
	if len(res.ATS) != 3 || len(res.RunIDs()) != 3 {
		t.Fatalf("results = %+v, want one per ATS", res.ATS)
	}
	total := 0
	for _, a := range res.ATS {
		if a.StopReason != "" || a.BoardsOK != a.Boards || a.Observations != a.NewJobs || a.Expired != 0 {
			t.Errorf("%s: %+v", a.ATS, a)
		}
		if a.Source != ats.SourceFixture(a.ATS) || !strings.HasPrefix(a.Note, a.ATS+": ") {
			t.Errorf("%s: source %q note %q", a.ATS, a.Source, a.Note)
		}
		total += a.Observations
	}
	if total != want {
		t.Errorf("observations = %d, want %d", total, want)
	}
	if got := byATS(t, res, ats.Lever).Boards; got != 2 {
		t.Errorf("lever boards = %d, want 2", got)
	}

	for q, n := range map[string]int{
		`SELECT count(*) FROM job`:                                      want,
		`SELECT count(*) FROM job WHERE listing_status = 'open'`:        want,
		`SELECT count(*) FROM job_observation WHERE apply_type = 'ats'`: want,
		`SELECT count(*) FROM job_observation o JOIN job j ON j.id = o.job_id WHERE o.derived_by = replace(j.source, '-fixture', '') || '-parser@1'`: want,
		`SELECT count(*) FROM status_event WHERE status = 'new' AND actor = 'system'`:                                                                want,
		`SELECT count(*) FROM job_observation o WHERE NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id)`:                        0,
		`SELECT count(*) FROM run WHERE command = 'collect' AND finished_at IS NOT NULL AND stop_reason = ''`:                                        3,
		`SELECT count(*) FROM http_fetch WHERE kind = 'board' AND status = 200`:                                                                      4,
		// Each observation cites the board fetch it came from, in its own run and source.
		`SELECT count(*) FROM job_observation o JOIN http_fetch f ON f.id = o.fetch_id JOIN job j ON j.id = o.job_id WHERE f.source = j.source`: want,
	} {
		if got := count(t, q); got != n {
			t.Errorf("%s = %d, want %d", q, got, n)
		}
	}
	for _, table := range []string{"run", "http_fetch", "job"} {
		if got := count(t, `SELECT count(*) FROM `+table+` WHERE source NOT LIKE '%-fixture'`); got != 0 {
			t.Errorf("%s rows not under a -fixture source = %d, want 0", table, got)
		}
	}

	// Steady state: the same boards again add nothing and expire nothing, and every job a
	// board still lists is stamped as checked by this run.
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), `UPDATE job SET status_checked_at = NULL`); err != nil {
		t.Fatal(err)
	}
	res = run(t, collector(t, s, dayOne, nil, &logs))
	if got := count(t, `SELECT count(*) FROM job WHERE status_checked_at IS NOT NULL`); got != want {
		t.Errorf("jobs stamped as checked = %d, want %d", got, want)
	}
	for _, a := range res.ATS {
		if a.NewJobs != 0 || a.Observations != 0 || a.Expired != 0 || a.Reopened != 0 || a.BoardsOK != a.Boards {
			t.Errorf("rerun %s: %+v", a.ATS, a)
		}
	}
	if got := count(t, `SELECT count(*) FROM job_observation`); got != want {
		t.Errorf("observations after rerun = %d, want %d", got, want)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE listing_status = 'open'`); got != want {
		t.Errorf("open jobs after rerun = %d, want %d", got, want)
	}
}

// TestCollectDayTwo: a posting gone from a board that answered expires; a new one is
// observed; a board that did not answer expires nothing; a posting listed again reopens.
func TestCollectDayTwo(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	one, two := listedAll(t, dayOne), listedAll(t, dayTwo)
	run(t, collector(t, s, dayOne, nil, &logs))

	res := run(t, collector(t, s, dayTwo, nil, &logs))
	var gone, added []string
	var failedBoard ats.Board
	for _, b := range classicBoards() {
		if two[b.String()] == nil {
			failedBoard = b
			continue
		}
		for _, id := range diff(one[b.String()], two[b.String()]) {
			gone = append(gone, b.ATS+" "+id)
		}
		for _, id := range diff(two[b.String()], one[b.String()]) {
			added = append(added, b.ATS+" "+id)
		}
	}
	if len(gone) == 0 || len(added) == 0 || failedBoard.ATS == "" {
		t.Fatalf("day-two fixtures: gone %v, added %v, failed %v; want one of each", gone, added, failedBoard)
	}

	expired, observed := int64(0), 0
	for _, a := range res.ATS {
		expired += a.Expired
		observed += a.Observations
	}
	if expired != int64(len(gone)) || observed != len(added) {
		t.Errorf("expired %d, observed %d; want %d, %d", expired, observed, len(gone), len(added))
	}
	for _, g := range gone {
		atsName, id, _ := strings.Cut(g, " ")
		if got := status(t, ats.SourceFixture(atsName), id); got != "expired" {
			t.Errorf("gone %s = %s, want expired", g, got)
		}
	}
	for _, a := range added {
		atsName, id, _ := strings.Cut(a, " ")
		if got := count(t, `SELECT count(*) FROM job_observation o JOIN job j ON j.id = o.job_id WHERE j.source = $1 AND j.source_job_id = $2`,
			ats.SourceFixture(atsName), id); got != 1 {
			t.Errorf("added %s observations = %d, want 1", a, got)
		}
	}
	// The board that did not answer keeps every job open and is named in the note.
	for _, id := range one[failedBoard.String()] {
		if got := status(t, ats.SourceFixture(failedBoard.ATS), id); got != "open" {
			t.Errorf("failed board %s job %s = %s, want open", failedBoard, id, got)
		}
	}
	fa := byATS(t, res, failedBoard.ATS)
	if fa.NotFound+fa.BoardsFailed == 0 || fa.Expired != 0 || !strings.Contains(fa.Note, failedBoard.String()) {
		t.Errorf("failed board's ATS: %+v", fa)
	}

	// Day one's list again: the gone posting reopens, the day-two posting expires.
	res = run(t, collector(t, s, dayOne, nil, &logs))
	reopened, expired := int64(0), int64(0)
	for _, a := range res.ATS {
		reopened += a.Reopened
		expired += a.Expired
	}
	if reopened != int64(len(gone)) || expired != int64(len(added)) {
		t.Errorf("reappearance: reopened %d, expired %d; want %d, %d", reopened, expired, len(gone), len(added))
	}
	for _, g := range gone {
		atsName, id, _ := strings.Cut(g, " ")
		if got := status(t, ats.SourceFixture(atsName), id); got != "open" {
			t.Errorf("reappeared %s = %s, want open", g, got)
		}
	}
	// A reopened job keeps its one observation: it is not new.
	if got := count(t, `SELECT count(*) FROM job j WHERE (SELECT count(*) FROM job_observation o WHERE o.job_id = j.id) <> 1`); got != 0 {
		t.Errorf("jobs without exactly one observation = %d, want 0", got)
	}
}

// TestFailedFetchExpiresNothing: a 500, a body the parser cannot read, and a board
// missing from a day's answers all leave every job of that board open.
func TestFailedFetchExpiresNothing(t *testing.T) {
	for name, answer := range map[string]func() (int, string){
		"500":          func() (int, string) { return http.StatusInternalServerError, "" },
		"parser broke": func() (int, string) { return http.StatusOK, "<html>maintenance</html>" },
		"empty jobs":   nil, // a control: an answer listing nothing expires the whole board
	} {
		t.Run(name, func(t *testing.T) {
			s := openStore(t)
			var logs bytes.Buffer
			run(t, collector(t, s, dayOne, nil, &logs))
			gh := ats.DemoBoards()[0]
			if gh.ATS != ats.Greenhouse {
				t.Fatalf("first demo board = %s, want a greenhouse one", gh)
			}
			ids := listed(t, dayOne, gh)
			fn := answer
			if fn == nil {
				fn = func() (int, string) { return http.StatusOK, `{"jobs": [], "meta": {"total": 0}}` }
			}
			res := run(t, collector(t, s, dayOne, map[string]func() (int, string){"boards-api.greenhouse.io": fn}, &logs))
			a := byATS(t, res, ats.Greenhouse)
			wantStatus := "open"
			switch name {
			case "500":
				if a.BoardsFailed != 1 || a.StopReason != "" || strings.Contains(a.Note, "listed 0") {
					t.Errorf("500: %+v", a)
				}
			case "parser broke":
				if a.BoardsBroke != 1 || a.StopReason != "parser_broke" || !strings.Contains(a.Note, "parser broke on "+gh.String()) {
					t.Errorf("parser broke: %+v", a)
				}
			default:
				if a.BoardsOK != 1 || a.Expired != int64(len(ids)) {
					t.Errorf("empty jobs: %+v (parse error? logs %s)", a, logs.String())
				}
				line := fmt.Sprintf("\n%s: listed 0 postings, %d expired", gh, len(ids))
				if !strings.HasSuffix(a.Note, line) {
					t.Errorf("note %q, want it to end with %q", a.Note, line)
				}
				wantStatus = "expired"
			}
			for _, id := range ids {
				if got := status(t, ats.SourceFixture(ats.Greenhouse), id); got != wantStatus {
					t.Errorf("%s = %s, want %s", id, got, wantStatus)
				}
			}
			// The other ATSs ran as usual.
			for _, other := range []string{ats.Lever, ats.Ashby} {
				if o := byATS(t, res, other); o.BoardsOK != o.Boards || o.StopReason != "" {
					t.Errorf("%s: %+v", other, o)
				}
			}
		})
	}
}

// TestParserBrokeOnOneBoardOfTwo: the run's stop_reason is parser_broke only when every
// board of the ATS broke; one of two is a note line, and the other board still expires.
func TestParserBrokeOnOneBoardOfTwo(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	run(t, collector(t, s, dayOne, nil, &logs))
	res := run(t, collector(t, s, dayOne, map[string]func() (int, string){
		"api.eu.lever.co": func() (int, string) { return http.StatusOK, "not json" },
	}, &logs))
	a := byATS(t, res, ats.Lever)
	if a.BoardsBroke != 1 || a.BoardsOK != 1 || a.StopReason != "" || !strings.Contains(a.Note, "parser broke on lever/") {
		t.Errorf("lever: %+v", a)
	}
	if got := count(t, `SELECT count(*) FROM run WHERE stop_reason = 'parser_broke'`); got != 0 {
		t.Errorf("parser_broke runs = %d, want 0", got)
	}
}

// TestBreakerStopLeavesOthersRunning: two 429s stop Greenhouse's run as throttled; Lever
// and Ashby run as usual, and the next run leaves Greenhouse alone for the day.
func TestBreakerStopLeavesOthersRunning(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	throttle := map[string]func() (int, string){
		"boards-api.greenhouse.io": func() (int, string) { return http.StatusTooManyRequests, "" },
	}
	res := run(t, collector(t, s, dayOne, throttle, &logs))
	gh := byATS(t, res, ats.Greenhouse)
	if gh.StopReason != string(fetch.StopThrottled) || gh.Observations != 0 || gh.BoardsOK != 0 || !strings.Contains(gh.Note, "stopped: throttled") {
		t.Errorf("greenhouse: %+v", gh)
	}
	for _, other := range []string{ats.Lever, ats.Ashby} {
		o := byATS(t, res, other)
		if o.StopReason != "" || o.BoardsOK != o.Boards || o.Observations == 0 {
			t.Errorf("%s: %+v", other, o)
		}
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = $1`, ats.SourceFixture(ats.Greenhouse)); got != 0 {
		t.Errorf("greenhouse jobs = %d, want 0", got)
	}

	// Throttled today: Greenhouse is paused, with no request, while the others run again.
	res = run(t, collector(t, s, dayOne, nil, &logs))
	gh = byATS(t, res, ats.Greenhouse)
	if !gh.Paused || gh.Requests != 0 || gh.StopReason != string(fetch.StopThrottled) {
		t.Errorf("greenhouse after throttle: %+v", gh)
	}
	if o := byATS(t, res, ats.Lever); o.Paused || o.BoardsOK != o.Boards {
		t.Errorf("lever after greenhouse throttle: %+v", o)
	}
}

// TestDuplicateBoardFetchedOnce: a board named twice is requested once.
func TestDuplicateBoardFetchedOnce(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	c := collector(t, s, dayOne, nil, &logs)
	b := ats.DemoBoards()[0]
	res, err := c.Run(context.Background(), []ats.Board{b, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ATS) != 1 || res.ATS[0].Boards != 1 || res.ATS[0].Requests != 1 {
		t.Errorf("results = %+v", res.ATS)
	}
}
