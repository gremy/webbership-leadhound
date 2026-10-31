package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// richCard is a Run now card with every action shown: preview done, fetch running.
// It has no ResultsAt, so the dashboard shows one "as of" only, in its Now card.
func richCard(text string) RunNowCard {
	since := testSent
	return RunNowCard{
		Actions: []RunNowAction{
			{Action: RunPreview, State: "done", Line: "Last done at 04:50 " + text},
			{Action: RunFetch, State: "running", Line: "Running since 04:50: collect " + text, Since: &since, Pending: true},
			{Action: RunBrief, State: "idle", Line: ""},
			{Action: RunATS, State: "idle", Line: ""},
		},
	}
}

// idleCard is a card with nothing pending and today's brief unsent.
func idleCard() RunNowCard {
	return RunNowCard{Actions: []RunNowAction{
		{Action: RunPreview, State: "idle"}, {Action: RunFetch, State: "idle"},
		{Action: RunBrief, State: "idle"}, {Action: RunATS, State: "idle"},
	}}
}

// richResults is a preview with a job in every list.
func richResults(text string) Results {
	n := 37
	posted := testSent.Add(-48 * time.Hour)
	job := func(id int64, url string) ResultJob {
		return ResultJob{JobID: id, Title: text, Company: text, Location: text, URL: url, ApplyType: "easy apply",
			Status: "new", ListingStatus: "open", ApplicantCount: &n, PostedAt: &posted,
			Criteria: []JobCriterion{{Name: "remote", Verdict: "pass"}, {Name: "salary", Verdict: "unclear"}}}
	}
	ru := job(44, "https://jobs.example/44")
	return Results{ //nolint:gosec // G101: job fields and health lines, not credentials
		MadeAt: testSent, Day: testDay, RecheckOff: true,
		Health:   []string{"Health: linkedin ok " + text},
		Apply:    []ResultJob{job(42, "https://jobs.example/42")},
		YourCall: []ResultJob{job(43, "https://jobs.example/43")},
		RunnerUp: &ru, RunnerUpWhy: "salary: below the floor",
		Unreachable: 2, Unscored: 1,
	}
}

func TestRunNowUnknownAction(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, p := range []string{"/run/nope", "/run/PREVIEW", "/run/preview2"} {
		if w := h.do(post(p, url.Values{"confirm": {"yes"}}, c)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, w.Code)
		}
		if w := h.do(hx(post(p, nil, c))); w.Code != http.StatusNotFound {
			t.Errorf("htmx %s: %d, want 404", p, w.Code)
		}
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("the backend was called %d times for an unknown action", n)
	}
}

func TestRunNowPostsTheRequest(t *testing.T) {
	tests := []struct {
		action string
		form   url.Values
		want   RunRequest
	}{
		{RunPreview, nil, RunRequest{Action: RunPreview}},
		{RunFetch, nil, RunRequest{Action: RunFetch}},
		{RunBrief, nil, RunRequest{Action: RunBrief}},
		{RunATS, nil, RunRequest{Action: RunATS}},
		{RunATS, url.Values{"confirm": {"yes"}}, RunRequest{Action: RunATS, Confirmed: true}},
		{RunATS, url.Values{"confirm": {"on"}}, RunRequest{Action: RunATS}},
		{RunPreview, url.Values{"confirm": {"yes"}}, RunRequest{Action: RunPreview, Confirmed: true}},
	}
	for _, tt := range tests {
		t.Run(tt.action+" "+tt.form.Encode(), func(t *testing.T) {
			b := richBackend("x")
			b.runOut = RunOutcome{Message: "Asked. Results usually in a minute or two."}
			h := newHarness(t, b, false)
			c := h.session(t)
			w := h.do(post("/run/"+tt.action, tt.form, c))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/jobs/home" {
				t.Fatalf("got %d to %q, want 303 to /jobs/home", w.Code, w.Header().Get("Location"))
			}
			if b.lastRun != tt.want {
				t.Errorf("request %+v, want %+v", b.lastRun, tt.want)
			}
			if n := b.n("RunNow"); n != 1 {
				t.Errorf("RunNow called %d times", n)
			}
			flash := cookieNamed(w, "__Host-lh_flash")
			if flash == nil || flash.Value == "" {
				t.Fatal("no flash cookie")
			}
			body := h.do(get("/jobs/home", c, flash)).Body.String()
			if !strings.Contains(body, "Asked. Results usually in a minute or two.") {
				t.Error("Jobs home does not show the outcome")
			}
			logs := h.logs.String()
			if !strings.Contains(logs, "console run now") || !strings.Contains(logs, "action="+tt.action) || !strings.Contains(logs, "outcome=ok") {
				t.Errorf("log %q lacks the run now line", logs)
			}
		})
	}
}

func TestRunNowRefused(t *testing.T) {
	const words = "Tick the box to confirm: this run takes hours"
	b := richBackend("x")
	b.runErr = fmt.Errorf("%w: %s", ErrUserFixable, words)
	h := newHarness(t, b, false)
	c := h.session(t)

	w := h.do(post("/run/ats", nil, c))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("plain refusal: %d, want 422", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, words) || !strings.Contains(body, "notice-error") || !strings.Contains(body, "<h2>Run now</h2>") {
		t.Errorf("the 422 dashboard lacks the refusal: %s", body)
	}
	if b.lastRun.Confirmed {
		t.Error("an unticked box reached the backend as confirmed")
	}

	w = h.do(hx(post("/run/ats", nil, c)))
	if w.Code != http.StatusOK {
		t.Fatalf("htmx refusal: %d, want 200", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, words) || !strings.Contains(body, `id="run-now"`) || strings.Contains(body, "<html") {
		t.Errorf("the htmx refusal is not the card with the words: %s", body)
	}
	if !strings.Contains(h.logs.String(), "outcome=refused") {
		t.Error("the refusal is not logged")
	}

	b.runErr = errors.New("disk full")
	if w := h.do(post("/run/preview", nil, c)); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "disk full") {
		t.Errorf("a backend failure: %d %q", w.Code, w.Body.String())
	}
}

func TestRunNowHTMXAnswersTheCard(t *testing.T) {
	for _, action := range RunActions {
		t.Run(action, func(t *testing.T) {
			b := richBackend("x")
			b.runOut = RunOutcome{Message: "Asked."}
			h := newHarness(t, b, false)
			w := h.do(hx(post("/run/"+action, url.Values{"confirm": {"yes"}}, h.session(t))))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			body := strings.TrimSpace(w.Body.String())
			if !strings.HasPrefix(body, `<section class="card run-now" id="run-now"`) || !strings.HasSuffix(body, "</section>") {
				t.Errorf("not the card alone: %s", body)
			}
			if !strings.Contains(body, "Asked.") {
				t.Error("the card lacks the outcome")
			}
			if cookieNamed(w, "__Host-lh_flash") != nil {
				t.Error("an htmx answer set the flash cookie")
			}
			if b.n("RunNowCard") != 1 {
				t.Error("the card was not read fresh after the press")
			}
		})
	}
}

func TestRunNowCrossOrigin(t *testing.T) {
	for _, action := range RunActions {
		h := newHarness(t, richBackend("x"), false)
		r := post("/run/"+action, url.Values{"confirm": {"yes"}}, h.session(t))
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		if w := h.do(r); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", action, w.Code)
		}
		if n := h.b.total(); n != 0 {
			t.Errorf("%s: backend called %d times", action, n)
		}
	}
}

func TestRunCardPollsOnlyWhilePending(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	poll := `hx-get="/run/status" hx-trigger="every 10s" hx-swap="outerHTML"`

	if body := h.do(get("/jobs/home", c)).Body.String(); !strings.Contains(body, poll) {
		t.Error("the dashboard card does not poll while fetch runs")
	}
	if body := h.do(hx(get("/run/status", c))).Body.String(); !strings.Contains(body, poll) {
		t.Error("the fragment does not poll while fetch runs")
	}

	b.dash.RunNow = idleCard()
	b.runCard = idleCard()
	if body := h.do(get("/jobs/home", c)).Body.String(); strings.Contains(body, "/run/status") {
		t.Error("the dashboard card polls with nothing pending")
	}
	if body := h.do(hx(get("/run/status", c))).Body.String(); strings.Contains(body, "/run/status") {
		t.Error("the fragment polls with nothing pending")
	}
}

func TestRunCardRows(t *testing.T) {
	b := richBackend("x")
	b.dash.RunNow = idleCard()
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/jobs/home", c)).Body.String()
	for _, want := range []string{
		`action="/run/preview" hx-post="/run/preview" hx-target="#run-now" hx-swap="outerHTML"`, "Show results now",
		`action="/run/fetch"`, "Fetch new jobs, then show",
		`action="/run/brief"`, "Send today's email now", "the 07:50 timer then skips today",
		">Run the nightly ATS collect now…</summary>", "about two hours",
		`<input class="check" type="checkbox" id="ats-confirm" name="confirm" value="yes" required>`, "I understand, run it now",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	if i, j := strings.Index(body, `id="run-now"`), strings.Index(body, "This morning's email"); i < 0 || i > j {
		t.Error("the Run now card is not the first card")
	}
	if strings.Contains(body, "See the results") {
		t.Error("a results link with no results")
	}

	sent := testSent
	b.dash.RunNow.BriefSentAt = &sent
	body = h.do(get("/jobs/home", c)).Body.String()
	if strings.Contains(body, `action="/run/brief"`) || strings.Contains(body, "Send today's email now") {
		t.Error("the send button is shown once today's email went")
	}
	if !strings.Contains(body, "Today's email went at 04:50.") {
		t.Error("the card does not say when today's email went")
	}

	made := testSent
	b.dash.RunNow.ResultsAt = &made
	b.dash.RunNow.Actions[0] = RunNowAction{Action: RunPreview, State: "done", Line: "Last done at 04:50"}
	b.dash.RunNow.Actions[3] = RunNowAction{Action: RunATS, State: "stuck", Line: "Asked at 04:50 and not started. See deploy/README.md, Run now"}
	body = h.do(get("/jobs/home", c)).Body.String()
	if !strings.Contains(body, `<a href="/results">See the results (as of 04:50)</a>`) {
		t.Error("a done preview does not link its results")
	}
	if !strings.Contains(body, `<p class="run-line bad">Asked at 04:50 and not started. See deploy/README.md, Run now</p>`) {
		t.Error("the stuck line is not shown as a problem")
	}

	b.dash.RunNow = RunNowCard{}
	if body = h.do(get("/jobs/home", c)).Body.String(); !strings.Contains(body, "Run now is not available") {
		t.Error("an empty card says nothing")
	}
}

func TestRunStatus(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	w := h.do(hx(get("/run/status", c)))
	body := strings.TrimSpace(w.Body.String())
	if w.Code != http.StatusOK || !strings.HasPrefix(body, `<section class="card run-now"`) || strings.Contains(body, "<html") {
		t.Errorf("htmx status: %d %s", w.Code, body)
	}
	for _, bad := range []string{"<style", " style=", "hx-on", "onclick", "<script"} {
		if strings.Contains(body, bad) {
			t.Errorf("the fragment contains %q", bad)
		}
	}
	w = h.do(get("/run/status", c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/jobs/home" {
		t.Errorf("a plain visit: %d to %q, want 303 /jobs/home", w.Code, w.Header().Get("Location"))
	}
}

func TestResultsEmpty(t *testing.T) {
	b := richBackend("x")
	b.resultsErr = ErrNotFound
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(get("/results", c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No results yet. Press Show results now on the dashboard.") {
		t.Errorf("empty state: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "as of") {
		t.Error("the empty state shows a time")
	}
	b.resultsErr = errors.New("bad file")
	if w := h.do(get("/results", c)); w.Code != http.StatusInternalServerError {
		t.Errorf("a read failure: %d, want 500", w.Code)
	}
}

func TestResultsPage(t *testing.T) {
	b := richBackend("Go Engineer")
	b.results.YourCall = append(b.results.YourCall, ResultJob{JobID: 45, Title: "y", Status: "shortlisted", URL: "javascript:alert(1)"})
	h := newHarness(t, b, false)
	w := h.do(get("/results", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h1>Results</h1>", "as of Sat 24 Oct 04:50", "Not rechecked against LinkedIn (live is off)",
		"linkedin ok Go Engineer", "<h2>Apply</h2>", "<h2>Your call</h2>", "<h2>Runner-up</h2>",
		"Missed on: salary: below the floor", "2 jobs unreachable · 1 job not scored yet.",
		"37 applicants", "easy apply", `<span class="verdict verdict-pass">pass</span>`,
		`<span class="status status-shortlisted">shortlisted</span>`,
		`aria-current="page">Results</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the results page lacks %q", want)
		}
	}
	if strings.Contains(body, "Health: ") {
		t.Error("the Health: prefix is shown")
	}
	for _, id := range []string{"42", "43", "44", "45"} {
		if !strings.Contains(body, `<a href="/jobs/`+id+`">`) {
			t.Errorf("job %s does not link to its page", id)
		}
		if !strings.Contains(body, `<input type="hidden" name="ref" value="job:`+id+`">`) {
			t.Errorf("job %s has no track actions", id)
		}
	}
	for _, id := range []string{"42", "43", "44"} {
		if !strings.Contains(body, `<a href="https://jobs.example/`+id+`" rel="noreferrer noopener" target="_blank">Open posting</a>`) {
			t.Errorf("job %s's posting link lacks rel and target", id)
		}
	}
	if strings.Contains(body, "javascript:") {
		t.Error("a javascript: URL was rendered as a link")
	}
	if !strings.Contains(body, `<input type="hidden" name="back" value="results">`) || !strings.Contains(body, `action="/jobs/track"`) {
		t.Error("the track forms do not come back to the results")
	}
}

func TestResultsTrack(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	form := url.Values{"ref": {"job:42"}, "back": {"results"}, "action": {"applied"}}
	w := h.do(post("/jobs/track", form, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/results#result-42" {
		t.Fatalf("got %d to %q, want 303 /results#result-42", w.Code, w.Header().Get("Location"))
	}
	if b.last.Ref != "job:42" || b.last.Action != "applied" {
		t.Errorf("request %+v", b.last)
	}
	flash := cookieNamed(w, "__Host-lh_flash")
	if body := h.do(get("/results", c, flash)).Body.String(); !strings.Contains(body, "Go Engineer: shortlisted → applied") {
		t.Error("the results page does not show the outcome")
	}

	b.err = fmt.Errorf("%w: already applied; use withdrawn", ErrUserFixable)
	w = h.do(post("/jobs/track", form, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "already applied; use withdrawn") ||
		!strings.Contains(w.Body.String(), "<h1>Results</h1>") {
		t.Errorf("a refusal: %d, want 422 on the results page", w.Code)
	}

	// back=results without a job ref is the Jobs page's own answer.
	b.err = nil
	w = h.do(post("/jobs/track", url.Values{"ref": {"1"}, "day": {"2026-10-24"}, "back": {"results"}, "action": {"applied"}}, c))
	if w.Header().Get("Location") != "/jobs#job-1" {
		t.Errorf("a numbered ref went to %q", w.Header().Get("Location"))
	}
}
