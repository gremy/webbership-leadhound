//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// saveRateSettings saves doc as the console would, as the latest leads settings.
func saveRateSettings(t *testing.T, doc string) {
	t.Helper()
	if _, err := leadstoretest.Q(t).SaveSettings(context.Background(), leadstore.DefaultAccountID, []byte(doc), leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
}

// ratedDomains is the domains of every lead_relevance row, in the order they were written.
func ratedDomains(t *testing.T) []string {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT o.domain FROM lead_relevance r JOIN lead l ON l.id = r.lead_id JOIN org o ON o.id = l.org_id ORDER BY r.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLeadsRateSkipsUnchanged: the fake decider rates every open lead once; a second run
// asks nothing; a change to the end-of-life date of Drupal 7 saved in Settings changes the
// input of the three Drupal 7 leads only, so exactly those are asked again.
func TestLeadsRateSkipsUnchanged(t *testing.T) {
	orderScene(t)
	out := mustRun(t, "leads", "rate", "--decider", "fake")
	wantLines(t, "first rate", out, "· decider fake · rated 8 · unchanged 0 · stop none", "still due: 0 · calls: 8 · failed calls: 0")
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance r JOIN run ON run.id = r.run_id
		WHERE r.question = 'relevance@1' AND r.decider = 'fake' AND r.model = 'fake' AND r.band = 'maybe' AND r.score = 50
		  AND length(r.input_sha256) = 32 AND run.command = 'leads-rate' AND run.source = 'fake' AND run.finished_at IS NOT NULL`); n != 8 {
		t.Errorf("%d fake ratings as written, want 8", n)
	}
	if n := dbCount(t, `SELECT coalesce(sum(calls), 0) FROM ai_usage WHERE provider = 'fake' AND purpose = 'leads'`); n != 8 {
		t.Errorf("leads usage = %d calls, want 8", n)
	}

	out = mustRun(t, "leads", "rate", "--decider", "fake")
	wantLines(t, "second rate", out, "· rated 0 · unchanged 8 · stop none")

	saveRateSettings(t, `{"facts":{"d7_eol":"2025-02-01"}}`)
	out = mustRun(t, "leads", "rate", "--decider", "fake")
	wantLines(t, "rate after a date change", out, "· rated 3 · unchanged 5 · stop none")
	rated := ratedDomains(t)
	slices.Sort(rated[8:])
	if got, want := rated[8:], []string{"lucerne.example", "margie.example", "woodgrove.example"}; !slices.Equal(got, want) {
		t.Errorf("asked again: %q, want the Drupal 7 leads %q", got, want)
	}
}

// TestLeadsRateStopsAtCap: a leads cap of 2 saved in Settings stops the rating after two
// calls with stop spend_cap; the next run the same day asks nothing more.
func TestLeadsRateStopsAtCap(t *testing.T) {
	orderScene(t)
	saveRateSettings(t, `{"relevance":{"leads_daily_calls":2}}`)
	out := mustRun(t, "leads", "rate", "--decider", "fake")
	wantLines(t, "rate at the cap", out, "· rated 2 · unchanged 0 · stop spend_cap", "still due: 6 · calls: 2 ·", "leads cap: 2 calls a day")
	out = mustRun(t, "leads", "rate", "--decider", "fake")
	wantLines(t, "rate past the cap", out, "· rated 0 · unchanged 2 · stop spend_cap", "still due: 6 · calls: 0 ·")
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance`); n != 2 {
		t.Errorf("%d ratings, want 2", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM run WHERE command = 'leads-rate' AND stop_reason = 'spend_cap' AND requests = 2`); n != 1 {
		t.Errorf("%d runs recorded stopping at the cap after 2 calls, want 1", n)
	}
	// --max stops a run below the cap.
	saveRateSettings(t, `{"relevance":{"leads_daily_calls":0}}`)
	out = mustRun(t, "leads", "rate", "--decider", "fake", "--max", "1")
	wantLines(t, "rate with --max", out, "· rated 1 · unchanged 2 · stop budget", "still due: 5 ·")
}

// TestLeadsRateRatesStarredFirst: starred leads are asked first in the operator's order,
// then the rest in the queue's order (points, then id), whatever their ids.
func TestLeadsRateRatesStarredFirst(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	for _, d := range []string{"cpandl.example", "margie.example"} {
		if _, err := lq.Star(ctx, leadIDOf(t, d), leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	out := mustRun(t, "leads", "rate", "--decider", "fake", "--max", "3")
	wantLines(t, "rate --max 3", out, "· rated 3 · unchanged 0 · stop budget")
	if got, want := ratedDomains(t), []string{"cpandl.example", "margie.example", "lucerne.example"}; !slices.Equal(got, want) {
		t.Errorf("first three asked: %q, want %q", got, want)
	}
	mustRun(t, "leads", "rate", "--decider", "fake")
	want := []string{"cpandl.example", "margie.example", "lucerne.example", "woodgrove.example",
		"fourthcoffee.example", "humongous.example", "bellows.example", "proseware.example"}
	if got := ratedDomains(t); !slices.Equal(got, want) {
		t.Errorf("asked in order %q, want %q", got, want)
	}
}

// TestLeadsRateOffRefuses: with rating switched off in Settings, rate refuses in plain words
// (a usage exit, so the unit goes on to the build), asks nothing and records no run.
func TestLeadsRateOffRefuses(t *testing.T) {
	orderScene(t)
	saveRateSettings(t, `{"relevance":{"rating":false}}`)
	code, out, errOut := leadsRun(t, "leads", "rate", "--decider", "fake")
	if code != 2 || !strings.Contains(errOut, "Relevance rating is off: turn it on in Leads → Settings.") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if n := dbCount(t, `SELECT (SELECT count(*) FROM lead_relevance) + (SELECT count(*) FROM run WHERE command = 'leads-rate') + (SELECT count(*) FROM ai_usage)`); n != 0 {
		t.Errorf("%d rows written while rating is off", n)
	}
}

// TestLeadsRateLiveOff: the default decider is jev, which asks nothing without
// LEADHOUND_LIVE=1: the run exits 0 having needed no key and written nothing.
func TestLeadsRateLiveOff(t *testing.T) {
	orderScene(t)
	t.Setenv("TYPESAFE_API_KEY", "")
	code, out, errOut := leadsRun(t, "leads", "rate")
	if code != 0 || !strings.Contains(out, "leads rate: live fetching is off here (LEADHOUND_LIVE is off); nothing requested") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	if n := dbCount(t, `SELECT (SELECT count(*) FROM lead_relevance) + (SELECT count(*) FROM run WHERE command = 'leads-rate')`); n != 0 {
		t.Errorf("%d rows written with live calls off", n)
	}
}

// TestJobsAndLeadsCountSeparately (D9): leads use their whole cap while the jobs cap is
// spent, and jobs still get their whole cap after leads used theirs.
func TestJobsAndLeadsCountSeparately(t *testing.T) {
	orderScene(t)
	ctx := context.Background()
	const jobsCap = 3
	owner := storetest.OwnerConn(t)
	// The jobs cap spent yesterday, today and tomorrow, so the test does not depend on the
	// zone's date.
	if _, err := owner.Exec(ctx, `INSERT INTO ai_usage (provider, model, day, calls, input_tokens, purpose)
		SELECT 'fake', 'fake', current_date + d, $1, 0, 'jobs' FROM generate_series(-1, 1) AS d`, jobsCap); err != nil {
		t.Fatal(err)
	}
	saveRateSettings(t, `{"relevance":{"leads_daily_calls":2}}`)
	out := mustRun(t, "leads", "rate", "--decider", "fake")
	wantLines(t, "rate with the jobs cap spent", out, "· rated 2 · unchanged 0 · stop spend_cap")

	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	q := store.NewQueries(storetest.AppConn(t))
	if got, err := remainingCalls(ctx, q, relevanceFake(), jobsCap, loc); err != nil || got != 0 {
		t.Fatalf("jobs calls left = %d, %v; want 0: the leads calls moved the jobs count", got, err)
	}
	if _, err := owner.Exec(ctx, `DELETE FROM ai_usage WHERE purpose = 'jobs'`); err != nil {
		t.Fatal(err)
	}
	if got, err := remainingCalls(ctx, q, relevanceFake(), jobsCap, loc); err != nil || got != jobsCap {
		t.Fatalf("jobs calls left = %d, %v; want the whole cap %d after the leads used theirs", got, err, jobsCap)
	}
	jobs := &ai.Capped{Decider: relevanceFake(), Store: q, DailyCalls: jobsCap, Loc: loc, Purpose: ai.PurposeJobs}
	for i := range jobsCap {
		if _, _, err := jobs.Decide(ctx, "state", []ai.Question{{Name: "relevance", Options: []ai.Option{{Name: "maybe"}}}}); err != nil {
			t.Fatalf("jobs call %d: %v", i+1, err)
		}
	}
	if _, _, err := jobs.Decide(ctx, "state", []ai.Question{{Name: "relevance", Options: []ai.Option{{Name: "maybe"}}}}); !errors.Is(err, ai.ErrSpendCap) {
		t.Fatalf("jobs call past the cap: %v, want ErrSpendCap", err)
	}
	if n := dbCount(t, `SELECT coalesce(sum(calls), 0) FROM ai_usage WHERE purpose = 'leads'`); n != 2 {
		t.Errorf("leads calls = %d, want still 2", n)
	}
}

// TestGoldenDraftAndEval: a golden draft writes the open leads as unlabelled golden lines at
// mode 0600, which the eval reads; it never overwrites the file. An eval with no label is a
// usage error; a labelled fake eval prints its report and records nothing.
func TestGoldenDraftAndEval(t *testing.T) {
	orderScene(t)
	path := filepath.Join(t.TempDir(), "golden", "leads-relevance.jsonl")
	out := mustRun(t, "leads", "relevance", "golden-draft", "--n", "5", "--out", path)
	wantLines(t, "golden-draft", out, "5 leads (of 8 open) written to "+path+", mode 0600 · 1 kinds · 3 majors · 1 countries")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	lines, err := leadrate.LoadGolden(path)
	if err != nil {
		t.Fatal(err)
	}
	majors := map[int]bool{}
	for _, g := range lines {
		if g.Label != nil {
			t.Errorf("%s has label %q, want null", g.ID, *g.Label)
		}
		if !strings.HasSuffix(g.ID, ".example") || g.Input.Kind != leadstore.LeadUnsupported || g.Input.EOL.IsZero() {
			t.Errorf("golden line %+v", g)
		}
		majors[g.Input.Major] = true
	}
	if len(lines) != 5 || len(majors) != 3 {
		t.Errorf("%d lines over %d majors, want 5 over all 3", len(lines), len(majors))
	}
	if code, _, errOut := leadsRun(t, "leads", "relevance", "golden-draft", "--n", "5", "--out", path); code != 2 || !strings.Contains(errOut, "never overwrites") {
		t.Errorf("second draft to the same file: exit %d, %q", code, errOut)
	}

	if code, _, errOut := leadsRun(t, "leads", "relevance", "eval", "--golden", path); code != 2 || !strings.Contains(errOut, "has no labelled line") {
		t.Errorf("eval of an unlabelled draft: exit %d, %q", code, errOut)
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), `"label":null`, `"label":"maybe"`)), 0o600); err != nil { // #nosec G703 -- the test's own temp file
		t.Fatal(err)
	}
	out = mustRun(t, "leads", "relevance", "eval", "--golden", path, "--runs", "1")
	wantLines(t, "fake eval", out, "relevance eval: 5 labelled lead(s), 0 unlabelled skipped · question relevance@1 · decider fake · 1 run(s)",
		"exact band: 100%", "not recorded: only an eval with --decider jev measures the number the list shows")
	if n := dbCount(t, `SELECT count(*) FROM lead_relevance_eval`); n != 0 {
		t.Errorf("%d evals recorded from a fake eval", n)
	}
}

// TestDashboardCountsJevPerPurpose: the dashboard counts today's Jev calls for jobs and for
// leads apart, the jobs calls against LEADHOUND_JEV_DAILY_CALL_CAP and the leads calls
// against the leads cap leads rate runs under (a saved setting may lower the server's cap),
// never every call against the jobs cap.
func TestDashboardCountsJevPerPurpose(t *testing.T) {
	ctx := context.Background()
	leadsDB(t)
	b := leadsBackend(t)
	today := b.now().In(b.loc)
	for _, r := range []ai.UsageRow{
		{Provider: "typesafe", Model: "jev", Day: today, Calls: 7, InputTokens: 70, Purpose: ai.PurposeJobs},
		{Provider: "typesafe", Model: "jev", Day: today, Calls: 40, InputTokens: 400, Purpose: ai.PurposeLeads},
	} {
		if err := b.s.Q().RecordAIUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	h, cookie := jobsHandler(t, b)
	check := func(what string, leadsCap int) {
		t.Helper()
		d, err := b.Dashboard(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d.JobsCalls != 7 || d.LeadsCalls != 40 || d.JobsCallCap != b.cfg.JevDailyCallCap || d.LeadsCallCap != leadsCap || d.AIInputTokens != 470 {
			t.Errorf("%s: jobs %d of %d, leads %d of %d, tokens %d; want jobs 7 of %d, leads 40 of %d, tokens 470", what,
				d.JobsCalls, d.JobsCallCap, d.LeadsCalls, d.LeadsCallCap, d.AIInputTokens, b.cfg.JevDailyCallCap, leadsCap)
		}
		get := func(path string) (int, string) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Cookie", cookie)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w.Code, w.Body.String()
		}
		// Jobs home's Scoring card says both purposes against their own caps.
		want := fmt.Sprintf("Jev today: 7 of %s calls for jobs · 40 of %s for leads", thousandsOf(b.cfg.JevDailyCallCap), thousandsOf(leadsCap))
		if code, page := get("/jobs/home"); code != http.StatusOK || !strings.Contains(page, want) {
			t.Errorf("%s: jobs home %d lacks %q", what, code, want)
		}
		// The Status page's Jev section meters each purpose against its own cap.
		code, page := get("/status")
		for _, want := range []string{
			fmt.Sprintf(`aria-label="Calls for jobs: 7 of %d"`, b.cfg.JevDailyCallCap),
			fmt.Sprintf(`aria-label="Calls for leads: 40 of %d"`, leadsCap),
			fmt.Sprintf("<strong>7</strong> of %s</span>", thousandsOf(b.cfg.JevDailyCallCap)),
			fmt.Sprintf("<strong>40</strong> of %s</span>", thousandsOf(leadsCap)),
		} {
			if code != http.StatusOK || !strings.Contains(page, want) {
				t.Errorf("%s: status page %d lacks %q", what, code, want)
			}
		}
	}
	check("the server's cap", b.cfg.JevLeadsDailyCallCap)
	saveRateSettings(t, `{"relevance":{"leads_daily_calls":25}}`)
	check("a saved lower cap", 25)
}

// thousandsOf writes n with comma thousands, as the console does.
func thousandsOf(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
