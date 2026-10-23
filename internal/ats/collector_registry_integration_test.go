//go:build integration

package ats_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// liveCollector is collector over the day-one fixtures with Fixture off: rows go under
// the ATS's own name and every fetch updates the board registry. Its transport is the stub
// router, so nothing dials.
func liveCollector(t *testing.T, s *store.Store, override map[string]func() (int, string), logs *bytes.Buffer) *ats.Collector {
	t.Helper()
	if override == nil {
		override = map[string]func() (int, string){}
	}
	c := collector(t, s, dayOne, override, logs)
	c.Fixture = false
	return c
}

func importDemo(t *testing.T, s *store.Store, origin string, boards ...ats.Board) {
	t.Helper()
	var rows []store.BoardImport
	for _, b := range boards {
		rows = append(rows, store.BoardImport{BoardKey: b.RegistryKey(), Name: b.Name, Origin: origin})
	}
	if _, _, err := s.Q().ImportBoards(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
}

// ownerRow scans one row of q as the owner.
func ownerRow(t *testing.T, q string, args []any, dest ...any) error {
	t.Helper()
	return storetest.OwnerConn(t).QueryRow(context.Background(), q, args...).Scan(dest...)
}

// fixtureBody is the day-one fixture answer for board b.
func fixtureBody(b ats.Board) ([]byte, error) {
	return os.ReadFile(filepath.Join(dayOne, b.ATS+"_"+b.Board+".json")) // #nosec G304 -- the test's own testdata
}

func boardState(t *testing.T, b ats.Board) (state string, misses int, postings *int) {
	t.Helper()
	k := b.RegistryKey()
	if err := ownerRow(t, `SELECT state, misses, postings_last FROM board WHERE ats = $1 AND lower(slug) = lower($2) AND region = $3`,
		[]any{k.ATS, k.Slug, k.Region}, &state, &misses, &postings); err != nil {
		t.Fatalf("board %s: %v", b, err)
	}
	return state, misses, postings
}

// TestCollectUpdatesBoardRegistry: each fetch writes its board's row as boards check does
// (ok with the posting count, not_found on a 404) and the summary counts the state changes;
// a board missing from the registry is collected but never added; a fixture run leaves the
// registry alone.
func TestCollectUpdatesBoardRegistry(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	demo := ats.DemoBoards()
	gh, levEU, ashby := demo[0], demo[2], demo[3]
	importDemo(t, s, store.OriginSeedList, gh, levEU, ashby) // the other lever board stays out

	// A fixture run first: nothing in the registry moves.
	run(t, collector(t, s, dayOne, nil, &logs))
	if got := count(t, `SELECT count(*) FROM board WHERE state <> 'unverified' OR checked_at IS NOT NULL`); got != 0 {
		t.Errorf("boards touched by a fixture run = %d, want 0", got)
	}

	res := run(t, liveCollector(t, s, map[string]func() (int, string){
		"api.ashbyhq.com": func() (int, string) { return http.StatusNotFound, "" },
	}, &logs))
	if st, _, n := boardState(t, gh); st != store.BoardOK || n == nil || *n != len(listed(t, dayOne, gh)) {
		t.Errorf("greenhouse board: %s, postings %v", st, n)
	}
	if st, _, _ := boardState(t, levEU); st != store.BoardOK {
		t.Errorf("eu lever board: %s", st)
	}
	if st, misses, _ := boardState(t, ashby); st != store.BoardNotFound || misses != 1 {
		t.Errorf("ashby board: %s, misses %d", st, misses)
	}
	if got := count(t, `SELECT count(*) FROM board`); got != 3 {
		t.Errorf("boards = %d, want 3 (collect never adds one)", got)
	}
	if l := byATS(t, res, ats.Lever); l.BoardsOK != 2 || l.StateChanges["unverified→ok"] != 1 {
		t.Errorf("lever: %+v", l)
	}
	a := byATS(t, res, ats.Ashby)
	if a.StateChanges["unverified→not_found"] != 1 || !strings.Contains(a.Note, "board states: 1 unverified→not_found") {
		t.Errorf("ashby: %+v", a)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = 'greenhouse'`); got == 0 {
		t.Error("the live run stored no greenhouse jobs")
	}
}

// TestTitleScreenNeitherStoredNorExpired: a posting whose title fails the screen is never
// stored and gets no observation, counted per board and per ATS; on a board fetched again
// with a screen, jobs stored earlier that the screen now drops stay open (expiry reads
// every listed id).
func TestTitleScreenNeitherStoredNorExpired(t *testing.T) {
	gh := ats.DemoBoards()[0]
	ids := listed(t, dayOne, gh)
	if len(ids) < 2 {
		t.Fatalf("greenhouse fixture lists %d postings, want at least 2", len(ids))
	}
	// Keep only the first greenhouse posting's title.
	body, err := fixtureBody(gh)
	if err != nil {
		t.Fatal(err)
	}
	ps, err := ats.Parse(gh, body)
	if err != nil {
		t.Fatal(err)
	}
	keep := ps[0].Title
	screen := func(title string) bool { return title == keep }

	t.Run("cold start", func(t *testing.T) {
		s := openStore(t)
		var logs bytes.Buffer
		c := collector(t, s, dayOne, nil, &logs)
		c.TitleScreen = screen
		res, err := c.Run(context.Background(), []ats.Board{gh})
		if err != nil {
			t.Fatal(err)
		}
		a := byATS(t, res, ats.Greenhouse)
		dropped := 0
		for _, p := range ps {
			if p.Title != keep {
				dropped++
			}
		}
		if a.ScreenedOut != dropped || a.ScreenedByBoard[gh.String()] != dropped || a.Postings != len(ps)-dropped {
			t.Errorf("result: %+v, want %d screened out", a, dropped)
		}
		if !strings.Contains(a.Note, " screened out") {
			t.Errorf("note %q", a.Note)
		}
		if got := count(t, `SELECT count(*) FROM job`); got != len(ps)-dropped {
			t.Errorf("jobs = %d, want %d", got, len(ps)-dropped)
		}
		if got := count(t, `SELECT count(*) FROM job_observation`); got != len(ps)-dropped {
			t.Errorf("observations = %d, want %d", got, len(ps)-dropped)
		}
	})

	t.Run("stored earlier", func(t *testing.T) {
		s := openStore(t)
		var logs bytes.Buffer
		if _, err := collector(t, s, dayOne, nil, &logs).Run(context.Background(), []ats.Board{gh}); err != nil {
			t.Fatal(err)
		}
		c := collector(t, s, dayOne, nil, &logs)
		c.TitleScreen = screen
		res, err := c.Run(context.Background(), []ats.Board{gh})
		if err != nil {
			t.Fatal(err)
		}
		if a := byATS(t, res, ats.Greenhouse); a.Expired != 0 || a.ScreenedOut == 0 {
			t.Errorf("result: %+v", a)
		}
		for _, id := range ids {
			if got := status(t, ats.SourceFixture(ats.Greenhouse), id); got != "open" {
				t.Errorf("%s = %s, want open", id, got)
			}
		}
	})
}

// gateTransport holds every request until a request of each of the three ATSs has
// arrived, then answers from the fixtures. If the ATSs ran one after another, the first
// request would wait alone and time out.
type gateTransport struct {
	ft      *fetch.FixtureTransport
	mu      sync.Mutex
	started map[string]bool
	all     chan struct{}
}

func (g *gateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	name := map[string]string{
		"boards-api.greenhouse.io": ats.Greenhouse,
		"api.lever.co":             ats.Lever,
		"api.eu.lever.co":          ats.Lever,
		"api.ashbyhq.com":          ats.Ashby,
	}[req.URL.Hostname()]
	g.mu.Lock()
	g.started[name] = true
	if len(g.started) == 3 {
		select {
		case <-g.all:
		default:
			close(g.all)
		}
	}
	g.mu.Unlock()
	select {
	case <-g.all:
	case <-time.After(10 * time.Second):
		return nil, errors.New("gate: the three ATSs never ran at the same time")
	}
	return g.ft.RoundTrip(req)
}

// TestATSRunsOverlap: the three ATSs run at the same time, and the results still come back
// in the fixed order.
func TestATSRunsOverlap(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	g := &gateTransport{ft: fixtures(t, dayOne), started: map[string]bool{}, all: make(chan struct{})}
	c := collector(t, s, dayOne, nil, &logs)
	c.NewClient = func(_ string, rec fetch.Recorder) *fetch.Client {
		return fetch.New(
			fetch.WithRecorder(rec),
			fetch.WithTransport(g),
			fetch.WithLiveGate(config.LiveATS),
			fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
			fetch.WithJitter(0),
			fetch.WithClock(nopClock{}),
			fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{fetch.KindBoard: 10})),
		)
	}
	res := run(t, c)
	var order []string
	for _, a := range res.ATS {
		order = append(order, a.ATS)
		if a.BoardsOK != a.Boards || a.StopReason != "" {
			t.Errorf("%s: %+v", a.ATS, a)
		}
	}
	if strings.Join(order, ",") != "greenhouse,lever,ashby" {
		t.Errorf("order = %v", order)
	}
}

// TestNoVerdictLeavesBoardState: a board answer over the size limit says nothing about the
// board: it is checked, but keeps its state and gains no miss, and none of its jobs expire.
func TestNoVerdictLeavesBoardState(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	gh := ats.DemoBoards()[0]
	importDemoBoards := func() { importDemo(t, s, store.OriginSeedList, gh) }
	importDemoBoards()
	run(t, liveCollector(t, s, nil, &logs)) // ok: jobs stored, board ok
	huge := strings.Repeat(" ", 6<<20)      // past fetch's default 5 MB limit
	res := run(t, liveCollector(t, s, map[string]func() (int, string){
		"boards-api.greenhouse.io": func() (int, string) { return http.StatusOK, `{"jobs": [` + huge + `]}` },
	}, &logs))
	if a := byATS(t, res, ats.Greenhouse); a.BoardsFailed != 1 || a.Expired != 0 || len(a.StateChanges) != 0 {
		t.Errorf("greenhouse: %+v", a)
	}
	if st, misses, _ := boardState(t, gh); st != store.BoardOK || misses != 0 {
		t.Errorf("board after no verdict: %s, misses %d; want ok, 0", st, misses)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = 'greenhouse' AND listing_status <> 'open'`); got != 0 {
		t.Errorf("greenhouse jobs not open = %d, want 0", got)
	}
}

// TestSameSlugTwoRegions: a Lever board and the EU board with the same slug store their
// jobs apart (the EU ids start "eu:") and never expire each other's.
func TestSameSlugTwoRegions(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	us := ats.Board{ATS: ats.Lever, Board: "samplesoft", Name: "SampleSoft"}
	eu := ats.Board{ATS: ats.Lever, Board: "samplesoft", Name: "SampleSoft", Region: ats.RegionEU}
	body, err := fixtureBody(us)
	if err != nil {
		t.Fatal(err)
	}
	ids := listed(t, dayOne, us)
	if len(ids) == 0 {
		t.Fatal("no lever fixture postings")
	}
	answer := func(b string) func() (int, string) { return func() (int, string) { return http.StatusOK, b } }
	collect := func(usBody, euBody string) ats.ATSResult {
		t.Helper()
		c := collector(t, s, dayOne, map[string]func() (int, string){"api.lever.co": answer(usBody), "api.eu.lever.co": answer(euBody)}, &logs)
		res, err := c.Run(context.Background(), []ats.Board{us, eu})
		if err != nil {
			t.Fatal(err)
		}
		return byATS(t, res, ats.Lever)
	}
	open := func(prefix string) int {
		t.Helper()
		return count(t, `SELECT count(*) FROM job WHERE source = $1 AND listing_status = 'open' AND starts_with(source_job_id, $2)`,
			ats.SourceFixture(ats.Lever), prefix)
	}

	if a := collect(string(body), string(body)); a.Boards != 2 || a.Postings != 2*len(ids) {
		t.Fatalf("both boards: %+v", a)
	}
	if open("samplesoft/") != len(ids) || open("eu:samplesoft/") != len(ids) {
		t.Fatalf("open: %d us, %d eu; want %d each", open("samplesoft/"), open("eu:samplesoft/"), len(ids))
	}
	// The EU board empties: only its jobs expire.
	if a := collect(string(body), "[]"); a.Expired != int64(len(ids)) {
		t.Errorf("eu emptied: %+v", a)
	}
	if open("samplesoft/") != len(ids) || open("eu:samplesoft/") != 0 {
		t.Errorf("after eu emptied: %d us, %d eu open", open("samplesoft/"), open("eu:samplesoft/"))
	}
	// And the other way round.
	if a := collect("[]", string(body)); a.Expired != int64(len(ids)) || a.Reopened != int64(len(ids)) {
		t.Errorf("us emptied: %+v", a)
	}
	if open("samplesoft/") != 0 || open("eu:samplesoft/") != len(ids) {
		t.Errorf("after us emptied: %d us, %d eu open", open("samplesoft/"), open("eu:samplesoft/"))
	}
}

// TestParserBrokeBodyKept: board fetch rows keep no body, except one the parser broke on,
// so the reason can be read later.
func TestParserBrokeBodyKept(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	broken := "<html>maintenance</html>"
	run(t, collector(t, s, dayOne, map[string]func() (int, string){
		"boards-api.greenhouse.io": func() (int, string) { return http.StatusOK, broken },
	}, &logs))
	if got := count(t, `SELECT count(*) FROM http_fetch WHERE kind = 'board' AND url LIKE 'https://boards-api.greenhouse.io/%' AND body = $1::bytea`, []byte(broken)); got != 1 {
		t.Errorf("broken greenhouse fetch rows with the body = %d, want 1", got)
	}
	if got := count(t, `SELECT count(*) FROM http_fetch WHERE kind = 'board' AND url NOT LIKE 'https://boards-api.greenhouse.io/%' AND length(body) > 0`); got != 0 {
		t.Errorf("parsed board fetch rows with a body = %d, want 0", got)
	}
}

// TestMixedCaseSlugFetchedAsSpelled: a registry board stored as MixedCase is requested at
// that spelling (a lower-cased URL answers 404 here, as Lever's does) and its row updated.
func TestMixedCaseSlugFetchedAsSpelled(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	b := ats.Board{ATS: ats.Lever, Board: "SampleSoft", Name: "SampleSoft"}
	importDemo(t, s, store.OriginManual, b)
	body, err := fixtureBody(ats.Board{ATS: ats.Lever, Board: "samplesoft"})
	if err != nil {
		t.Fatal(err)
	}
	var asked []string
	c := liveCollector(t, s, nil, &logs)
	c.NewClient = func(_ string, rec fetch.Recorder) *fetch.Client {
		return fetch.New(
			fetch.WithRecorder(rec),
			fetch.WithTransport(stubCase{body: body, asked: &asked}),
			fetch.WithLiveGate(config.LiveATS),
			fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
			fetch.WithJitter(0),
			fetch.WithClock(nopClock{}),
		)
	}
	res, err := c.Run(context.Background(), []ats.Board{b})
	if err != nil {
		t.Fatal(err)
	}
	if a := byATS(t, res, ats.Lever); a.BoardsOK != 1 {
		t.Errorf("lever: %+v (asked %v)", a, asked)
	}
	if st, _, _ := boardState(t, b); st != store.BoardOK {
		t.Errorf("board state %s, want ok", st)
	}
}

// stubCase answers 200 only at the exact spelling /v0/postings/SampleSoft, 404 otherwise.
type stubCase struct {
	body  []byte
	asked *[]string
}

func (s stubCase) RoundTrip(r *http.Request) (*http.Response, error) {
	*s.asked = append(*s.asked, r.URL.Path)
	status, body := http.StatusNotFound, ""
	if r.URL.Path == "/v0/postings/SampleSoft" {
		status, body = http.StatusOK, string(s.body)
	}
	return &http.Response{StatusCode: status, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
}
