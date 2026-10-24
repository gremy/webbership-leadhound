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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// answerFunc answers one request: a status and a body.
type answerFunc func(r *http.Request) (int, string)

// stubRT answers every request through fn and counts them by URL; it never dials.
type stubRT struct {
	fn   answerFunc
	mu   sync.Mutex
	hits map[string]int
}

func (s *stubRT) RoundTrip(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	if s.hits == nil {
		s.hits = map[string]int{}
	}
	s.hits[req.URL.String()]++
	s.mu.Unlock()
	status, body := s.fn(req)
	return &http.Response{StatusCode: status, Header: http.Header{}, Request: req,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
}

// requests counts the requests whose URL contains substr.
func (s *stubRT) requests(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for u, c := range s.hits {
		if strings.Contains(u, substr) {
			n += c
		}
	}
	return n
}

// stubCollector is a dry-run collector whose every request goes to rt, live-gated as
// LEADHOUND_LIVE=ats, with the given budgets.
func stubCollector(s *store.Store, rt http.RoundTripper, budgets fetch.Budgets, logs *bytes.Buffer) *ats.Collector {
	return &ats.Collector{
		Store: s,
		NewClient: func(_ string, rec fetch.Recorder) *fetch.Client {
			return fetch.New(
				fetch.WithTransport(rt),
				fetch.WithLiveGate(config.LiveATS),
				fetch.WithRecorder(rec),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
				fetch.WithJitter(0),
				fetch.WithClock(nopClock{}),
				fetch.WithBreaker(fetch.NewBreaker(budgets)),
			)
		},
		Loc:     time.UTC,
		Fixture: true,
		Gates:   gates.CandidateGates{Languages: []string{"en"}},
		Log:     slog.New(slog.NewTextHandler(logs, nil)),
	}
}

func runBoards(t *testing.T, c *ats.Collector, boards ...ats.Board) ats.Result {
	t.Helper()
	res, err := c.Run(context.Background(), boards)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

var (
	srBoard = ats.Board{ATS: ats.SmartRecruiters, Board: "ExampleRobotics", Name: "Example Robotics"}
	ttBoard = ats.Board{ATS: ats.Teamtailor, Board: "mockworks", Name: "Mockworks"}
	peBoard = ats.Board{ATS: ats.Personio, Board: "musterfabrik", Name: "Musterfabrik"}
)

// srList answers a SmartRecruiters list request from a board of total postings with ids
// 1..total, honouring limit and offset.
func srList(r *http.Request, total int) string {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	var items []string
	for id := offset + 1; id <= total && id <= offset+limit; id++ {
		items = append(items, fmt.Sprintf(`{"id": "%d", "name": "Go Engineer %d", "location": {"city": "Exampleville", "remote": true, "hybrid": false}}`, id, id))
	}
	return fmt.Sprintf(`{"offset": %d, "limit": %d, "totalFound": %d, "content": [%s]}`, offset, limit, total, strings.Join(items, ","))
}

// srDetail is a posting's detail answer.
func srDetail(id string) string {
	return `{"id": "` + id + `", "postingUrl": "https://jobs.example/ExampleRobotics/` + id + `", "applyUrl": "https://jobs.example/ExampleRobotics/` + id + `?apply",` +
		` "jobAd": {"sections": {"jobDescription": {"title": "Job Description", "text": "<p>Build Go services with AI agents for posting ` + id + `.</p>"}}}}`
}

// isDetail reports whether r asks for one SmartRecruiters posting, and which.
func isDetail(r *http.Request) (string, bool) {
	_, id, ok := strings.Cut(r.URL.Path, "/postings/")
	return id, ok && r.URL.Host == "api.smartrecruiters.com"
}

const srSource = "smartrecruiters-fixture"

// TestCollectNewATSsFromFixtures: the demo boards of the three new ATSs, answered by the
// testdata fixtures, store and observe every posting with its gate rows; the one
// SmartRecruiters posting with a detail fixture gets its description, apply URL and public
// URL from it, cited to the detail fetch, whose body is not kept.
func TestCollectNewATSsFromFixtures(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	ft := fixtures(t, dayOne)
	c := collector(t, s, dayOne, nil, &logs)
	c.NewClient = func(_ string, rec fetch.Recorder) *fetch.Client {
		return fetch.New(fetch.WithFixtures(ft), fetch.WithRecorder(rec), fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
			fetch.WithJitter(0), fetch.WithClock(nopClock{}), fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{fetch.KindBoard: 40, fetch.KindDetail: 10})))
	}
	res := runBoards(t, c, srBoard, ttBoard, peBoard)
	if len(res.ATS) != 3 {
		t.Fatalf("results = %+v", res.ATS)
	}
	for name, want := range map[string]int{ats.SmartRecruiters: 5, ats.Teamtailor: 5, ats.Personio: 4} {
		a := byATS(t, res, name)
		if a.StopReason != "" || a.BoardsOK != 1 || a.Postings != want || a.NewJobs != want || a.Observations != want {
			t.Errorf("%s: %+v (logs %s)", name, a, logs.String())
		}
	}
	sr := byATS(t, res, ats.SmartRecruiters)
	if sr.Details != 1 || sr.DetailsFailed != 4 || sr.DetailsDeferred != 0 || !strings.Contains(sr.Note, "details: 1 fetched, 4 failed, 0 left for a later run") {
		t.Errorf("smartrecruiters details: %+v", sr)
	}
	if got := count(t, `SELECT count(*) FROM job_observation o WHERE NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id)`); got != 0 {
		t.Errorf("observations without gate rows = %d", got)
	}
	// Posting 1: description, apply URL and public URL from its detail, cited to the detail fetch.
	q := `SELECT count(*) FROM job_observation o JOIN job j ON j.id = o.job_id JOIN http_fetch f ON f.id = o.fetch_id
	      WHERE j.source = $1 AND j.source_job_id = 'examplerobotics/700000000000001' AND f.kind = 'detail'
	        AND o.description_clean LIKE '%friendly robots%' AND o.apply_url LIKE '%oga=true'
	        AND j.url LIKE 'https://jobs.example.com/ExampleRobotics/700000000000001-%'`
	if got := count(t, q, srSource); got != 1 {
		t.Errorf("posting 1 with its detail = %d, want 1", got)
	}
	// The others: an empty description, cited to the list page.
	if got := count(t, `SELECT count(*) FROM job_observation o JOIN job j ON j.id = o.job_id JOIN http_fetch f ON f.id = o.fetch_id
	      WHERE j.source = $1 AND f.kind = 'board' AND o.description_clean = ''`, srSource); got != 4 {
		t.Errorf("postings without a detail = %d, want 4", got)
	}
	// A detail answer is kept as size and hash only.
	if got := count(t, `SELECT count(*) FROM http_fetch WHERE kind = 'detail' AND status = 200 AND length(body) = 0 AND body_size > 0 AND body_sha256 IS NOT NULL`); got != 1 {
		t.Errorf("detail fetch rows kept as size and hash = %d, want 1", got)
	}

	// The next run asks again only for the postings still waiting (their latest observation
	// came from the list); posting 1 is not asked again, and nothing new is observed.
	res = runBoards(t, c, srBoard, ttBoard, peBoard)
	sr = byATS(t, res, ats.SmartRecruiters)
	if sr.NewJobs != 0 || sr.Observations != 0 || sr.Details != 0 || sr.DetailsFailed != 4 {
		t.Errorf("second run smartrecruiters: %+v", sr)
	}
	if got := ft.HitsFor("/postings/700000000000001"); got != 1 {
		t.Errorf("posting 1 detail asked %d times, want 1", got)
	}
}

// TestSmartRecruitersPaging: a board of 250 postings is read in three pages, offset by
// what each page held, every posting saved; a later run whose second page fails saves and
// expires nothing of that board.
func TestSmartRecruitersPaging(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	total, failPage := 250, false
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if id, ok := isDetail(r); ok {
			return http.StatusOK, srDetail(id)
		}
		if failPage && r.URL.Query().Get("offset") == "100" {
			return http.StatusInternalServerError, ""
		}
		return http.StatusOK, srList(r, total)
	}}
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 60, fetch.KindDetail: 1000}, &logs)
	a := byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.BoardsOK != 1 || a.Postings != total || a.NewJobs != total || a.Details != total || a.Capped != 0 || a.Expired != 0 {
		t.Fatalf("first run: %+v (logs %s)", a, logs.String())
	}
	for _, off := range []string{"offset=0", "offset=100", "offset=200"} {
		if got := rt.requests("postings?limit=100&" + off); got != 1 {
			t.Errorf("%s asked %d times, want 1", off, got)
		}
	}
	if got := rt.requests("offset=300"); got != 0 {
		t.Errorf("a fourth page was asked for")
	}
	// Each observation cites its own detail fetch; the list pages are board fetches.
	if got := count(t, `SELECT count(*) FROM http_fetch WHERE kind = 'board' AND source = $1`, srSource); got != 3 {
		t.Errorf("board fetch rows = %d, want 3", got)
	}

	// The board shrinks to 120, but page two fails: nothing of the board is saved or expired.
	total, failPage = 120, true
	a = byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.BoardsFailed != 1 || a.BoardsOK != 0 || a.Expired != 0 || a.Postings != 0 {
		t.Errorf("failed page: %+v", a)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = $1 AND listing_status = 'open'`, srSource); got != 250 {
		t.Errorf("open jobs after a failed page = %d, want 250", got)
	}
	// Every page answers: the 130 no longer listed expire.
	failPage = false
	a = byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.BoardsOK != 1 || a.Expired != 130 {
		t.Errorf("complete run: %+v", a)
	}
}

// TestPagedBoardCapped: a board with more pages than MaxBoardPages is read up to the cap,
// saved as read, and expires nothing, with a note.
func TestPagedBoardCapped(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	total := 100 * (ats.MaxBoardPages + 5)
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if id, ok := isDetail(r); ok {
			return http.StatusOK, srDetail(id)
		}
		return http.StatusOK, srList(r, total)
	}}
	// One job stored earlier that the board does not list: a capped read must not expire it.
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 100, fetch.KindDetail: 0}, &logs)
	small := &stubRT{fn: func(r *http.Request) (int, string) {
		if _, ok := isDetail(r); ok {
			return http.StatusNotFound, ""
		}
		return http.StatusOK, `{"offset": 0, "limit": 100, "totalFound": 1, "content": [{"id": "999999", "name": "Old Role"}]}`
	}}
	runBoards(t, stubCollector(s, small, fetch.Budgets{fetch.KindBoard: 10}, &logs), srBoard)

	a := byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.Capped != 1 || a.BoardsOK != 1 || a.Postings != 100*ats.MaxBoardPages || a.Expired != 0 ||
		!strings.Contains(a.Note, "read in part, nothing expired: "+srBoard.String()) {
		t.Errorf("capped: %+v", a)
	}
	if got := rt.requests("/postings?"); got != ats.MaxBoardPages {
		t.Errorf("list requests = %d, want %d", got, ats.MaxBoardPages)
	}
	if got := status(t, srSource, "examplerobotics/999999"); got != "open" {
		t.Errorf("unlisted job after a capped read = %s, want open", got)
	}
	// A detail budget of 0: no detail asked, every new posting left for a later run.
	if a.Details != 0 || a.DetailsDeferred != a.NewJobs || rt.requests("/postings/") != 0 {
		t.Errorf("detail budget 0: %+v, %d detail requests", a, rt.requests("/postings/"))
	}
}

// TestTeamtailorPaging: a full page of 100 items is followed by the next; a short page ends.
func TestTeamtailorPaging(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	item := func(n int) string {
		return fmt.Sprintf(`<item><title>Go Engineer %d</title><description>&lt;p&gt;Go and AI.&lt;/p&gt;</description><guid>g-%d</guid><link>https://mockworks.teamtailor.com/jobs/%d-go</link><remoteStatus>fully</remoteStatus></item>`, n, n, n)
	}
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		var items []string
		for n := offset + 1; n <= 107 && n <= offset+100; n++ {
			items = append(items, item(n))
		}
		return http.StatusOK, `<rss version="2.0"><channel><title>Mockworks</title>` + strings.Join(items, "") + `</channel></rss>`
	}}
	a := byATS(t, runBoards(t, stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 40}, &logs), ttBoard), ats.Teamtailor)
	if a.BoardsOK != 1 || a.Postings != 107 || a.NewJobs != 107 || a.Requests != 2 {
		t.Errorf("teamtailor: %+v", a)
	}
	if rt.requests("offset=0&per_page=100") != 1 || rt.requests("offset=100&per_page=100") != 1 {
		t.Errorf("pages asked: %v", rt.hits)
	}
}

// TestPersonioRetryWithoutLanguage: a feed with no position described in English is asked
// once more without the language, and that answer is the board's; a retry that fails
// leaves the board unread.
func TestPersonioRetryWithoutLanguage(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	empty, full := string(readTestdata(t, "personio_leerwerk.xml")), string(readTestdata(t, "personio_musterfabrik.xml"))
	retryStatus := http.StatusOK
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if r.URL.Query().Has("language") {
			return http.StatusOK, empty
		}
		return retryStatus, full
	}}
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 10}, &logs)
	a := byATS(t, runBoards(t, c, peBoard), ats.Personio)
	if a.BoardsOK != 1 || a.Postings != 4 || a.Requests != 2 || rt.requests("/xml?language=en") != 1 || rt.requests("/xml") != 2 {
		t.Errorf("retry: %+v, hits %v", a, rt.hits)
	}
	// The retry fails: the board is not read, and its four jobs stay open.
	retryStatus = http.StatusServiceUnavailable
	a = byATS(t, runBoards(t, c, peBoard), ats.Personio)
	if a.BoardsFailed != 1 || a.Expired != 0 {
		t.Errorf("failed retry: %+v", a)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = 'personio-fixture' AND listing_status = 'open'`); got != 4 {
		t.Errorf("open jobs after a failed retry = %d, want 4", got)
	}
	// A feed described in English is not asked twice.
	before := rt.requests("/xml")
	full2 := &stubRT{fn: func(*http.Request) (int, string) { return http.StatusOK, full }}
	a = byATS(t, runBoards(t, stubCollector(s, full2, fetch.Budgets{fetch.KindBoard: 10}, &logs), peBoard), ats.Personio)
	if a.Requests != 1 || full2.requests("language=en") != 1 || rt.requests("/xml") != before {
		t.Errorf("described feed: %+v", a)
	}
}

// TestDetailBudgetAndLaterRuns: a detail budget smaller than the new postings details the
// first ones, stores the rest without a description and lists them as left for later;
// the next run details them, with one more observation each and no second `new` event.
func TestDetailBudgetAndLaterRuns(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if id, ok := isDetail(r); ok {
			return http.StatusOK, srDetail(id)
		}
		return http.StatusOK, srList(r, 5)
	}}
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 10, fetch.KindDetail: 2}, &logs)
	a := byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.StopReason != "" || a.NewJobs != 5 || a.Details != 2 || a.DetailsDeferred != 3 || a.Observations != 5 ||
		!strings.Contains(a.Note, "2 fetched, 0 failed, 3 left for a later run") {
		t.Errorf("first run: %+v", a)
	}
	if got := count(t, `SELECT count(*) FROM job_observation WHERE description_clean = ''`); got != 3 {
		t.Errorf("observations without a description = %d, want 3", got)
	}
	a = byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.NewJobs != 0 || a.Details != 2 || a.DetailsDeferred != 1 || a.Observations != 2 {
		t.Errorf("second run: %+v", a)
	}
	a = byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.Details != 1 || a.DetailsDeferred != 0 || a.Observations != 1 {
		t.Errorf("third run: %+v", a)
	}
	a = byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.Details != 0 || a.Observations != 0 || rt.requests("/postings/") != 5 {
		t.Errorf("fourth run: %+v, %d detail requests (want 5 in all)", a, rt.requests("/postings/"))
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM status_event WHERE status = 'new'`: 5,
		`SELECT count(*) FROM job_observation`:                   8,
		// Every job's latest observation now has its description.
		`SELECT count(*) FROM job j WHERE (SELECT o.description_clean FROM job_observation o WHERE o.job_id = j.id ORDER BY o.observed_at DESC, o.id DESC LIMIT 1) = ''`: 0,
		`SELECT count(*) FROM job_observation o WHERE NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id)`:                                            0,
	} {
		if got := count(t, q); got != want {
			t.Errorf("%s = %d, want %d", q, got, want)
		}
	}
}

// TestDetailFailuresOpenTheBreaker: detail requests that fail count against the run's
// breaker; once it opens, the postings are still stored (without descriptions), the rest of
// the details wait, and the run stops with the breaker's reason.
func TestDetailFailuresOpenTheBreaker(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if _, ok := isDetail(r); ok {
			return http.StatusInternalServerError, ""
		}
		return http.StatusOK, srList(r, 8)
	}}
	a := byATS(t, runBoards(t, stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 10, fetch.KindDetail: 100}, &logs), srBoard), ats.SmartRecruiters)
	if a.StopReason != string(fetch.StopFailureRate) || a.Postings != 8 || a.NewJobs != 8 || a.Details != 0 ||
		a.DetailsFailed+a.DetailsDeferred != 8 || a.DetailsDeferred == 0 {
		t.Errorf("breaker: %+v", a)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = $1`, srSource); got != 8 {
		t.Errorf("jobs = %d, want 8", got)
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dayOne, name)) // #nosec G304 -- the package's own fixtures
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSmartRecruitersListingChangedWhileReading: a board whose totalFound changes between
// its pages (here 250 on page one, 220 after) is read to the end and saved, but it may have
// shifted postings across a page edge, so nothing of it expires, and the note says why.
func TestSmartRecruitersListingChangedWhileReading(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	total, shrinkAfterFirst := 250, false
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if id, ok := isDetail(r); ok {
			return http.StatusOK, srDetail(id)
		}
		if shrinkAfterFirst && r.URL.Query().Get("offset") != "0" {
			return http.StatusOK, srList(r, 220)
		}
		return http.StatusOK, srList(r, total)
	}}
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 60, fetch.KindDetail: 1000}, &logs)
	if a := byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters); a.BoardsOK != 1 || a.Postings != 250 || a.Shifted != 0 {
		t.Fatalf("first run: %+v", a)
	}
	shrinkAfterFirst = true
	a := byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.BoardsOK != 1 || a.Expired != 0 || a.Shifted != 1 || a.Postings != 220 ||
		!strings.Contains(a.Note, "listing changed while reading, nothing expired: "+srBoard.String()) {
		t.Errorf("listing changed: %+v", a)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = $1 AND listing_status = 'open'`, srSource); got != 250 {
		t.Errorf("open jobs after a changed listing = %d, want 250", got)
	}
}

// TestPagedBoardRepeatingPage: a Teamtailor board that ignores offset answers the same full
// page every time; the second page adds no new posting, so the read stops there and the
// board expires nothing, with a note.
func TestPagedBoardRepeatingPage(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	item := func(n int) string {
		return fmt.Sprintf(`<item><title>Go Engineer %d</title><description>&lt;p&gt;Go and AI.&lt;/p&gt;</description><guid>g-%d</guid><link>https://mockworks.teamtailor.com/jobs/%d-go</link><remoteStatus>fully</remoteStatus></item>`, n, n, n)
	}
	feed := func(from, to int) string {
		var items []string
		for n := from; n <= to; n++ {
			items = append(items, item(n))
		}
		return `<rss version="2.0"><channel><title>Mockworks</title>` + strings.Join(items, "") + `</channel></rss>`
	}
	// A job stored earlier that the repeating board does not list: it must stay open.
	old := &stubRT{fn: func(*http.Request) (int, string) { return http.StatusOK, feed(500, 500) }}
	runBoards(t, stubCollector(s, old, fetch.Budgets{fetch.KindBoard: 10}, &logs), ttBoard)

	rt := &stubRT{fn: func(*http.Request) (int, string) { return http.StatusOK, feed(1, 100) }}
	a := byATS(t, runBoards(t, stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 40}, &logs), ttBoard), ats.Teamtailor)
	if a.BoardsOK != 1 || a.Postings != 100 || a.Expired != 0 || a.Repeated != 1 || a.Requests > 2 || rt.requests("mockworks") > 2 ||
		!strings.Contains(a.Note, "a page added no new posting, read in part, nothing expired: "+ttBoard.String()) {
		t.Errorf("repeating page: %+v, hits %v", a, rt.hits)
	}
	if got := count(t, `SELECT count(*) FROM job WHERE source = 'teamtailor-fixture' AND listing_status = 'open'`); got != 101 {
		t.Errorf("open jobs = %d, want 101 (the unlisted one kept)", got)
	}
}

// listGatesCandidate is a remote-only candidate allowed to work from Romania only.
var listGatesCandidate = gates.CandidateGates{Languages: []string{"en"}, AllowedCountries: []string{"Romania"}, RemoteOnly: true}

// srListOf answers a SmartRecruiters list request with the given postings, each a JSON
// object, all on one page.
func srListOf(items ...string) string {
	return fmt.Sprintf(`{"offset": 0, "limit": 100, "totalFound": %d, "content": [%s]}`, len(items), strings.Join(items, ","))
}

// srItem is one list posting with its location object (a JSON fragment).
func srItem(id, title, location string) string {
	return fmt.Sprintf(`{"id": "%s", "name": "%s", "location": %s}`, id, title, location)
}

// listGatePostings are five postings for listGatesCandidate:
//  1. remote from the United States: fails the location gate on list data;
//  2. remote from Europe: passes on list data;
//  3. a location with no workplace flag in Germany: fails the workplace gate only because
//     nothing says remote, which the description may (the empty description's doing);
//  4. remote from Europe with a German title: the language gate reads the title while the
//     description is empty, and fails;
//  5. on-site in Germany by the list's own flags: fails the workplace gate on list data.
var listGatePostings = []string{
	srItem("1", "Go Engineer", `{"fullLocation": "Exampleville, United States", "remote": true, "hybrid": false}`),
	srItem("2", "Go Engineer", `{"fullLocation": "Europe", "remote": true, "hybrid": false}`),
	srItem("3", "Go Engineer", `{"fullLocation": "Exampleville, Germany"}`),
	srItem("4", "Softwareentwickler für verteilte Systeme und Datenbanken", `{"fullLocation": "Europe", "remote": true, "hybrid": false}`),
	srItem("5", "Go Engineer", `{"fullLocation": "Exampleville, Germany", "remote": false, "hybrid": false}`),
}

// TestDetailOnlyForPostingsPassingListGates: before the detail step, the gates run on each
// posting's list data (title, company, location with its workplace label, no description);
// a posting that fails there is stored with its gate rows and never asked for a detail,
// counted as not needed. A fail that rests only on the missing description (the language
// read from the title, a location with no workplace flag) leaves the posting eligible.
func TestDetailOnlyForPostingsPassingListGates(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if id, ok := isDetail(r); ok {
			return http.StatusOK, srDetail(id)
		}
		return http.StatusOK, srListOf(listGatePostings...)
	}}
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 10, fetch.KindDetail: 100}, &logs)
	c.Gates = listGatesCandidate
	a := byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.NewJobs != 5 || a.Details != 3 || a.DetailsFailed != 0 || a.DetailsDeferred != 0 || a.DetailsNotNeeded != 2 ||
		a.Observations != 5 || !strings.Contains(a.Note, "details: 3 fetched, 0 failed, 0 left for a later run, 2 not needed (gates failed)") {
		t.Errorf("run: %+v (logs %s)", a, logs.String())
	}
	for id, want := range map[string]int{"1": 0, "2": 1, "3": 1, "4": 1, "5": 0} {
		if got := rt.requests("/postings/" + id); got != want {
			t.Errorf("posting %s: %d detail requests, want %d", id, got, want)
		}
	}
	// Postings 1 and 5 are stored from the list, with their list-data fails.
	listFail := `SELECT count(*) FROM gate_result g JOIN job_observation o ON o.id = g.observation_id
	             JOIN job j ON j.id = o.job_id JOIN http_fetch f ON f.id = o.fetch_id
	             WHERE j.source_job_id = $1 AND f.kind = 'board' AND g.gate = $2 AND g.verdict = 'fail'`
	for _, c := range []struct{ id, gate string }{
		{"examplerobotics/1", gates.GateLocation}, {"examplerobotics/5", gates.GateWorkplace},
	} {
		if got := count(t, listFail, c.id, c.gate); got != 1 {
			t.Errorf("%s: %d list-data %s fails, want 1", c.id, got, c.gate)
		}
	}
	// The premises: with no description, postings 3 and 4 do fail a gate as Observe
	// evaluates them, on the gate the description decides.
	for _, c := range []struct{ title, location, gate string }{
		{"Go Engineer", "Exampleville, Germany", gates.GateWorkplace},
		{"Softwareentwickler für verteilte Systeme und Datenbanken", "Europe (Remote)", gates.GateLanguage},
	} {
		failed := false
		for _, g := range gates.Evaluate(gates.Observation{Title: c.title, Location: c.location}, listGatesCandidate, nil) {
			failed = failed || (g.Gate == c.gate && g.Verdict == gates.Fail)
		}
		if !failed {
			t.Errorf("%q in %q: no %s fail on list data; the test's premise is gone", c.title, c.location, c.gate)
		}
	}

	// The next run asks for nothing: the details came, and the two failed postings, still
	// without one, are counted as not needing it again.
	a = byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.Details != 0 || a.DetailsDeferred != 0 || a.DetailsNotNeeded != 2 || rt.requests("/postings/") != 3 {
		t.Errorf("second run: %+v, %d detail requests (want 3 in all)", a, rt.requests("/postings/"))
	}
}

// TestListGateFailedNeverWaitsForDetail: with no detail budget, a posting that fails on
// list data is not left for a later run, and the score queue counts it as gate-failed, not
// as awaiting its detail; the posting that passed waits for its detail, and so does the
// one whose only fail is the language read from its title.
func TestListGateFailedNeverWaitsForDetail(t *testing.T) {
	s := openStore(t)
	var logs bytes.Buffer
	rt := &stubRT{fn: func(r *http.Request) (int, string) {
		if id, ok := isDetail(r); ok {
			return http.StatusOK, srDetail(id)
		}
		return http.StatusOK, srListOf(listGatePostings[0], listGatePostings[1], listGatePostings[3])
	}}
	c := stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 10, fetch.KindDetail: 0}, &logs)
	c.Gates = listGatesCandidate
	a := byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.NewJobs != 3 || a.Details != 0 || a.DetailsDeferred != 2 || a.DetailsNotNeeded != 1 ||
		!strings.Contains(a.Note, "details: 0 fetched, 0 failed, 2 left for a later run, 1 not needed (gates failed)") {
		t.Errorf("run: %+v (logs %s)", a, logs.String())
	}
	queue, err := s.Q().ScoreQueue(context.Background())
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	if len(queue.Entries) != 0 || queue.GateFailed != 1 || queue.AwaitingDetail != 2 {
		t.Errorf("queue: %d entries, %d gate failed, %d awaiting detail; want 0, 1, 2", len(queue.Entries), queue.GateFailed, queue.AwaitingDetail)
	}

	// With budget again, only the two eligible postings are asked for.
	c = stubCollector(s, rt, fetch.Budgets{fetch.KindBoard: 10, fetch.KindDetail: 100}, &logs)
	c.Gates = listGatesCandidate
	a = byATS(t, runBoards(t, c, srBoard), ats.SmartRecruiters)
	if a.Details != 2 || a.DetailsDeferred != 0 || a.DetailsNotNeeded != 1 || rt.requests("/postings/1") != 0 {
		t.Errorf("second run: %+v", a)
	}
}
