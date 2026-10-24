package boardlist

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ats"
)

func TestFromURLNewATSs(t *testing.T) {
	sr := func(s string) ats.Board { return ats.Board{ATS: ats.SmartRecruiters, Board: s} }
	tt := func(s string) ats.Board { return ats.Board{ATS: ats.Teamtailor, Board: s} }
	pde := func(s string) ats.Board { return ats.Board{ATS: ats.Personio, Board: s} }
	pcom := func(s string) ats.Board { return ats.Board{ATS: ats.Personio, Board: s, Region: ats.RegionCom} }
	ok := []struct {
		in   string
		want ats.Board
	}{
		{"https://jobs.smartrecruiters.com/ExampleRobotics", sr("ExampleRobotics")}, // the id keeps its case
		{"https://jobs.smartrecruiters.com/ExampleRobotics/700000000000001-go-platform-engineer", sr("ExampleRobotics")},
		{"https://careers.smartrecruiters.com/ExampleRobotics", sr("ExampleRobotics")},
		{"jobs.smartrecruiters.com/examplerobotics?trid=x", sr("examplerobotics")},
		{"https://mockworks.teamtailor.com", tt("mockworks")},
		{"https://mockworks.teamtailor.com/jobs", tt("mockworks")},
		{"https://mockworks.teamtailor.com/jobs/123-go-engineer", tt("mockworks")},
		{"https://MockWorks.TeamTailor.com./jobs.rss", tt("mockworks")}, // a host has no case
		{"https://mock-works-2.teamtailor.com/", tt("mock-works-2")},
		{"https://musterfabrik.jobs.personio.de", pde("musterfabrik")},
		{"https://musterfabrik.jobs.personio.de/job/100?language=en", pde("musterfabrik")},
		{"https://musterfabrik.jobs.personio.com/xml", pcom("musterfabrik")},
	}
	for _, c := range ok {
		got, err := FromURL(c.in)
		if err != nil || got != c.want {
			t.Errorf("FromURL(%q) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	bad := []struct {
		in   string
		want error
	}{
		{"https://smartrecruiters.com/ExampleRobotics", ErrUnsupportedHost},
		{"https://jobs.smartrecruiters.com/", ErrNoSlug},
		{"https://jobs.smartrecruiters.com/sitemap.xml", ErrNotBoardPath},
		{"https://jobs.smartrecruiters.com/oneclick-ui/company/x", ErrNotBoardPath},
		{"https://jobs.smartrecruiters.com/" + strings.Repeat("a", 101), ErrBadSlug},
		{"https://teamtailor.com/jobs", ErrUnsupportedHost},
		{"https://a.b.teamtailor.com/jobs", ErrUnsupportedHost},
		{"https://app.na.teamtailor.com/x", ErrUnsupportedHost},
		{"https://mockworksteamtailor.com/jobs", ErrUnsupportedHost},
		{"https://mockworks.teamtailor.com.other.example/jobs", ErrUnsupportedHost},
		{"https://www.teamtailor.com/en/", ErrNotBoardPath},
		{"https://app.teamtailor.com/login", ErrNotBoardPath},
		{"https://mock_works.teamtailor.com/", ErrBadSlug},
		{"https://-mock.teamtailor.com/", ErrBadSlug},
		{"https://jobs.personio.de/", ErrUnsupportedHost},
		{"https://personio.de/", ErrUnsupportedHost},
		{"https://musterfabrik.personio.de/", ErrUnsupportedHost},
		{"https://a.musterfabrik.jobs.personio.de/", ErrUnsupportedHost},
		{"https://www.jobs.personio.de/", ErrNotBoardPath},
		{"https://musterfabrik.jobs.personio.net/", ErrUnsupportedHost},
	}
	for _, c := range bad {
		got, err := FromURL(c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("FromURL(%q) = %+v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

func TestBoardURLNewATSs(t *testing.T) {
	for _, tc := range []struct {
		b    ats.Board
		want string
	}{
		{ats.Board{ATS: ats.SmartRecruiters, Board: "ExampleRobotics"}, "https://jobs.smartrecruiters.com/ExampleRobotics"},
		{ats.Board{ATS: ats.Teamtailor, Board: "mockworks"}, "https://mockworks.teamtailor.com/jobs"},
		{ats.Board{ATS: ats.Personio, Board: "musterfabrik"}, "https://musterfabrik.jobs.personio.de/"},
		{ats.Board{ATS: ats.Personio, Board: "musterfabrik", Region: ats.RegionCom}, "https://musterfabrik.jobs.personio.com/"},
	} {
		if got := BoardURL(tc.b); got != tc.want {
			t.Errorf("BoardURL(%+v) = %q, want %q", tc.b, got, tc.want)
		}
		back, err := FromURL(tc.want)
		if err != nil || back != tc.b {
			t.Errorf("FromURL(%q) = %+v, %v; want %+v", tc.want, back, err, tc.b)
		}
	}
}

// TestParseCDXHostFamilies: the lines a domain query on a host family returns (the board
// hosts at any path, the ATS's own hosts, deeper hosts) become one board per company.
func TestParseCDXHostFamilies(t *testing.T) {
	in := `{"url": "https://mockworks.teamtailor.com/jobs"}
{"url": "https://mockworks.teamtailor.com/jobs/123-go-engineer"}
{"url": "https://www.teamtailor.com/en/pricing"}
{"url": "https://app.na.teamtailor.com/x"}
{"url": "https://teamtailor.com/"}
{"url": "https://otherworks.teamtailor.com/"}
{"url": "https://musterfabrik.jobs.personio.de/job/100"}
{"url": "https://musterfabrik.jobs.personio.com/"}
{"url": "https://jobs.smartrecruiters.com/ExampleRobotics/700000000000001-x"}
{"url": "https://jobs.smartrecruiters.com/sitemap.xml"}
`
	entries, refused, skipped, err := ParseCDX(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Board: ats.Board{ATS: ats.Teamtailor, Board: "mockworks"}},
		{Board: ats.Board{ATS: ats.Teamtailor, Board: "otherworks"}},
		{Board: ats.Board{ATS: ats.Personio, Board: "musterfabrik"}},
		{Board: ats.Board{ATS: ats.Personio, Board: "musterfabrik", Region: ats.RegionCom}},
		{Board: ats.Board{ATS: ats.SmartRecruiters, Board: "ExampleRobotics"}},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("entries\n got %+v\nwant %+v", entries, want)
	}
	if skipped != 2 {
		t.Errorf("skipped %d, want 2 (the deeper host and the bare domain)", skipped)
	}
	if len(refused) != 2 || refused[0].Reason != ErrNotBoardPath.Error() || refused[1].Reason != ErrNotBoardPath.Error() {
		t.Errorf("refused %+v, want www and sitemap as not a board", refused)
	}
}
