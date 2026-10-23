//go:build integration

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Fictional postings. The English one carries a hidden block and a zero-width space, so the
// guard has something to count.
const (
	englishHTML = `<p>We are looking for a senior backend engineer to design and maintain services written in Go.</p>` +
		`<p>You will work closely with product managers and other engineers to ship features every week.</p>` +
		`<p>Fully remote within the EU. We expect strong experience with distributed sys` + "​" + `tems and PostgreSQL.</p>` +
		`<span style="display:none">hidden note for a reader</span>`
	hybridHTML = `<p>We are looking for a senior backend engineer to design and maintain services written in Go.</p>` +
		`<p>You will work closely with product managers and other engineers to ship features every week.</p>` +
		`<p>The role is hybrid, 2 days a week in the office. We expect strong experience with PostgreSQL.</p>`
	frenchHTML = `<p>Nous recherchons un ingénieur backend expérimenté pour concevoir et maintenir des services écrits en Go.</p>` +
		`<p>Vous travaillerez avec les équipes produit pour livrer de nouvelles fonctionnalités chaque semaine.</p>`
)

var english = gates.CandidateGates{Languages: []string{"en"}}

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

// seed makes a job of company with one detail fetch, and returns their ids.
func seed(t *testing.T, q store.Queries, sourceJobID, company string) (jobID, fetchID int64) {
	t.Helper()
	ctx := context.Background()
	runID, err := q.StartRun(ctx, "collect", "test")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.NewFetchRecorder(q, runID, "test")
	if err := rec.Record(ctx, fetch.Record{Kind: fetch.KindDetail, URL: "https://jobs.example/" + sourceJobID, Status: 200, Body: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	companyID, err := q.UpsertCompany(ctx, company, "", "")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err = q.UpsertJob(ctx, store.JobUpsert{Source: "test", SourceJobID: sourceJobID, CompanyID: companyID, Title: "Backend Engineer", URL: "https://jobs.example/" + sourceJobID})
	if err != nil {
		t.Fatal(err)
	}
	return jobID, rec.LastID()
}

func input(jobID, fetchID int64, company, html string) Input {
	return Input{
		JobID: jobID, FetchID: fetchID, DerivedBy: "test-parser@1",
		Title: "Senior Backend Engineer", Company: company, Location: "European Union",
		DescriptionHTML: html, Gates: english,
	}
}

// observe runs Observe, which opens and commits its own transaction.
func observe(t *testing.T, s *store.Store, in Input) ObserveResult {
	t.Helper()
	res, err := Observe(context.Background(), s, in, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
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

func gatesOf(t *testing.T, q store.Queries, jobID int64) map[string]store.GateResult {
	t.Helper()
	rows, err := q.LatestGateResults(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]store.GateResult{}
	for _, r := range rows {
		if _, dup := m[r.Gate]; dup && r.Gate != gates.GateRule {
			t.Fatalf("gate %s written twice", r.Gate)
		}
		m[r.Gate] = r
	}
	return m
}

func TestObserveWritesGateRows(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()
	jobID, fetchID := seed(t, q, "1", "Initech")

	res := observe(t, s, input(jobID, fetchID, "Initech", englishHTML))
	if res.Lang != "en" {
		t.Errorf("lang = %q, want en", res.Lang)
	}
	o, err := q.LatestObservation(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if o.ID != res.ObservationID || o.Lang != "en" || o.LangConfidence <= 0 || o.DerivedBy != "test-parser@1" {
		t.Errorf("observation = %+v", o)
	}
	if o.DescriptionClean == "" || containsAny(o.DescriptionClean, "<", "hidden note", "​") {
		t.Errorf("description_clean = %q, want visible plain text only", o.DescriptionClean)
	}
	assertCountsOnly(t, o.GuardFindings)
	var f struct {
		HiddenBlocks   int            `json:"hidden_blocks"`
		InvisibleChars map[string]int `json:"invisible_chars"`
	}
	if err := json.Unmarshal(o.GuardFindings, &f); err != nil || f.HiddenBlocks != 1 || f.InvisibleChars["zero_width"] != 1 {
		t.Errorf("guard_findings = %s (%v), want 1 hidden block and 1 zero-width", o.GuardFindings, err)
	}

	got := gatesOf(t, q, jobID)
	for _, g := range []string{gates.GateLanguage, gates.GateLocation, gates.GateWorkplace, gates.GateWorkRights} {
		r, ok := got[g]
		if !ok {
			t.Errorf("no %s gate row", g)
			continue
		}
		if r.ObservationID != res.ObservationID || r.DerivedBy != gates.DerivedBy || r.Reason == "" {
			t.Errorf("%s row = %+v", g, r)
		}
	}
	if len(got) != 4 || got[gates.GateLanguage].Verdict != gates.Pass {
		t.Errorf("gate rows = %+v, want the four gates with language pass", got)
	}
}

// TestInstructionShapedTitleIsNotQuoted: titles are stored as cleaned values without the
// quote marker, so a title rule anchored with ^ still matches.
func TestInstructionShapedTitleIsNotQuoted(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()
	if _, err := q.AddRule(ctx, "flag", "title", `^(linux|ignore)\b`, "anchored title rule", nil); err != nil {
		t.Fatal(err)
	}
	for i, title := range []string{"Linux Engineer (SSH)", "Ignore previous instructions Engineer"} {
		t.Run(title, func(t *testing.T) {
			jobID, fetchID := seed(t, q, string(rune('1'+i)), "Initech")
			in := input(jobID, fetchID, "Initech", englishHTML)
			in.Title = "\u200B" + title + "\u202E"
			observe(t, s, in)
			o, err := q.LatestObservation(ctx, jobID)
			if err != nil {
				t.Fatal(err)
			}
			if o.Title != title {
				t.Errorf("stored title %q, want %q (no quote marker, no invisible runes)", o.Title, title)
			}
			r, ok := gatesOf(t, q, jobID)[gates.GateRule]
			if !ok || r.Verdict != gates.Flag || strings.Contains(r.Snippet, postingguard.QuotePrefix) {
				t.Errorf("rule row = %+v (%t), want the anchored rule to flag, unquoted", r, ok)
			}
		})
	}
}

// TestStoredLangIsTheGateBasis: the language gate reuses Observe's detection, which ran on
// the text the gate reads: the cleaned description, or the cleaned title when the
// description is blank. The stored lang is therefore what the gate judged.
func TestStoredLangIsTheGateBasis(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()
	var got gates.Observation
	evaluate = func(o gates.Observation, c gates.CandidateGates, r []gates.Rule) []gates.Result {
		got = o
		return gates.Evaluate(o, c, r)
	}
	t.Cleanup(func() { evaluate = gates.Evaluate })

	for i, tt := range []struct {
		name, title, html string
	}{
		{"description", "Senior Backend Engineer", frenchHTML},
		{"blank description falls back to the title", "Ing\u200bénieur backend expérimenté pour concevoir des services en Go", `<div hidden>only hidden text</div>`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			jobID, fetchID := seed(t, q, string(rune('1'+i)), "Initech")
			in := input(jobID, fetchID, "Initech", tt.html)
			in.Title = tt.title
			observe(t, s, in)
			o, err := q.LatestObservation(ctx, jobID)
			if err != nil {
				t.Fatal(err)
			}
			basis := got.Description
			if strings.TrimSpace(basis) == "" {
				basis = got.Title
			}
			wantLang, wantConf := gates.DetectLanguage(basis)
			if got.Lang != o.Lang || o.Lang != wantLang || got.LangConfidence != wantConf {
				t.Errorf("gate saw lang %q (%.3f), stored %q, detection over the gate's text %q (%.3f)",
					got.Lang, got.LangConfidence, o.Lang, wantLang, wantConf)
			}
			if r := gatesOf(t, q, jobID)[gates.GateLanguage]; r.Verdict != gates.Fail || !strings.Contains(r.Reason, "French") {
				t.Errorf("language row = %+v, want a fail for French", r)
			}
		})
	}
}

// TestObserveStoresSnippets: each gate row keeps the snippet its gate quoted. A flag quotes
// the words that raised it; a gate that matched nothing quotes the description's opening.
func TestObserveStoresSnippets(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()
	jobID, fetchID := seed(t, q, "1", "Initech")

	res := observe(t, s, input(jobID, fetchID, "Initech", hybridHTML))
	got := gatesOf(t, q, jobID)

	wp := got[gates.GateWorkplace]
	if wp.Verdict != gates.Flag || !strings.Contains(wp.Snippet, "hybrid, 2 days a week in the office") {
		t.Errorf("workplace row = %+v, want a flag quoting the hybrid line", wp)
	}
	if n := utf8.RuneCountInString(wp.Snippet); n > 120 {
		t.Errorf("workplace snippet is %d runes, want at most 120", n)
	}

	o, err := q.LatestObservation(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	opening := []rune(strings.Join(strings.Fields(o.DescriptionClean), " "))
	if len(opening) > 120 {
		opening = opening[:120]
	}
	wr := got[gates.GateWorkRights]
	if wr.Verdict != gates.Pass || wr.Snippet == "" || wr.Snippet != string(opening) {
		t.Errorf("work_rights row = %+v, want a pass quoting the description's first 120 runes %q", wr, string(opening))
	}

	// What is stored is what the gates returned, gate by gate.
	for _, r := range res.Gates {
		if got[r.Gate].Snippet != r.Snippet {
			t.Errorf("%s snippet stored %q, gates gave %q", r.Gate, got[r.Gate].Snippet, r.Snippet)
		}
	}
}

func TestObserveFrenchShortCircuits(t *testing.T) {
	s := openStore(t)
	q := s.Q()
	jobID, fetchID := seed(t, q, "1", "Initech")
	observe(t, s, input(jobID, fetchID, "Initech", frenchHTML))
	got := gatesOf(t, q, jobID)
	if len(got) != 1 || got[gates.GateLanguage].Verdict != gates.Fail {
		t.Fatalf("gate rows = %+v, want only language fail", got)
	}

	// With a matching rule: the rule row and the language row, nothing else.
	if _, err := q.AddRule(context.Background(), "flag", "title", "senior", "seniority check", nil); err != nil {
		t.Fatal(err)
	}
	observe(t, s, input(jobID, fetchID, "Initech", frenchHTML))
	got = gatesOf(t, q, jobID)
	if len(got) != 2 || got[gates.GateRule].Verdict != gates.Flag || got[gates.GateLanguage].Verdict != gates.Fail {
		t.Fatalf("gate rows = %+v, want rule flag + language fail", got)
	}
	if lang := count(t, `SELECT count(*) FROM job_observation WHERE lang = 'fr'`); lang != 2 {
		t.Errorf("french observations = %d, want 2", lang)
	}
}

// TestNoObservationWithoutGates: when a gate row cannot be written, when the gates give no
// result, or when the caller's own write fails, the observation is rolled back with it.
func TestNoObservationWithoutGates(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()
	jobID, fetchID := seed(t, q, "1", "Initech")
	nothingCommitted := func(t *testing.T) {
		t.Helper()
		for _, table := range []string{"job_observation", "gate_result", "status_event"} {
			if n := count(t, `SELECT count(*) FROM `+table); n != 0 {
				t.Errorf("%s rows = %d after a failed Observe, want 0", table, n)
			}
		}
	}
	statusNew := func(q store.Queries, _ ObserveResult) error {
		_, err := q.InsertStatusEvent(ctx, jobID, "new", "", "system", nil)
		return err
	}

	t.Run("bad gate verdict", func(t *testing.T) {
		evaluate = func(gates.Observation, gates.CandidateGates, []gates.Rule) []gates.Result {
			return []gates.Result{
				{Gate: gates.GateLanguage, Verdict: gates.Pass, Reason: "ok"},
				{Gate: gates.GateLocation, Verdict: "maybe", Reason: "violates the verdict CHECK"},
			}
		}
		t.Cleanup(func() { evaluate = gates.Evaluate })
		if _, err := Observe(ctx, s, input(jobID, fetchID, "Initech", englishHTML), statusNew); err == nil {
			t.Fatal("Observe with a bad gate verdict succeeded")
		}
		nothingCommitted(t)
	})

	t.Run("no gate result", func(t *testing.T) {
		evaluate = func(gates.Observation, gates.CandidateGates, []gates.Rule) []gates.Result { return nil }
		t.Cleanup(func() { evaluate = gates.Evaluate })
		if _, err := Observe(ctx, s, input(jobID, fetchID, "Initech", englishHTML), statusNew); err == nil {
			t.Fatal("Observe with no gate result succeeded")
		}
		nothingCommitted(t)
	})

	t.Run("failing callback", func(t *testing.T) {
		boom := errors.New("boom")
		var seen ObserveResult
		_, err := Observe(ctx, s, input(jobID, fetchID, "Initech", englishHTML), func(q store.Queries, res ObserveResult) error {
			seen = res
			if err := statusNew(q, res); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("Observe = %v, want the callback's error", err)
		}
		if seen.ObservationID == 0 || len(seen.Gates) == 0 {
			t.Errorf("callback saw %+v, want the written observation and its gates", seen)
		}
		nothingCommitted(t)
	})

	t.Run("callback commits with the observation", func(t *testing.T) {
		res, err := Observe(ctx, s, input(jobID, fetchID, "Initech", englishHTML), statusNew)
		if err != nil {
			t.Fatal(err)
		}
		if n := count(t, `SELECT count(*) FROM gate_result WHERE observation_id = $1`, res.ObservationID); n != len(res.Gates) {
			t.Errorf("gate rows = %d, want %d", n, len(res.Gates))
		}
		if st, err := q.LatestStatus(ctx, jobID); err != nil || st.Status != "new" {
			t.Errorf("status = %+v, %v; want new", st, err)
		}
	})
}

// TestObserveStoresCleanedFields: title, company and location are cleaned like the
// description before they are stored or gated, so no invisible rune reaches
// job_observation or a gate snippet.
func TestObserveStoresCleanedFields(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()
	// Zero-width, bidi and tag characters, plus a line break, in every one-line field.
	const zw, rlo, tagA = "\u200b", "\u202e", "\U000E0041"
	company := "Ini" + zw + "tech" + tagA + " Sta" + rlo + "ffing"
	jobID, fetchID := seed(t, q, "1", "Initech Staffing")
	in := input(jobID, fetchID, company, englishHTML+`<p>The role is hybrid, 2 days a week in the office.</p>`)
	in.Title = "Senior" + zw + " Back" + rlo + "end\nEngineer" + tagA
	in.Location = "Euro" + zw + "pean " + tagA + "Uni" + rlo + "on\r\n"
	for _, r := range []struct{ field, pattern string }{
		{"title", "senior backend engineer"}, {"company", "initech staffing"}, {"location", "european union"},
	} {
		if _, err := q.AddRule(ctx, "flag", r.field, r.pattern, "cleaned "+r.field, nil); err != nil {
			t.Fatal(err)
		}
	}

	res := observe(t, s, in)
	o, err := q.LatestObservation(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if o.Title != "Senior Backend Engineer" || o.LocationText != "European Union" {
		t.Errorf("stored title %q, location %q; want them cleaned to one line", o.Title, o.LocationText)
	}
	for label, v := range map[string]string{"title": o.Title, "location_text": o.LocationText, "description_clean": o.DescriptionClean} {
		if r, at, found := postingguard.FirstInvisible(v); found {
			t.Errorf("%s holds invisible U+%04X at byte %d: %q", label, r, at, v)
		}
	}
	rows, err := q.LatestGateResults(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	rules := 0
	for _, g := range rows {
		if g.Gate == gates.GateRule {
			rules++ // each rule matched only because its field was cleaned first
		}
		if r, at, found := postingguard.FirstInvisible(g.Snippet); found {
			t.Errorf("%s snippet holds invisible U+%04X at byte %d: %q", g.Gate, r, at, g.Snippet)
		}
		if strings.ContainsAny(g.Snippet, "\n\r") {
			t.Errorf("%s snippet spans lines: %q", g.Gate, g.Snippet)
		}
	}
	if rules != 3 {
		t.Errorf("rule rows = %d, want 3 (title, company and location rules on the cleaned fields): %+v", rules, rows)
	}
	if len(res.Gates) != len(rows) {
		t.Errorf("stored %d gate rows, Observe gave %d", len(rows), len(res.Gates))
	}
}

func TestCompanyRuleAppliesToNextObservation(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()

	// Job A is seen and `new` before the rule exists.
	jobA, fetchA := seed(t, q, "1", "Acme Staffing")
	observe(t, s, input(jobA, fetchA, "Acme Staffing", englishHTML))
	if _, err := q.InsertStatusEvent(ctx, jobA, "new", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := gatesOf(t, q, jobA)[gates.GateRule]; ok {
		t.Fatal("rule row before any rule exists")
	}

	ruleID, err := q.AddRule(ctx, "skip", "company", `acme\s+staffing`, "staffing agency", &jobA)
	if err != nil {
		t.Fatal(err)
	}
	// Adding a rule changes no status.
	if n := count(t, `SELECT count(*) FROM status_event`); n != 1 {
		t.Fatalf("status events = %d after AddRule, want 1", n)
	}

	// The next new job of that company fails the rule gate.
	jobB, fetchB := seed(t, q, "2", "Acme Staffing")
	observe(t, s, input(jobB, fetchB, "Acme Staffing", englishHTML))
	r, ok := gatesOf(t, q, jobB)[gates.GateRule]
	if !ok || r.Verdict != gates.Fail || r.RuleID == nil || *r.RuleID != ruleID {
		t.Fatalf("job B rule row = %+v (%t), want fail by rule %d", r, ok, ruleID)
	}

	// The `new` job A fails it on its next observation; its status stays new.
	observe(t, s, input(jobA, fetchA, "Acme Staffing", englishHTML))
	if r := gatesOf(t, q, jobA)[gates.GateRule]; r.Verdict != gates.Fail || r.RuleID == nil || *r.RuleID != ruleID {
		t.Fatalf("job A next observation rule row = %+v, want fail", r)
	}
	if st, err := q.LatestStatus(ctx, jobA); err != nil || st.Status != "new" {
		t.Fatalf("job A status = %+v, %v; want new", st, err)
	}

	// Another company is untouched.
	jobC, fetchC := seed(t, q, "3", "Initech")
	observe(t, s, input(jobC, fetchC, "Initech", englishHTML))
	if _, ok := gatesOf(t, q, jobC)[gates.GateRule]; ok {
		t.Fatal("rule matched another company")
	}

	// A disabled rule no longer applies.
	if err := q.DisableRule(ctx, ruleID, "agency is fine after all"); err != nil {
		t.Fatal(err)
	}
	observe(t, s, input(jobB, fetchB, "Acme Staffing", englishHTML))
	if _, ok := gatesOf(t, q, jobB)[gates.GateRule]; ok {
		t.Fatal("disabled rule still applied")
	}
}

// TestRuleAfterAppliedKeepsStatus is the grid cell "applied × rule added matching it":
// the status is unchanged and no status event is written, even on the next observation.
func TestRuleAfterAppliedKeepsStatus(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	q := s.Q()
	jobID, fetchID := seed(t, q, "1", "Acme Staffing")
	observe(t, s, input(jobID, fetchID, "Acme Staffing", englishHTML))
	for _, st := range []string{"new", "shortlisted", "applied"} {
		if _, err := q.InsertStatusEvent(ctx, jobID, st, "", "gremy", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.AddRule(ctx, "skip", "company", "acme", "staffing agency", nil); err != nil {
		t.Fatal(err)
	}
	observe(t, s, input(jobID, fetchID, "Acme Staffing", englishHTML))
	if r := gatesOf(t, q, jobID)[gates.GateRule]; r.Verdict != gates.Fail {
		t.Fatalf("rule row = %+v, want the gate to record the fail", r)
	}
	if n := count(t, `SELECT count(*) FROM status_event WHERE job_id = $1`, jobID); n != 3 {
		t.Errorf("status events = %d, want 3 (none written by the rule)", n)
	}
	if st, err := q.LatestStatus(ctx, jobID); err != nil || st.Status != "applied" {
		t.Errorf("status = %+v, %v; want applied", st, err)
	}
}

// assertCountsOnly checks guard_findings holds numbers only, nested objects of numbers
// included: no excerpt of the posting is ever stored.
func assertCountsOnly(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("guard_findings %s: %v", raw, err)
	}
	if len(m) == 0 {
		t.Fatalf("guard_findings is empty: %s", raw)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case float64:
		case map[string]any:
			for k, x := range v {
				walk(path+"."+k, x)
			}
		default:
			t.Errorf("guard_findings%s = %v (%T), want a number", path, v, v)
		}
	}
	walk("", m)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
