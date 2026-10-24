package eval

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
)

var update = flag.Bool("update", false, "rewrite the golden output files")

const examples = "../../examples"

func demoProfile(t *testing.T) candidate.Config {
	t.Helper()
	prof, err := candidate.LoadFile(filepath.Join(examples, "leadhound.demo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return prof
}

// passAll is the keyless fake: every criterion passes at 0.9.
func passAll() *ai.Fake {
	a := ai.Answer{Choice: "pass", Confidence: 0.9, Probabilities: map[string]float64{"pass": 0.9, "fail": 0.05, "unclear": 0.05}}
	m := map[string]ai.Answer{}
	for _, n := range candidate.CriterionNames {
		m[n] = a
	}
	return &ai.Fake{Answers: m}
}

func runDemo(t *testing.T, d ai.Decider, runs int) (Report, string) {
	t.Helper()
	jobs, file, err := Load(examples)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), examples, file, jobs,
		Options{Runs: runs, Decider: d, Profile: demoProfile(t), Now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := rep.Write(&b); err != nil {
		t.Fatal(err)
	}
	return rep, b.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a fixed testdata path
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("output differs from %s (run with -update after reviewing):\n%s", path, got)
	}
}

// TestDemoFakeTable: the demo golden set with the fake gives a deterministic table, and
// the decider is asked on every job, gate-failed or not.
func TestDemoFakeTable(t *testing.T) {
	fake := passAll()
	rep, out := runDemo(t, fake, 3)
	if fake.Calls() != 3*len(rep.Jobs) {
		t.Errorf("decider calls %d, want %d (every job, every run)", fake.Calls(), 3*len(rep.Jobs))
	}
	golden(t, "demo_fake.golden.txt", out)
}

// swinging answers stack differently on every other call, so runs disagree.
type swinging struct{ n int }

func (*swinging) Name() string { return "swinging" }

func (s *swinging) Decide(_ context.Context, _ string, qs []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	s.n++
	out := map[string]ai.Answer{}
	for _, q := range qs {
		out[q.Name] = ai.Answer{Choice: "pass", Confidence: 0.9}
	}
	if s.n%2 == 0 {
		out["stack"] = ai.Answer{Choice: "fail", Confidence: 0.9}
	}
	return out, ai.Usage{InputTokens: 1000}, nil
}

func TestDisagreementsAndCost(t *testing.T) {
	_, out := runDemo(t, &swinging{}, 2)
	golden(t, "demo_swinging.golden.txt", out)
	if !strings.Contains(out, "remote-en stack: pass / fail") || !strings.Contains(out, "input tokens: 10000") {
		t.Errorf("output:\n%s", out)
	}
}

// TestOutputNamesNoTitleOrCompany: the table prints ids only.
func TestOutputNamesNoTitleOrCompany(t *testing.T) {
	_, out := runDemo(t, &swinging{}, 2)
	for _, s := range []string{"Platform Engineer", "Tailspin", "Fabrikam", "Contoso", "Litware"} {
		if strings.Contains(out, s) {
			t.Errorf("output names %q", s)
		}
	}
}

func TestTooLongRetriesShorter(t *testing.T) {
	d := &tooLongOnce{}
	jobs, file, err := Load(examples)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), examples, file, jobs[:1], Options{Runs: 1, Decider: d, Profile: demoProfile(t)}); err != nil {
		t.Fatal(err)
	}
	if d.calls != 2 {
		t.Errorf("calls %d, want 2", d.calls)
	}
}

type tooLongOnce struct{ calls int }

func (*tooLongOnce) Name() string { return "too-long-once" }

func (d *tooLongOnce) Decide(_ context.Context, _ string, qs []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	d.calls++
	if d.calls == 1 {
		return nil, ai.Usage{}, ai.ErrTooLong
	}
	out := map[string]ai.Answer{}
	for _, q := range qs {
		out[q.Name] = ai.Answer{Choice: "pass", Confidence: 0.9}
	}
	return out, ai.Usage{}, nil
}

func TestDeciderErrorFails(t *testing.T) {
	jobs, file, err := Load(examples)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), examples, file, jobs, Options{Runs: 1, Decider: &ai.Fake{Err: ai.ErrUnavailable}, Profile: demoProfile(t)})
	if !errors.Is(err, ai.ErrUnavailable) || !strings.Contains(err.Error(), "remote-en") {
		t.Errorf("err = %v", err)
	}
}

// TestLoadColdStartAndBadLines: an empty directory or file is a clear error, and so is
// every malformed line.
func TestLoadColdStartAndBadLines(t *testing.T) {
	empty := t.TempDir()
	if _, _, err := Load(empty); !errors.Is(err, ErrNoGolden) || !strings.Contains(err.Error(), "jobs.jsonl") {
		t.Errorf("empty dir: %v", err)
	}
	for name, content := range map[string]string{
		"no jobs":         "\n\n",
		"bad json":        `{"id":`,
		"unknown field":   `{"id":"a","raw":"raw/a.html","labels":{},"overall":null,"note":"","extra":1}`,
		"unknown label":   `{"id":"a","raw":"raw/a.html","labels":{"salary":"pass"},"overall":null}`,
		"bad value":       `{"id":"a","raw":"raw/a.html","labels":{"stack":"flag"},"overall":null}`,
		"bad gate value":  `{"id":"a","raw":"raw/a.html","labels":{"language":"unclear"},"overall":null}`,
		"bad overall":     `{"id":"a","raw":"raw/a.html","labels":{},"overall":"maybe"}`,
		"escaping raw":    `{"id":"a","raw":"../a.html","labels":{},"overall":null}`,
		"absolute raw":    `{"id":"a","raw":"/tmp/a.html","labels":{},"overall":null}`,
		"path id":         `{"id":"a/b","raw":"raw/a.html","labels":{},"overall":null}`,
		"duplicate id":    `{"id":"a","raw":"raw/a.html","labels":{},"overall":null}` + "\n" + `{"id":"a","raw":"raw/b.html","labels":{},"overall":null}`,
		"missing raw key": `{"id":"a","labels":{},"overall":null}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "jobs.jsonl"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(dir); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// TestNullLabelsSkippedAndCounted: a null label is neither a match nor a miss.
func TestNullLabelsSkippedAndCounted(t *testing.T) {
	rep, _ := runDemo(t, passAll(), 1)
	for _, rw := range rep.rows() {
		switch rw.name {
		case "work_rights":
			// Labelled on hybrid-en only.
			if rw.nulls != 4 || rw.runs[0] != (tally{match: 1, labelled: 1}) {
				t.Errorf("work_rights row %+v", rw)
			}
		case "gate language":
			if rw.nulls != 0 || rw.runs[0] != (tally{match: 5, labelled: 5}) {
				t.Errorf("language row %+v", rw)
			}
		}
	}
}

// TestPrepareReadsLinkedInAsListedRemote: eval's golden rows are LinkedIn pages, whose
// search lists remote jobs only, so prepare tells the gates so, as the collector does: a
// location field naming a place outside the allowed countries is not read as an office.
func TestPrepareReadsLinkedInAsListedRemote(t *testing.T) {
	// The English fixture, with its location moved outside the allowed places and its
	// "fully remote" line taken out, so only ListedRemote keeps the field from failing.
	page, err := os.ReadFile(filepath.Join("..", "linkedin", "testdata", "detail_offsite.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"Exampleville, Country", "Fully remote within the EU."} {
		if !bytes.Contains(page, []byte(s)) {
			t.Fatalf("fixture no longer carries %q; update this test", s)
		}
	}
	dir := t.TempDir()
	page = bytes.ReplaceAll(page, []byte("Exampleville, Country"), []byte("Berlin, Germany"))
	page = bytes.ReplaceAll(page, []byte("Fully remote within the EU."), []byte("The team is spread across the EU."))
	if err := os.WriteFile(filepath.Join(dir, "page.html"), page, 0o600); err != nil { //nolint:gosec // G703: a fixed name in the test's temp dir
		t.Fatal(err)
	}
	prof := demoProfile(t)
	prof.Candidate.AllowedCities = nil
	opt := Options{Profile: prof, Now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	p, gs, err := prepare(dir, Job{ID: "bare", Raw: "page.html"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	if p.Location != "Berlin, Germany" {
		t.Fatalf("location = %q, want the substituted field", p.Location)
	}
	if len(gs) < 4 {
		t.Fatalf("gates = %+v, want all four (an English page)", gs)
	}
	for _, g := range gs {
		if strings.Contains(g.Reason, "location field") {
			t.Errorf("gate %s = %s (%q); a LinkedIn field must not be read as an office", g.Gate, g.Verdict, g.Reason)
		}
	}
	// The same page from a source that lists every workplace would fail: the test bites.
	obs := gates.Observation{Title: p.Title, Company: p.Company, Location: p.Location, Description: p.Description}
	for _, g := range gates.Evaluate(obs, opt.Profile.Gates(), nil) {
		if g.Gate == gates.GateWorkplace && g.Verdict != gates.Fail {
			t.Errorf("without ListedRemote the workplace gate = %s (%q), want fail", g.Verdict, g.Reason)
		}
	}
}
