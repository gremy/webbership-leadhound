package boardlist

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ats"
)

func TestFromURL(t *testing.T) {
	gh := func(s string) ats.Board { return ats.Board{ATS: ats.Greenhouse, Board: s} }
	lv := func(s string) ats.Board { return ats.Board{ATS: ats.Lever, Board: s} }
	ok := []struct {
		in   string
		want ats.Board
	}{
		{"https://boards.greenhouse.io/examplecorp", gh("examplecorp")},
		{"https://job-boards.greenhouse.io/examplecorp/jobs/123?gh_src=x", gh("examplecorp")},
		{"http://BOARDS.Greenhouse.IO/ExampleCorp", gh("examplecorp")},
		{"boards.greenhouse.io/examplecorp/", gh("examplecorp")},
		{"https://boards.greenhouse.io./examplecorp", gh("examplecorp")},
		{"https://jobs.lever.co/samplesoft", lv("samplesoft")},
		{"https://jobs.lever.co/samplesoft/0000-aaaa/apply", lv("samplesoft")},
		{"https://jobs.eu.lever.co/beispielwerk", ats.Board{ATS: ats.Lever, Board: "beispielwerk", Region: ats.RegionEU}},
		{"https://jobs.ashbyhq.com/demolabs?utm_source=x", ats.Board{ATS: ats.Ashby, Board: "demolabs"}},
		{"https://jobs.ashbyhq.com/demo.labs", ats.Board{ATS: ats.Ashby, Board: "demo.labs"}},
		{"https://jobs.ashbyhq.com/demo-labs_2#top", ats.Board{ATS: ats.Ashby, Board: "demo-labs_2"}},
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
		{"", ErrBadURL},
		{"ftp://boards.greenhouse.io/examplecorp", ErrBadURL},
		{"https://", ErrBadURL},
		{"https://job-boards.eu.greenhouse.io/examplecorp", ErrUnsupportedHost},
		{"https://apply.workable.com/examplecorp", ErrUnsupportedHost},
		{"https://evil.jobs.lever.co/samplesoft", ErrUnsupportedHost},
		{"https://jobs.lever.co.example/samplesoft", ErrUnsupportedHost},
		{"https://greenhouse.io/examplecorp", ErrUnsupportedHost},
		{"https://boards.greenhouse.io/", ErrNoSlug},
		{"https://jobs.lever.co", ErrNoSlug},
		{"https://boards.greenhouse.io/embed/job_board?for=examplecorp", ErrNotBoardPath},
		{"https://boards.greenhouse.io/v1/boards/examplecorp/jobs", ErrNotBoardPath},
		{"https://boards.greenhouse.io/include/x.js", ErrNotBoardPath},
		{"https://jobs.ashbyhq.com/api/non-user-graphql", ErrNotBoardPath},
		{"https://jobs.lever.co/favicon.ico", ErrNotBoardPath},
		{"https://jobs.ashbyhq.com/robots.txt", ErrNotBoardPath},
		{"https://jobs.lever.co/sitemap.xml", ErrNotBoardPath},
		{"https://jobs.ashbyhq.com/_next/static/x.js", ErrBadSlug},
		{"https://jobs.lever.co/%20space", ErrBadSlug},
		{"https://jobs.lever.co/" + strings.Repeat("a", 101), ErrBadSlug},
	}
	for _, c := range bad {
		got, err := FromURL(c.in)
		if !errors.Is(err, c.want) {
			t.Errorf("FromURL(%q) = %+v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

func TestParseCSV(t *testing.T) {
	in := "Name,Slug,URL,extra\n" +
		"Example Corp,examplecorp,https://job-boards.greenhouse.io/examplecorp,x\n" +
		"SampleSoft,SampleSoft,https://jobs.lever.co/samplesoft,\n" +
		"No Slug Col,,https://jobs.eu.lever.co/beispielwerk\n" +
		"Mismatch,othercorp,https://jobs.ashbyhq.com/demolabs\n" +
		"Elsewhere,elsewhere,https://apply.workable.com/elsewhere\n" +
		"\"Quoted, Inc\",demolabs,https://jobs.ashbyhq.com/demolabs\n" +
		"Embed,examplecorp,https://boards.greenhouse.io/embed/job_board\n" +
		"Short\n"
	entries, refused, err := ParseCSV(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Board: ats.Board{ATS: ats.Greenhouse, Board: "examplecorp", Name: "Example Corp"}, Name: "Example Corp"},
		{Board: ats.Board{ATS: ats.Lever, Board: "samplesoft", Name: "SampleSoft"}, Name: "SampleSoft"},
		{Board: ats.Board{ATS: ats.Lever, Board: "beispielwerk", Region: ats.RegionEU, Name: "No Slug Col"}, Name: "No Slug Col"},
		{Board: ats.Board{ATS: ats.Ashby, Board: "demolabs", Name: "Quoted, Inc"}, Name: "Quoted, Inc"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("entries\n got %+v\nwant %+v", entries, want)
	}
	wantRefused := []Refused{
		{Line: 5, Text: "https://jobs.ashbyhq.com/demolabs", Reason: ErrSlugMismatch.Error()},
		{Line: 6, Text: "https://apply.workable.com/elsewhere", Reason: ErrUnsupportedHost.Error()},
		{Line: 8, Text: "https://boards.greenhouse.io/embed/job_board", Reason: ErrNotBoardPath.Error()},
		{Line: 9, Text: "", Reason: ErrBadURL.Error()},
	}
	if !reflect.DeepEqual(refused, wantRefused) {
		t.Errorf("refused\n got %+v\nwant %+v", refused, wantRefused)
	}
}

func TestParseCSVBadLineContinues(t *testing.T) {
	in := "name,slug,url\nBad \"quote,x,https://jobs.lever.co/x\nGood,samplesoft,https://jobs.lever.co/samplesoft\n"
	entries, refused, err := ParseCSV(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Board.Board != "samplesoft" {
		t.Errorf("entries %+v", entries)
	}
	if len(refused) != 1 || refused[0].Reason != ErrBadCSVLine.Error() || refused[0].Line != 2 {
		t.Errorf("refused %+v", refused)
	}
}

func TestParseCSVHeader(t *testing.T) {
	for _, in := range []string{"", "name,url\nx,https://jobs.lever.co/x\n", "https://jobs.lever.co/x\n"} {
		if _, _, err := ParseCSV(strings.NewReader(in)); err == nil {
			t.Errorf("ParseCSV(%q): no error", in)
		}
	}
	entries, refused, err := ParseCSV(strings.NewReader("\ufeffurl,slug,name\n"))
	if err != nil || len(entries) != 0 || len(refused) != 0 {
		t.Errorf("header only with BOM: %v %v %v", entries, refused, err)
	}
}

func TestParseCDX(t *testing.T) {
	in := `{"url": "https://boards.greenhouse.io/examplecorp/jobs/1", "timestamp": "20990101000000"}
{"url": "https://boards.greenhouse.io/examplecorp/jobs/2"}

{"url": "https://jobs.lever.co/samplesoft"}
{"url": "https://boards.greenhouse.io/embed/job_board?for=examplecorp"}
{"url": "https://jobs.lever.co/robots.txt"}
{"url": "https://other.example/samplesoft"}
{"url": "https://job-boards.eu.greenhouse.io/examplecorp"}
not json
{"urlkey": "io,greenhouse,boards)/x"}
{"url": "https://jobs.eu.lever.co/beispielwerk/abc"}
{"url": "https://jobs.ashbyhq.com/demolabs"}
`
	entries, refused, skipped, err := ParseCDX(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Board: ats.Board{ATS: ats.Greenhouse, Board: "examplecorp"}},
		{Board: ats.Board{ATS: ats.Lever, Board: "samplesoft"}},
		{Board: ats.Board{ATS: ats.Lever, Board: "beispielwerk", Region: ats.RegionEU}},
		{Board: ats.Board{ATS: ats.Ashby, Board: "demolabs"}},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("entries\n got %+v\nwant %+v", entries, want)
	}
	if skipped != 2 {
		t.Errorf("skipped %d, want 2", skipped)
	}
	var lines []int
	for _, r := range refused {
		lines = append(lines, r.Line)
	}
	if !reflect.DeepEqual(lines, []int{5, 6, 9, 10}) {
		t.Errorf("refused lines %v (%+v)", lines, refused)
	}
	if refused[0].Reason != ErrNotBoardPath.Error() || refused[2].Reason != ErrBadCDXLine.Error() {
		t.Errorf("refused reasons %+v", refused)
	}
}

func TestDedupe(t *testing.T) {
	a := Entry{Board: ats.Board{ATS: ats.Lever, Board: "samplesoft"}, Name: "first"}
	b := Entry{Board: ats.Board{ATS: ats.Lever, Board: "samplesoft"}, Name: "second"}
	eu := Entry{Board: ats.Board{ATS: ats.Lever, Board: "samplesoft", Region: ats.RegionEU}}
	gh := Entry{Board: ats.Board{ATS: ats.Greenhouse, Board: "samplesoft"}}
	got := Dedupe([]Entry{a, b, eu, gh, eu})
	if !reflect.DeepEqual(got, []Entry{a, eu, gh}) {
		t.Errorf("Dedupe = %+v", got)
	}
	if got := Dedupe(nil); len(got) != 0 {
		t.Errorf("Dedupe(nil) = %+v", got)
	}
}

func TestWriteCSVRoundTrip(t *testing.T) {
	in := []Entry{
		{Board: ats.Board{ATS: ats.Lever, Board: "samplesoft", Region: ats.RegionEU}},
		{Board: ats.Board{ATS: ats.Lever, Board: "samplesoft", Name: "SampleSoft"}, Name: "SampleSoft"},
		{Board: ats.Board{ATS: ats.Ashby, Board: "demolabs", Name: "Demo, \"Labs\""}, Name: "Demo, \"Labs\""},
		{Board: ats.Board{ATS: ats.Greenhouse, Board: "examplecorp"}},
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, in); err != nil {
		t.Fatal(err)
	}
	wantText := "name,slug,url\n" +
		"\"Demo, \"\"Labs\"\"\",demolabs,https://jobs.ashbyhq.com/demolabs\n" +
		",examplecorp,https://job-boards.greenhouse.io/examplecorp\n" +
		"SampleSoft,samplesoft,https://jobs.lever.co/samplesoft\n" +
		",samplesoft,https://jobs.eu.lever.co/samplesoft\n"
	if buf.String() != wantText {
		t.Errorf("WriteCSV:\n%s\nwant:\n%s", buf.String(), wantText)
	}
	back, refused, err := ParseCSV(&buf)
	if err != nil || len(refused) != 0 {
		t.Fatalf("read back: %v %+v", err, refused)
	}
	want := []Entry{in[2], in[3], in[1], in[0]}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("round trip\n got %+v\nwant %+v", back, want)
	}
	if in[0].Board.Region != ats.RegionEU {
		t.Error("WriteCSV reordered its input")
	}
}

func TestBoardURLMapsBack(t *testing.T) {
	for _, b := range ats.DemoBoards() {
		b.Name = ""
		got, err := FromURL(BoardURL(b))
		if err != nil || got != b {
			t.Errorf("FromURL(BoardURL(%+v)) = %+v, %v", b, got, err)
		}
	}
}
