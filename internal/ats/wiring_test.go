package ats

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store"
)

func TestValidateNamesEveryATS(t *testing.T) {
	err := Board{ATS: "workable", Board: "x"}.Validate()
	if err == nil {
		t.Fatal("unknown ATS accepted")
	}
	for _, name := range Names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
	if len(Names) != 6 {
		t.Errorf("Names = %v, want six ATSs", Names)
	}
}

func TestCheckAndDetailURLs(t *testing.T) {
	sr := Board{ATS: SmartRecruiters, Board: "ExampleRobotics"}
	for _, tc := range []struct {
		name, got, want string
	}{
		{"sr check", sr.CheckURL(), "https://api.smartrecruiters.com/v1/companies/ExampleRobotics/postings?limit=1&offset=0"},
		{"sr page 2", sr.pageURL(100), "https://api.smartrecruiters.com/v1/companies/ExampleRobotics/postings?limit=100&offset=100"},
		{"sr detail", sr.DetailURL("700000000000001"), "https://api.smartrecruiters.com/v1/companies/ExampleRobotics/postings/700000000000001"},
		{"sr detail escaped", sr.DetailURL("a/b"), "https://api.smartrecruiters.com/v1/companies/ExampleRobotics/postings/a%2Fb"},
		{"teamtailor check", Board{ATS: Teamtailor, Board: "mockworks"}.CheckURL(), "https://mockworks.teamtailor.com/jobs.rss?offset=0&per_page=100"},
		{"teamtailor page 2", Board{ATS: Teamtailor, Board: "mockworks"}.pageURL(100), "https://mockworks.teamtailor.com/jobs.rss?offset=100&per_page=100"},
		{"personio check", Board{ATS: Personio, Board: "musterfabrik"}.CheckURL(), "https://musterfabrik.jobs.personio.de/xml?language=en"},
		{"no detail for teamtailor", Board{ATS: Teamtailor, Board: "mockworks"}.DetailURL("1"), ""},
		{"no detail for greenhouse", Board{ATS: Greenhouse, Board: "examplecorp"}.DetailURL("1"), ""},
		{"greenhouse ignores offset", Board{ATS: Greenhouse, Board: "examplecorp"}.pageURL(100), Board{ATS: Greenhouse, Board: "examplecorp"}.APIURL()},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestMaxBoardRequests(t *testing.T) {
	for name, want := range map[string]int{
		Greenhouse: 1, Lever: 1, Ashby: 1, SmartRecruiters: MaxBoardPages, Teamtailor: MaxBoardPages, Personio: 2,
	} {
		if got := MaxBoardRequests(name); got != want {
			t.Errorf("MaxBoardRequests(%s) = %d, want %d", name, got, want)
		}
	}
}

// srPage builds a SmartRecruiters list page of n postings numbered from first.
func srPage(total, offset, first, n int) []byte {
	var items []string
	for i := range n {
		items = append(items, fmt.Sprintf(`{"id": "%d", "name": "Role %d"}`, first+i, first+i))
	}
	return []byte(fmt.Sprintf(`{"offset": %d, "limit": 100, "totalFound": %d, "content": [%s]}`, offset, total, strings.Join(items, ",")))
}

// ttPage builds a Teamtailor feed page of n items numbered from first.
func ttPage(first, n int) []byte {
	var items []string
	for i := range n {
		items = append(items, fmt.Sprintf(`<item><title>Role %d</title><guid>g-%d</guid><link>https://mockworks.teamtailor.com/jobs/%d-role</link></item>`, first+i, first+i, first+i))
	}
	return []byte(`<rss version="2.0"><channel><title>Mockworks</title>` + strings.Join(items, "") + `</channel></rss>`)
}

func TestParsePage(t *testing.T) {
	sr := Board{ATS: SmartRecruiters, Board: "ExampleRobotics"}
	tt := Board{ATS: Teamtailor, Board: "mockworks"}
	for _, tc := range []struct {
		name     string
		b        Board
		offset   int
		body     []byte
		n, next  int
		wantLast bool
	}{
		{"sr first of three", sr, 0, srPage(250, 0, 1, 100), 100, 100, false},
		{"sr middle", sr, 100, srPage(250, 100, 101, 100), 100, 200, false},
		{"sr last short", sr, 200, srPage(250, 200, 201, 50), 50, 250, true},
		{"sr exact fit", sr, 100, srPage(200, 100, 101, 100), 100, 200, true},
		{"sr empty page ends", sr, 300, srPage(999, 300, 1, 0), 0, 300, true},
		{"sr empty board", sr, 0, srPage(0, 0, 1, 0), 0, 0, true},
		{"sr fixture", sr, 0, readFixture(t, "smartrecruiters_examplerobotics.json"), 5, 5, true},
		{"teamtailor full page", tt, 0, ttPage(1, 100), 100, 100, false},
		{"teamtailor short page", tt, 100, ttPage(101, 7), 7, 107, true},
		{"teamtailor empty page", tt, 100, ttPage(1, 0), 0, 100, true},
		{"greenhouse one answer", Board{ATS: Greenhouse, Board: "examplecorp"}, 0, readFixture(t, "greenhouse_examplecorp.json"), -1, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps, next, last, err := parsePage(tc.b, tc.offset, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.n >= 0 && (len(ps) != tc.n || next != tc.next) {
				t.Errorf("postings %d, next %d; want %d, %d", len(ps), next, tc.n, tc.next)
			}
			if last != tc.wantLast {
				t.Errorf("last = %v, want %v", last, tc.wantLast)
			}
		})
	}
	// A page with no totalFound is broken, even when its postings read.
	if _, _, _, err := parsePage(sr, 0, []byte(`{"content": [{"id": "1", "name": "A"}]}`)); !errors.Is(err, ErrParserBroke) {
		t.Errorf("no totalFound: err = %v, want ErrParserBroke", err)
	}
}

func TestCountPostings(t *testing.T) {
	sr := Board{ATS: SmartRecruiters, Board: "ExampleRobotics"}
	if n, err := CountPostings(sr, srPage(250, 0, 1, 1)); err != nil || n != 250 {
		t.Errorf("sr count = %d, %v; want totalFound 250", n, err)
	}
	if _, err := CountPostings(sr, []byte(`<html/>`)); !errors.Is(err, ErrParserBroke) {
		t.Errorf("sr broken: err = %v, want ErrParserBroke", err)
	}
	if n, err := CountPostings(Board{ATS: Teamtailor, Board: "mockworks"}, readFixture(t, "teamtailor_mockworks.rss")); err != nil || n != 5 {
		t.Errorf("teamtailor count = %d, %v; want 5", n, err)
	}
	if n, err := CountPostings(Board{ATS: Personio, Board: "musterfabrik"}, readFixture(t, "personio_musterfabrik.xml")); err != nil || n != 4 {
		t.Errorf("personio count = %d, %v; want 4", n, err)
	}
}

func TestParseDispatchesNewATSs(t *testing.T) {
	for _, tc := range []struct {
		b    Board
		file string
	}{
		{Board{ATS: SmartRecruiters, Board: "ExampleRobotics"}, "smartrecruiters_examplerobotics.json"},
		{Board{ATS: Teamtailor, Board: "mockworks"}, "teamtailor_mockworks.rss"},
		{Board{ATS: Personio, Board: "musterfabrik"}, "personio_musterfabrik.xml"},
	} {
		ps, err := Parse(tc.b, readFixture(t, tc.file))
		if err != nil || len(ps) == 0 {
			t.Errorf("%s: %d postings, %v", tc.b, len(ps), err)
		}
		if _, err := Parse(tc.b, []byte("{")); !errors.Is(err, ErrParserBroke) || !strings.Contains(err.Error(), tc.b.String()) {
			t.Errorf("%s broken: err = %v, want ErrParserBroke naming the board", tc.b, err)
		}
	}
}

func TestRegionIDs(t *testing.T) {
	de := Board{ATS: Personio, Board: "musterfabrik"}
	com := Board{ATS: Personio, Board: "Musterfabrik", Region: RegionCom}
	p := Posting{ID: "100"}
	if got := SourceJobID(com, p); got != "com:musterfabrik/100" {
		t.Errorf("SourceJobID com = %q", got)
	}
	if SourceJobID(de, p) == SourceJobID(com, p) || reflect.DeepEqual(de.SameBoardKey(), com.SameBoardKey()) {
		t.Error("the .de and .com boards of one slug are one board")
	}
	if got := boardPrefix(com); got != "com:musterfabrik" {
		t.Errorf("boardPrefix = %q", got)
	}
}

// TestListTotal: a SmartRecruiters page says its board's total; other ATSs and a broken
// page say nothing.
func TestListTotal(t *testing.T) {
	sr := Board{ATS: SmartRecruiters, Board: "ExampleRobotics"}
	if n, ok := listTotal(sr, srPage(250, 100, 101, 100)); !ok || n != 250 {
		t.Errorf("sr total = %d, %v; want 250, true", n, ok)
	}
	if _, ok := listTotal(sr, []byte(`{"content": []}`)); ok {
		t.Error("a page with no totalFound gave a total")
	}
	if _, ok := listTotal(Board{ATS: Teamtailor, Board: "mockworks"}, ttPage(1, 100)); ok {
		t.Error("teamtailor gave a total")
	}
}

// TestDetailSourcesMatchTheCollector: the store holds back from scoring the jobs of exactly
// the sources the collector writes SmartRecruiters rows under, live and dry run.
func TestDetailSourcesMatchTheCollector(t *testing.T) {
	want := []string{SmartRecruiters, SourceFixture(SmartRecruiters)}
	if !reflect.DeepEqual(store.DetailSources, want) {
		t.Errorf("store.DetailSources = %v, want %v", store.DetailSources, want)
	}
}
