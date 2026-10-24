package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// TestCheckBoardNewATSs: each new ATS is checked at its cheapest list URL and its count is
// read the way that ATS states it (SmartRecruiters' totalFound from a one-posting page).
func TestCheckBoardNewATSs(t *testing.T) {
	for _, tc := range []struct {
		b      ats.Board
		url    string
		body   string
		accept string
		want   int
	}{
		{ats.Board{ATS: ats.SmartRecruiters, Board: "ExampleRobotics"},
			"https://api.smartrecruiters.com/v1/companies/ExampleRobotics/postings?limit=1&offset=0",
			`{"offset": 0, "limit": 1, "totalFound": 312, "content": [{"id": "1", "name": "Go Engineer"}]}`, "application/json", 312},
		{ats.Board{ATS: ats.Teamtailor, Board: "mockworks"},
			"https://mockworks.teamtailor.com/jobs.rss?offset=0&per_page=100",
			`<rss version="2.0"><channel><item><title>Go Engineer</title><guid>g-1</guid></item></channel></rss>`, "application/rss+xml", 1},
		{ats.Board{ATS: ats.Personio, Board: "musterfabrik", Region: ats.RegionCom},
			"https://musterfabrik.jobs.personio.com/xml?language=en",
			`<workzag-jobs><position><id>1</id><name>Go Engineer</name></position><position><id>2</id><name>AI Engineer</name></position></workzag-jobs>`, "application/rss+xml", 2},
	} {
		t.Run(tc.b.String(), func(t *testing.T) {
			var asked []string
			var accept string
			c := fetch.New(
				fetch.WithTransport(stubAnswer(func(r *http.Request) (*http.Response, error) {
					asked = append(asked, r.URL.String())
					accept = r.Header.Get("Accept")
					return answer(r, 200, tc.body), nil
				})),
				fetch.WithLiveGate(config.LiveATS),
				fetch.WithRecorder(verifyRecorder{}),
				fetch.WithProfile(atsProfileFor(tc.b.ATS)),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
				fetch.WithJitter(0),
			)
			outcome, n, stop, err := checkBoard(context.Background(), c, tc.b)
			if err != nil || stop != "" || outcome != store.CheckOK || n != tc.want {
				t.Fatalf("= %q, %d, stop %q, err %v; want ok, %d", outcome, n, stop, err, tc.want)
			}
			if len(asked) != 1 || asked[0] != tc.url {
				t.Errorf("asked %v, want %s", asked, tc.url)
			}
			if !strings.HasPrefix(accept, tc.accept) {
				t.Errorf("Accept %q, want it to start %q", accept, tc.accept)
			}
		})
	}
}

// TestLiveCoversEveryATS: LEADHOUND_LIVE=ats allows a board on every ATS host family, and
// allATSHosts names one board for each ATS and region, so a live check or collect never
// refuses a registry board for its host.
func TestLiveCoversEveryATS(t *testing.T) {
	hosts := allATSHosts()
	for _, name := range ats.Names {
		if !slices.ContainsFunc(hosts, func(b ats.Board) bool { return b.ATS == name }) {
			t.Errorf("allATSHosts has no %s board", name)
		}
	}
	for _, b := range append(hosts, ats.DemoBoards()...) {
		if err := b.Validate(); err != nil {
			t.Error(err)
		}
		if !config.LiveATS.Allows(boardHost(b)) {
			t.Errorf("ats refuses %s (host %s)", b, boardHost(b))
		}
		if d := b.DetailURL("1"); d != "" && !strings.HasPrefix(d, "https://api.smartrecruiters.com/") {
			t.Errorf("%s detail URL %s is on another host", b, d)
		}
	}
	if n := boardsPerATS(ats.DemoBoards()); len(n) != len(ats.Names) {
		t.Errorf("boardsPerATS(demo) = %v, want every ATS", n)
	}
}

func TestBoardsImportAndFilterKnowEveryATS(t *testing.T) {
	noDB(t)
	for _, name := range ats.Names {
		f := boardFilterFlags{ats: name}
		if err := f.check(); err != nil {
			t.Errorf("--ats %s: %v", name, err)
		}
	}
	if err := (&boardFilterFlags{ats: "workable"}).check(); err == nil {
		t.Error("--ats workable accepted")
	}
	_, _, stderr := runOut(t, "boards", "import", "x.csv", "--origin", "manual", "--ats", "workable")
	for _, name := range ats.Names {
		if !strings.Contains(stderr, name) {
			t.Errorf("import --ats error %q does not name %s", stderr, name)
		}
	}
}
