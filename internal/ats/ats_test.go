package ats

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
)

// readFixture reads testdata/<name>.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- the package's own fixtures
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ts parses an RFC 3339 time for a want table.
func ts(t *testing.T, s string) *time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	v = v.UTC()
	return &v
}

// assertPostings compares every field of every posting, in order.
func assertPostings(t *testing.T, got, want []Posting) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d postings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		check := func(field, gv, wv string) {
			if gv != wv {
				t.Errorf("posting %d %s:\n got %q\nwant %q", i, field, gv, wv)
			}
		}
		check("ID", g.ID, w.ID)
		check("Title", g.Title, w.Title)
		check("Company", g.Company, w.Company)
		check("Location", g.Location, w.Location)
		check("DescriptionHTML", g.DescriptionHTML, w.DescriptionHTML)
		check("EmploymentType", g.EmploymentType, w.EmploymentType)
		check("ApplyURL", g.ApplyURL, w.ApplyURL)
		check("URL", g.URL, w.URL)
		switch {
		case g.PostedAt == nil && w.PostedAt == nil:
		case g.PostedAt == nil || w.PostedAt == nil:
			t.Errorf("posting %d PostedAt: got %v, want %v", i, g.PostedAt, w.PostedAt)
		case !g.PostedAt.Equal(*w.PostedAt) || g.PostedAt.Location() != time.UTC:
			t.Errorf("posting %d PostedAt: got %v, want %v (UTC)", i, g.PostedAt, w.PostedAt)
		}
	}
}

func TestBoardValidate(t *testing.T) {
	ok := []Board{
		{ATS: Greenhouse, Board: "examplecorp"},
		{ATS: Lever, Board: "Sample.Soft_2-x"},
		{ATS: Lever, Board: "samplesoft", Region: RegionEU},
		{ATS: Ashby, Board: "9demo", Name: "Demo Labs"},
		{ATS: Ashby, Board: strings.Repeat("a", 100)},
	}
	for _, b := range ok {
		if err := b.Validate(); err != nil {
			t.Errorf("%+v: unexpected %v", b, err)
		}
	}
	bad := map[string]Board{
		"no ats":               {Board: "x"},
		"unknown ats":          {ATS: "workday", Board: "x"},
		"ats case":             {ATS: "Lever", Board: "x"},
		"empty slug":           {ATS: Ashby},
		"slug with slash":      {ATS: Ashby, Board: "a/b"},
		"slug with query":      {ATS: Ashby, Board: "a?b=1"},
		"slug leading dot":     {ATS: Ashby, Board: ".hidden"},
		"slug leading dash":    {ATS: Ashby, Board: "-x"},
		"slug with space":      {ATS: Ashby, Board: "a b"},
		"slug too long":        {ATS: Ashby, Board: strings.Repeat("a", 101)},
		"region on ashby":      {ATS: Ashby, Board: "x", Region: RegionEU},
		"region on gh":         {ATS: Greenhouse, Board: "x", Region: RegionEU},
		"unknown lever region": {ATS: Lever, Board: "x", Region: "us"},
	}
	for name, b := range bad {
		if err := b.Validate(); err == nil || !strings.HasPrefix(err.Error(), "ats: ") {
			t.Errorf("%s: err = %v, want an ats: error", name, err)
		}
	}
}

func TestBoardAPIURLAndString(t *testing.T) {
	for _, tc := range []struct {
		b       Board
		url, id string
	}{
		{Board{ATS: Greenhouse, Board: "examplecorp"}, "https://boards-api.greenhouse.io/v1/boards/examplecorp/jobs?content=true", "greenhouse/examplecorp"},
		{Board{ATS: Lever, Board: "samplesoft"}, "https://api.lever.co/v0/postings/samplesoft?mode=json", "lever/samplesoft"},
		{Board{ATS: Lever, Board: "beispielwerk", Region: RegionEU}, "https://api.eu.lever.co/v0/postings/beispielwerk?mode=json", "lever/beispielwerk"},
		{Board{ATS: Ashby, Board: "demolabs"}, "https://api.ashbyhq.com/posting-api/job-board/demolabs", "ashby/demolabs"},
		{Board{ATS: "workday", Board: "x"}, "", "workday/x"},
	} {
		if got := tc.b.APIURL(); got != tc.url {
			t.Errorf("%+v APIURL = %q, want %q", tc.b, got, tc.url)
		}
		if got := tc.b.String(); got != tc.id {
			t.Errorf("%+v String = %q, want %q", tc.b, got, tc.id)
		}
	}
}

// writeBoards writes content as ats-boards.yaml in a fresh directory.
func writeBoards(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, BoardsFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadBoards(t *testing.T) {
	dir := writeBoards(t, `# boards
- {ats: ashby, board: demolabs}
- ats: lever
  board: beispielwerk
  region: eu
  name: Beispielwerk
- {ats: greenhouse, board: examplecorp, name: Example Corp}
`)
	got, err := LoadBoards(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Board{
		{ATS: Ashby, Board: "demolabs"},
		{ATS: Lever, Board: "beispielwerk", Region: RegionEU, Name: "Beispielwerk"},
		{ATS: Greenhouse, Board: "examplecorp", Name: "Example Corp"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	got, err = LoadBoards(writeBoards(t, "[]\n"))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("[]: got %#v, %v; want an empty list", got, err)
	}
}

func TestLoadBoardsRefuses(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"unknown key", "- {ats: ashby, board: demolabs, url: x}\n", "url"},
		{"bad ats", "- {ats: ashby, board: a}\n- {ats: workday, board: demolabs}\n", "line 2"},
		{"bad slug", "- {ats: ashby, board: \"../x\"}\n", "line 1"},
		{"region on non-lever", "- {ats: greenhouse, board: x, region: eu}\n", "lever boards only"},
		{"ats is lower-case", "- {ats: ashby, board: demolabs}\n- {ats: Ashby, board: x}\n", "line 2: ats: board \"x\": unknown ats"},
		{"duplicate", "- {ats: lever, board: samplesoft}\n- {ats: lever, board: samplesoft, region: eu}\n", "line 2: lever/samplesoft is listed twice (first on line 1)"},
		{"duplicate ignoring case", "- {ats: ashby, board: demolabs}\n- {ats: lever, board: demolabs}\n- {ats: ashby, board: DemoLabs}\n", "line 3: ashby/DemoLabs is listed twice (first on line 1)"},
		{"not a list", "ats: ashby\nboard: demolabs\n", "cannot unmarshal"},
		{"empty", "# nothing yet\n", "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBoards(writeBoards(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "ats: "+BoardsFile+": ") {
				t.Errorf("err = %v, want it to name the file", err)
			}
		})
	}
}

func TestLoadBoardsMissingFile(t *testing.T) {
	_, err := LoadBoards(t.TempDir())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist in the chain", err)
	}
}

func TestDemoBoardsMatchExample(t *testing.T) {
	got, err := LoadBoardsFile(filepath.Join("..", "..", "examples", "ats-boards.demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, DemoBoards()) {
		t.Fatalf("examples/ats-boards.demo.yaml = %+v\nDemoBoards() = %+v", got, DemoBoards())
	}
	ats := map[string]bool{}
	for _, b := range DemoBoards() {
		if err := b.Validate(); err != nil {
			t.Error(err)
		}
		ats[b.ATS] = true
	}
	if len(ats) != 3 {
		t.Errorf("DemoBoards covers %d ATSs, want all 3", len(ats))
	}
}

// fetchFixture asks the fixture transport in dir for b's API URL.
func fetchFixture(t *testing.T, ft *fetch.FixtureTransport, b Board) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, b.APIURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ft.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestDemoBoardsAnsweredByFixtures(t *testing.T) {
	ft, err := fetch.NewFixtureTransport("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range DemoBoards() {
		status, body := fetchFixture(t, ft, b)
		if status != http.StatusOK {
			t.Errorf("%s: status %d, want 200", b, status)
			continue
		}
		ps, err := Parse(b, body)
		if err != nil || len(ps) == 0 {
			t.Errorf("%s: %d postings, err %v", b, len(ps), err)
		}
	}
}

// TestLaterFixtures pins what testdata/later changes against testdata: one posting gone
// from Greenhouse, one new on Lever, the EU Lever board unchanged, and Ashby failing
// (no manifest entry, so a 404: the fixture transport serves no other status).
func TestLaterFixtures(t *testing.T) {
	before, err := fetch.NewFixtureTransport("testdata")
	if err != nil {
		t.Fatal(err)
	}
	after, err := fetch.NewFixtureTransport(filepath.Join("testdata", "later"))
	if err != nil {
		t.Fatal(err)
	}
	ids := func(ft *fetch.FixtureTransport, b Board) []string {
		status, body := fetchFixture(t, ft, b)
		if status != http.StatusOK {
			return nil
		}
		ps, err := Parse(b, body)
		if err != nil {
			t.Fatalf("%s: %v", b, err)
		}
		var out []string
		for _, p := range ps {
			out = append(out, SourceJobID(b, p))
		}
		return out
	}
	demo := DemoBoards()
	gh, lv, eu, ab := demo[0], demo[1], demo[2], demo[3]

	if got, want := ids(after, gh), []string{"examplecorp/4000000001", "examplecorp/4000000003"}; !reflect.DeepEqual(got, want) {
		t.Errorf("greenhouse later = %v, want %v", got, want)
	}
	if got := ids(after, lv); len(got) != 4 || got[3] != "samplesoft/a1b2c3d4-0000-4000-8000-000000000004" ||
		!reflect.DeepEqual(got[:3], ids(before, lv)) {
		t.Errorf("lever later = %v, want the first three plus one new", got)
	}
	if got, want := ids(after, eu), ids(before, eu); !reflect.DeepEqual(got, want) {
		t.Errorf("lever eu later = %v, want unchanged %v", got, want)
	}
	if status, _ := fetchFixture(t, after, ab); status != http.StatusNotFound {
		t.Errorf("ashby later: status %d, want 404", status)
	}
}

func TestParseUnknownATS(t *testing.T) {
	_, err := Parse(Board{ATS: "workday", Board: "x"}, []byte(`{}`))
	if err == nil || errors.Is(err, ErrParserBroke) {
		t.Fatalf("err = %v, want a plain error (a bad board is not a broken parser)", err)
	}
}

func TestIDsAndProvenance(t *testing.T) {
	b := Board{ATS: Lever, Board: "SampleSoft"}
	if got := SourceJobID(b, Posting{ID: "Ab-1"}); got != "samplesoft/Ab-1" {
		t.Errorf("SourceJobID = %q", got)
	}
	if got := SourceJobID(Board{ATS: Ashby, Board: "other"}, Posting{ID: "Ab-1"}); got == SourceJobID(b, Posting{ID: "Ab-1"}) {
		t.Error("two boards gave the same SourceJobID")
	}
	if got := DerivedBy(Ashby); got != "ashby-parser@1" {
		t.Errorf("DerivedBy = %q", got)
	}
	if got := SourceFixture(Greenhouse); got != "greenhouse-fixture" {
		t.Errorf("SourceFixture = %q", got)
	}
}

func TestWorkplaceRule(t *testing.T) {
	for _, tc := range []struct{ loc, value, want string }{
		{"Exampleville", "remote", "Exampleville (Remote)"},
		{"Exampleville", "Remote", "Exampleville (Remote)"},
		{"Remote - Europe", "remote", "Remote - Europe"},
		{"Fully REMOTE", "Remote", "Fully REMOTE"},
		{"Exampleville", "hybrid", "Exampleville (Hybrid)"},
		{"Exampleville, hybrid", "Hybrid", "Exampleville, hybrid"},
		{"Exampleville", "on-site", "Exampleville (On-site)"},
		{"Exampleville", "OnSite", "Exampleville (On-site)"},
		{"Exampleville (onsite)", "on-site", "Exampleville (onsite)"},
		{"Exampleville, On-site", "OnSite", "Exampleville, On-site"},
		{"Exampleville - in office", "OnSite", "Exampleville - in office"},
		{"Exampleville", "unspecified", "Exampleville"},
		{"Exampleville", "", "Exampleville"},
		{"Exampleville", "flexible", "Exampleville"},
		{"", "remote", "Remote"},
		{"  ", "OnSite", "On-site"},
		{"", "", ""},
		{"Exampleville (Remote)", "hybrid", "Exampleville (Remote) (Hybrid)"},
	} {
		if got := withWorkplace(tc.loc, workplaceKind(tc.value)); got != tc.want {
			t.Errorf("withWorkplace(%q, %q) = %q, want %q", tc.loc, tc.value, got, tc.want)
		}
	}
}

func TestIDText(t *testing.T) {
	for raw, want := range map[string]string{
		`4000000001`:       "4000000001",
		`9007199254740993`: "9007199254740993", // past float64's exact range
		`"abc-1"`:          "abc-1",
		`" spaced "`:       "spaced",
		`null`:             "",
		``:                 "",
		`1.5`:              "1.5",
		`{"nested":true}`:  "",
	} {
		if got := idText([]byte(raw)); got != want {
			t.Errorf("idText(%s) = %q, want %q", raw, got, want)
		}
	}
}
