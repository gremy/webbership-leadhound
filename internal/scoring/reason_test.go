package scoring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func TestProbabilitiesSortedAndStable(t *testing.T) {
	p := map[string]float64{"unclear": 0.07, "fail": 0.11, "pass": 0.82}
	if got := Probabilities(p); got != "p(pass)=0.82 p(fail)=0.11 p(unclear)=0.07" {
		t.Errorf("got %q", got)
	}
	tie := map[string]float64{"unclear": 0.5, "fail": 0.5}
	for range 20 {
		if got := Probabilities(tie); got != "p(fail)=0.50 p(unclear)=0.50" {
			t.Fatalf("tie: got %q", got)
		}
	}
}

func TestEvidenceLines(t *testing.T) {
	gs := []store.GateResult{
		{Gate: "workplace", Verdict: "flag", Snippet: "hybrid, 2 days in Exampleville"},
		{Gate: "location", Verdict: "pass", Snippet: ""},
		{Gate: "work_rights", Verdict: "pass", Snippet: "Exampleland citizens welcome"},
	}
	desc := "We use Rust daily.\nGood coffee.\nRusty skills are fine.\nPostgres and Rust together.\nRust again."
	ev := Evidence{Title: "Senior Rust Engineer", Description: desc, Gates: gs,
		Keywords: map[string][]string{"stack": {"rust", "postgres"}, "seniority": {"senior"}}}
	for _, tc := range []struct {
		criterion string
		want      []string
	}{
		// Whole words only: "Rusty" is not "rust"; two lines at most.
		{"stack", []string{"We use Rust daily.", "Postgres and Rust together."}},
		{"seniority", nil}, // keywords that name nothing: no fallback to the title
		{"title", []string{"Senior Rust Engineer"}},
		{"location", []string{"hybrid, 2 days in Exampleville"}}, // the location snippet is empty, so workplace's
		{"work_rights", []string{"Exampleland citizens welcome"}},
	} {
		got := EvidenceLines(tc.criterion, ev)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: got %q, want %q", tc.criterion, got, tc.want)
		}
	}
	// No snippet and no keywords: the reason says so.
	r := Reason(testCriteria[1], answer(Pass, 0.9), Pass, 0.7, Evidence{Description: "Nothing here."})
	if !strings.Contains(r, "\n"+NoEvidence+"\n") {
		t.Errorf("reason without evidence: %q", r)
	}
}

func TestEvidenceWindow(t *testing.T) {
	line := strings.Repeat("filler words here ", 12) + "we write Rust every day " + strings.Repeat("and more filler text ", 12)
	got := keywordLines(line, []string{"rust"})
	if len(got) != 1 {
		t.Fatalf("got %d lines", len(got))
	}
	if n := utf8.RuneCountInString(got[0]); n > maxEvidenceRunes {
		t.Errorf("line is %d runes, want at most %d", n, maxEvidenceRunes)
	}
	if !strings.Contains(got[0], "Rust") || !strings.HasPrefix(got[0], "…") || !strings.HasSuffix(got[0], "…") {
		t.Errorf("window %q", got[0])
	}
	if w := window("short", 0, 5, 120); w != "short" {
		t.Errorf("short line changed: %q", w)
	}
}

// fictionalCard is a recruiter card as LinkedIn places it, beside the description; its
// name and words are fictional and must never reach a reason.
const fictionalCard = `<div class="message-the-` + `recruiter"><p>Meet the hiring team</p>` +
	`<div class="base-card"><h3>Ottoline Brackwater</h3><h4>Talent Partner at Northwind Labs, hiring Rust engineers</h4>` +
	`<a href="https://www.linkedin.com/login">Message Ottoline Brackwater</a></div></div>`

// TestNoRecruiterTextInReasons is the invariant sweep: over the detail_recruiter_removed
// fixture with a recruiter card put back beside the description, no decision reason
// carries any of the card's text, even with keywords that would match it.
func TestNoRecruiterTextInReasons(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "linkedin", "testdata", "detail_recruiter_removed.html"))
	if err != nil {
		t.Fatal(err)
	}
	const anchor = `<div class="show-more-less-html__markup`
	i := strings.Index(string(page), anchor)
	if i < 0 {
		t.Fatal("fixture has no description markup")
	}
	withCard := string(page[:i]) + fictionalCard + string(page[i:])
	d, err := linkedin.ParseDetail([]byte(withCard), time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	in := store.ScoreInput{JobID: 1, ObservationID: 1, Company: d.Company, Title: d.Title, Location: d.Location,
		Seniority: d.Seniority, EmploymentType: d.EmploymentType, Description: postingguard.Clean(d.Description, true).Visible,
		Gates: []store.GateResult{{Gate: "location", Verdict: "flag", Snippet: d.Location}}}
	keywords := map[string][]string{
		"stack":     {"go", "kubernetes", "rust", "hiring"},
		"seniority": {"ottoline", "brackwater", "talent", "message", "team"},
	}
	ds := Decisions(in, allPass(), testCriteria, 0.7, keywords, "fake", []byte("h"))
	if len(ds) != 5 {
		t.Fatalf("%d decisions", len(ds))
	}
	cardWords := []string{"Ottoline", "Brackwater", "Talent Partner", "hiring team", "Meet the", "Message", "hiring Rust"}
	for _, dec := range ds {
		for _, w := range cardWords {
			if strings.Contains(dec.Reason, w) {
				t.Errorf("%s reason carries recruiter card text %q:\n%s", dec.Criterion, w, dec.Reason)
			}
		}
	}
	// The sweep must have quoted something, or it proves nothing.
	if !strings.Contains(ds[1].Reason, "Go") {
		t.Errorf("stack reason quotes no description line:\n%s", ds[1].Reason)
	}
}

func TestReasonUsesChosenOption(t *testing.T) {
	r := Reason(testCriteria[0], ai.Answer{Choice: Fail, Confidence: 0.8, Probabilities: map[string]float64{Fail: 0.8, Pass: 0.2}}, Fail, 0.7, Evidence{Title: "Sales Lead"})
	want := "title: Not engineering.\n\"Sales Lead\"\np(fail)=0.80 p(pass)=0.20"
	if r != want {
		t.Errorf("got\n%s\nwant\n%s", r, want)
	}
}
