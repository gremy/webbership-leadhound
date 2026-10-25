package brief

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// day is the brief's day: Friday 23 October 2026.
var day = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)

func ago(days int) *time.Time {
	t := day.Add(9*time.Hour).AddDate(0, 0, -days)
	return &t
}

func count(n int) *int { return &n }

func healthyLinkedIn(n int) []Source {
	return []Source{{Name: "LinkedIn", State: StateOK, New: n, Canary: CanaryOK}}
}

func applyJobs() []Job {
	return []Job{
		{
			ID: 101, Title: "Senior Go Engineer", Company: "Northwind Labs", Location: "remote EU",
			URL: "https://www.linkedin.com/jobs/view/1000000001", ApplicantCount: count(12),
			PostedAt: ago(0), ApplyType: "offsite",
			Criteria: []Criterion{{Name: "stack", Verdict: "pass", Note: "Go, Postgres"}, {Name: "seniority", Verdict: "pass", Note: "senior"}, {Name: "location", Verdict: "pass"}},
			Evidence: []string{"Build Go services on Postgres", "Fully remote within the EU"},
		},
		{
			ID: 205, Title: "Backend Engineer (Go)", Company: "Contoso Systems", Location: "Bucharest, Romania",
			URL: "https://www.linkedin.com/jobs/view/1000000002", ApplicantCount: count(1),
			PostedAt: ago(1), ApplyType: "simple",
			Criteria: []Criterion{{Name: "stack", Verdict: "pass", Note: "Go"}, {Name: "seniority", Verdict: "pass"}},
		},
	}
}

func yourCallJobs() []Job {
	return []Job{{
		ID: 333, Title: "Platform Engineer", Company: "Fabrikam Cloud", Location: "Berlin, Germany",
		URL: "https://www.linkedin.com/jobs/view/1000000003", PostedAt: ago(3), ApplyType: "onsite",
		Criteria: []Criterion{{Name: "stack", Verdict: "pass", Note: "Go, Kubernetes"}, {Name: "location", Verdict: "unclear"}},
		Unclear:  []Point{{Name: "location", Snippet: "hybrid, 2 days in Berlin"}},
		Evidence: []string{"Our platform runs on Go and Kubernetes"},
	}}
}

func runnerUp() *RunnerUp {
	return &RunnerUp{
		Job:             Job{ID: 404, Title: "Go Developer", Company: "Tailspin Toys", URL: "https://www.linkedin.com/jobs/view/1000000004"},
		FailedCriterion: "seniority", Note: "associate",
	}
}

// cases are the golden inputs, one per brief shape.
func cases() map[string]Input {
	return map[string]Input{
		"normal_day": {
			Day: day, Apply: applyJobs(), YourCall: yourCallJobs(), RunnerUp: runnerUp(),
			Skipped: map[string]int{"language": 9, "location": 3, "rule": 2},
			Sources: healthyLinkedIn(23), Scoring: ScoringOK, Recheck: RecheckOK,
		},
		"empty_day": {
			Day: day, Sources: healthyLinkedIn(0), Scoring: ScoringOK, Recheck: RecheckNotRun,
		},
		"parser_broke": {
			Day: day, Sources: []Source{{Name: "LinkedIn", State: StateParserBroke}}, Scoring: ScoringOK, Recheck: RecheckNotRun,
		},
		"throttled": {
			Day: day, Sources: []Source{{Name: "LinkedIn", State: StateThrottled}}, Scoring: ScoringOK, RecheckOff: true,
		},
		"spend_cap": {
			Day: day, Sources: []Source{{Name: "LinkedIn", State: StateOK, New: 7, Canary: CanaryFailed}},
			Scoring: ScoringSpendCap, Recheck: RecheckNotRun, Skipped: map[string]int{"unscored": 7}, Unscored: 9,
		},
		"your_call_only": {
			Day: day, YourCall: yourCallJobs(), Sources: []Source{{Name: "LinkedIn", State: StateOK, New: 4}},
			Scoring: ScoringOK, Recheck: RecheckOK, Skipped: map[string]int{"criterion": 2, "closed": 1},
		},
		// Every source ran clean, but the recheck failed and left two jobs unreachable.
		"recheck_failed": {
			Day: day, Sources: healthyLinkedIn(3), Scoring: ScoringOK, Recheck: RecheckFailed,
			Skipped: map[string]int{"unreachable": 2, "closed": 1}, Unreachable: 2,
		},
		// LinkedIn and one board ran clean, but two jobs left out as not seen on their board,
		// one board's parser broke and one ATS that did not run still has a job left out.
		"ats_boards": {
			Day: day, Apply: applyJobs()[:1], Scoring: ScoringOK, Recheck: RecheckOK,
			Sources: []Source{
				{Name: "LinkedIn", State: StateOK, New: 4, Canary: CanaryOK},
				{Name: "Greenhouse", State: StateOK, New: 3, NoCanary: true, NotSeen: 2},
				{Name: "Lever", State: StateParserBroke, NoCanary: true},
				{Name: "Ashby", State: StateNotRun, NoCanary: true, NotSeen: 1},
			},
		},
		// The source ran clean, but the AI provider stayed busy and jobs wait unscored.
		"scoring_throttled": {
			Day: day, Sources: healthyLinkedIn(5), Scoring: ScoringThrottled, Recheck: RecheckNotRun,
			Skipped: map[string]int{"unscored": 5}, Unscored: 5,
		},
	}
}

func render(in Input) string {
	subject, text, html := Compose(in)
	return "== subject\n" + subject + "\n== text\n" + text + "== html\n" + html
}

func TestGolden(t *testing.T) {
	for name, in := range cases() {
		t.Run(name, func(t *testing.T) {
			got := render(in)
			path := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path) // #nosec G304 -- a fixed testdata path
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("%s differs from the golden file:\n%s", name, got)
			}
		})
	}
}

// fictionalRecruiter is the fixtures' recruiter card text (see the scoring sweep); none of
// it may reach a brief.
var fictionalRecruiter = []string{"Ottoline", "Brackwater", "Talent Partner", "hiring team", "Meet the"}

var (
	profileURL = regexp.MustCompile(`/in/`)
	emailAddr  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// forbidden returns every forbidden string found in s.
func forbidden(s string) []string {
	var hits []string
	for _, w := range fictionalRecruiter {
		if strings.Contains(s, w) {
			hits = append(hits, w)
		}
	}
	hits = append(hits, profileURL.FindAllString(s, -1)...)
	hits = append(hits, emailAddr.FindAllString(s, -1)...)
	return hits
}

// TestInvariantSweep: no golden brief carries recruiter text, a profile URL or an email
// address, and no failed-source brief says there are no jobs.
func TestInvariantSweep(t *testing.T) {
	for name, in := range cases() {
		out := render(in)
		if hits := forbidden(out); len(hits) > 0 {
			t.Errorf("%s carries forbidden strings %q", name, hits)
		}
		if strings.Contains(strings.ToLower(out), "no jobs today") {
			t.Errorf("%s says no jobs today", name)
		}
	}
}

// TestSweepCatchesPlanted proves the sweep fails when a forbidden string is planted in an
// input field, so a clean sweep means something.
func TestSweepCatchesPlanted(t *testing.T) {
	plants := map[string]func(*Input){
		"recruiter": func(in *Input) { in.Apply[0].Evidence = []string{"Ask " + "Ottoline" + " about it"} },
		"profile":   func(in *Input) { in.Apply[0].URL = "https://www.linkedin.com" + "/in/" + "someone" },
		"email":     func(in *Input) { in.YourCall[0].Unclear[0].Snippet = "write to jobs" + "@" + "contoso.example" },
	}
	for name, plant := range plants {
		in := cases()["normal_day"]
		plant(&in)
		if len(forbidden(render(in))) == 0 {
			t.Errorf("planted %s was not caught", name)
		}
	}
}

func TestHTMLEscaping(t *testing.T) {
	in := cases()["normal_day"]
	in.Apply[0].Company = "A&B <x>"
	in.Apply[0].Evidence = []string{`<script>alert("x")</script>`}
	_, text, html := Compose(in)
	if strings.Contains(html, "<x>") || strings.Contains(html, "<script>") {
		t.Errorf("html carries unescaped input:\n%s", html)
	}
	if !strings.Contains(html, "A&amp;B &lt;x&gt;") {
		t.Errorf("html lacks the escaped company:\n%s", html)
	}
	if !strings.Contains(text, "A&B <x>") {
		t.Errorf("text should carry the company as is")
	}
	// The referral search is built from the company name and escaped as a query value.
	want := "https://www.linkedin.com/search/results/people/?keywords=A%26B%20%3Cx%3E%20recruiter"
	if !strings.Contains(text, want) {
		t.Errorf("text lacks referral URL %s", want)
	}
}

func TestUnsafeLinkNotLinked(t *testing.T) {
	in := cases()["normal_day"]
	in.Apply[0].URL = "javascript:alert(1)"
	_, _, html := Compose(in)
	if strings.Contains(html, `href="javascript:`) {
		t.Errorf("html links a non-http URL:\n%s", html)
	}
}

func TestJobIDsByPosition(t *testing.T) {
	in := cases()["normal_day"]
	got := in.JobIDs()
	want := []int64{101, 205, 333}
	if len(got) != len(want) {
		t.Fatalf("JobIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("JobIDs = %v, want %v", got, want)
		}
	}
	// Numbering continues across sections, so position 3 is the first Your-call job.
	_, text, _ := Compose(in)
	if !strings.Contains(text, "\n3. Platform Engineer · Fabrikam Cloud") {
		t.Errorf("Your-call numbering does not continue:\n%s", text)
	}
}

func TestFailedSourceNeverSaysNothingNew(t *testing.T) {
	for _, name := range []string{"parser_broke", "throttled", "spend_cap", "recheck_failed", "scoring_throttled"} {
		_, text, _ := Compose(cases()[name])
		if strings.Contains(text, "Nothing new worth applying to today") {
			t.Errorf("%s claims nothing new while a source or scoring failed:\n%s", name, text)
		}
	}
	in := Input{Day: day, Sources: []Source{{Name: "LinkedIn", State: StateNotRun}}, Scoring: ScoringOK}
	_, text, _ := Compose(in)
	if !strings.Contains(text, "Health: LinkedIn did not run") || strings.Contains(text, "Nothing new") {
		t.Errorf("not-run brief:\n%s", text)
	}
	// Sources clean, but scoring or the recheck did not finish, or a job waits on either.
	clean := func(f func(*Input)) Input {
		in := Input{Day: day, Sources: healthyLinkedIn(0), Scoring: ScoringOK, Recheck: RecheckOK}
		f(&in)
		return in
	}
	for name, in := range map[string]Input{
		"scoring not run":    clean(func(in *Input) { in.Scoring = "" }),
		"scoring failed":     clean(func(in *Input) { in.Scoring = ScoringFailed }),
		"scoring throttled":  clean(func(in *Input) { in.Scoring = ScoringThrottled }),
		"scoring budget":     clean(func(in *Input) { in.Scoring = ScoringBudget }),
		"recheck failed":     clean(func(in *Input) { in.Recheck = RecheckFailed }),
		"recheck stopped":    clean(func(in *Input) { in.Recheck = StateParserBroke }),
		"recheck budget":     clean(func(in *Input) { in.Recheck = "budget" }),
		"unreachable":        clean(func(in *Input) { in.Unreachable = 1 }),
		"unscored":           clean(func(in *Input) { in.Unscored = 1 }),
		"unreachable no run": clean(func(in *Input) { in.RecheckOff, in.Unreachable = true, 1 }),
		"not seen on board": clean(func(in *Input) {
			in.Sources = append(in.Sources, Source{Name: "Lever", State: StateOK, NoCanary: true, NotSeen: 1})
		}),
	} {
		if subject, text, _ := Compose(in); strings.Contains(text, "Nothing new") || strings.Contains(subject, "nothing new") {
			t.Errorf("%s claims nothing new:\n%s", name, text)
		}
	}
	for name, in := range map[string]Input{
		"recheck ok":      clean(func(*Input) {}),
		"recheck not run": clean(func(in *Input) { in.Recheck = RecheckNotRun }),
		"recheck off":     clean(func(in *Input) { in.RecheckOff = true }),
		"ats clean": clean(func(in *Input) {
			in.Sources = append(in.Sources, Source{Name: "Lever", State: StateOK, New: 2, NoCanary: true})
		}),
	} {
		if _, text, _ := Compose(in); !strings.Contains(text, "Nothing new worth applying to today") {
			t.Errorf("%s: a clean day does not say nothing new:\n%s", name, text)
		}
	}
}

func TestHealthLines(t *testing.T) {
	for _, c := range []struct {
		in   Input
		want []string
	}{
		{Input{Scoring: ScoringOK, Recheck: RecheckOK}, []string{"Health: Scoring ok", "Health: recheck ok"}},
		{Input{Recheck: StateParserBroke, Unreachable: 3, Unscored: 2},
			[]string{"Health: Scoring did not run today · 2 not scored yet", "Health: recheck stopped (parser broke) · 3 unreachable, retried next brief"}},
		{Input{Scoring: ScoringFailed, RecheckOff: true}, []string{"Health: Scoring failed: jobs it did not reach are scored on the next run", "Health: recheck off (not live)"}},
		{Input{Scoring: ScoringThrottled, Recheck: RecheckFailed}, []string{"Health: Scoring paused: the AI provider was busy (throttled)", "Health: recheck failed"}},
		{Input{Scoring: ScoringSpendCap}, []string{"Health: Scoring paused: daily Jev cap reached", "Health: recheck not run"}},
		{Input{Scoring: ScoringBudget, Recheck: RecheckOK, Unscored: 40},
			[]string{"Health: Scoring paused: time budget reached; jobs it did not reach are scored on the next run · 40 not scored yet", "Health: recheck ok"}},
	} {
		got := healthLines(c.in)[1:] // after "no source ran today"
		if !slices.Equal(got, c.want) {
			t.Errorf("%+v: %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSubject(t *testing.T) {
	tests := []struct {
		name string
		in   Input
		want string
	}{
		{"normal", cases()["normal_day"], "LeadHound · Fri 23 Oct · 2 to apply, 1 needs your call"},
		{"empty", cases()["empty_day"], "LeadHound · Fri 23 Oct · nothing new"},
		{"broken", cases()["parser_broke"], "LeadHound · Fri 23 Oct · nothing listed, see health"},
		{"plural", Input{Day: day, YourCall: append(yourCallJobs(), yourCallJobs()...), Sources: healthyLinkedIn(2)}, "LeadHound · Fri 23 Oct · 2 need your call"},
	}
	for _, tt := range tests {
		if got, _, _ := Compose(tt.in); got != tt.want {
			t.Errorf("%s: subject %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestATSSourceLines: an ATS line names no canary, and the jobs left out as not seen on
// their board are counted on it whatever its state.
func TestATSSourceLines(t *testing.T) {
	for _, c := range []struct {
		src  Source
		want string
	}{
		{Source{Name: "Lever", State: StateOK, New: 2, NoCanary: true}, "Health: Lever ok · 2 new"},
		{Source{Name: "Lever", State: StateOK, New: 0, NoCanary: true, NotSeen: 1}, "Health: Lever ok · 0 new · 1 job left out, not seen on its board for 2 days"},
		{Source{Name: "Ashby", State: StateNotRun, NoCanary: true, NotSeen: 3}, "Health: Ashby did not run · 3 jobs left out, not seen on their board for 2 days"},
		{Source{Name: "Ashby", State: "failure_rate", NoCanary: true}, "Health: Ashby did not finish (failure rate)"},
		{Source{Name: "Ashby", State: "budget", NoCanary: true}, "Health: Ashby did not finish (budget)"},
		{Source{Name: "Ashby", State: StateBlocked, NoCanary: true}, "Health: Ashby paused today (blocked)"},
		{Source{Name: "LinkedIn", State: StateOK, New: 1}, "Health: LinkedIn ok · 1 new · canary not run"},
	} {
		if got := healthLines(Input{Sources: []Source{c.src}})[0]; got != c.want {
			t.Errorf("%+v: %q, want %q", c.src, got, c.want)
		}
	}
}

// cliFooter is the footer when no console URL is set.
const cliFooter = "Mark a job with leadhound track N applied, leadhound track N skip --reason TEXT or leadhound track N maybe, N being its number."

// TestFooter: with a console URL the footer links its jobs page, without one it names
// the track commands, and a day with nothing listed has no footer.
func TestFooter(t *testing.T) {
	in := cases()["normal_day"]
	_, text, html := Compose(in)
	if !strings.HasSuffix(text, "\n"+cliFooter+"\n") {
		t.Errorf("text does not end with the track line:\n%s", text)
	}
	if !strings.Contains(html, "\n<p>"+cliFooter+"</p>\n</body>") {
		t.Errorf("html does not end with the track line:\n%s", html)
	}

	for _, u := range []string{"https://console.example", "https://console.example/"} {
		in.ConsoleURL = u
		_, text, html = Compose(in)
		if !strings.HasSuffix(text, "\nMark jobs in the console: https://console.example/jobs\n") {
			t.Errorf("%s: text does not end with the console line:\n%s", u, text)
		}
		want := `<p>Mark jobs in the console: <a href="https://console.example/jobs" rel="noreferrer">https://console.example/jobs</a></p>`
		if !strings.Contains(html, "\n"+want+"\n</body>") {
			t.Errorf("%s: html lacks %s:\n%s", u, want, html)
		}
		if strings.Contains(text, "leadhound track") || strings.Contains(html, "leadhound track") {
			t.Errorf("%s: the console footer also names the track commands", u)
		}
	}

	// The URL is escaped in the attribute and the text.
	in.ConsoleURL = `https://console.example/a"b<c>`
	_, _, html = Compose(in)
	if strings.Contains(html, `a"b`) || strings.Contains(html, "<c>") {
		t.Errorf("html carries the URL unescaped:\n%s", html)
	}
	if !strings.Contains(html, `href="https://console.example/a&#34;b&lt;c&gt;/jobs"`) {
		t.Errorf("html lacks the escaped href:\n%s", html)
	}

	// A URL that is not http or https is written as text, never linked.
	in.ConsoleURL = "javascript:alert(1)"
	_, _, html = Compose(in)
	if strings.Contains(html, `href="javascript:`) {
		t.Errorf("html links a non-http console URL:\n%s", html)
	}

	for _, name := range []string{"empty_day", "parser_broke", "spend_cap"} {
		for _, u := range []string{"", "https://console.example"} {
			in := cases()[name]
			in.ConsoleURL = u
			_, text, html := Compose(in)
			if strings.Contains(text, "Mark ") || strings.Contains(html, "Mark ") {
				t.Errorf("%s (console %q) has a footer with nothing listed", name, u)
			}
		}
	}
}

// TestHealthLinesParity: HealthLines is exactly the "Health:" lines Compose writes, for
// every golden input.
func TestHealthLinesParity(t *testing.T) {
	for name, in := range cases() {
		_, text, _ := Compose(in)
		var got []string
		for l := range strings.SplitSeq(text, "\n") {
			if strings.HasPrefix(l, "Health: ") {
				got = append(got, l)
			}
		}
		if want := HealthLines(in); !slices.Equal(got, want) {
			t.Errorf("%s: Compose wrote %q, HealthLines is %q", name, got, want)
		}
	}
}

// TestNoAssistantName: no golden and no source file in this package names the assistant
// the brief used to point at. The word is built here so this file does not carry it.
func TestNoAssistantName(t *testing.T) {
	word := "Gli" + "tch"
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	goldens, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || len(goldens) == 0 {
		t.Fatalf("sweep found %d source files and %d goldens", len(files), len(goldens))
	}
	for _, f := range append(files, goldens...) {
		b, err := os.ReadFile(f) // #nosec G304 -- a path globbed inside this package
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(word)) {
			t.Errorf("%s names %s", f, word)
		}
	}
}
