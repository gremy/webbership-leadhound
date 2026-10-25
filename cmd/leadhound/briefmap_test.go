package main

import (
	"bytes"
	"context"
	"github.com/gremy/webbership-leadhound/internal/ats"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func TestMatchedKeywords(t *testing.T) {
	text := "We build in Go and PostgreSQL.\nSome Rust, no gRPCish things. Postgres too."
	got := matchedKeywords(text, []string{"rust", "grpc", "postgres", "postgresql", "linux", "rust"})
	if want := []string{"rust", "postgres", "postgresql"}; !slices.Equal(got, want) {
		t.Errorf("matched = %v, want %v", got, want)
	}
	if got := matchedKeywords(text, nil); got != nil {
		t.Errorf("no keywords = %v", got)
	}
}

func TestOverlaps(t *testing.T) {
	a := "… within the EU. About the role Experience with Go and PostgreSQL. You will design and maintain backend services in Go."
	b := "Fully remote within the EU. About the role Experience with Go and PostgreSQL. You will design and maintain backend servi…"
	if !overlaps(a, b) || !overlaps(b, a) {
		t.Error("two windows of one passage read as different")
	}
	if overlaps("You will use Rust and Postgres daily.", "Senior role.") {
		t.Error("different lines read as one passage")
	}
}

func TestCollectStateAndCanary(t *testing.T) {
	done := time.Now()
	for _, c := range []struct {
		run  store.Run
		ats  string
		want string
	}{
		{store.Run{FinishedAt: &done}, "", brief.StateOK},
		{store.Run{FinishedAt: &done, StopReason: "budget"}, "", brief.StateOK},
		{store.Run{FinishedAt: &done, StopReason: "parser_broke"}, "", brief.StateParserBroke},
		{store.Run{FinishedAt: &done, StopReason: "failure_rate"}, "", "failure_rate"},
		{store.Run{FinishedAt: &done, Note: "failed: boom"}, "", "failed"},
		{store.Run{}, "", "still_running"},
		// An ATS: its budget covers every board, and its failed run names the ATS first.
		{store.Run{FinishedAt: &done, Note: "lever: 2 boards (2 ok, 0 failed, 0 parser broke, 0 not found)"}, "lever", brief.StateOK},
		{store.Run{FinishedAt: &done, StopReason: "budget"}, "lever", "budget"},
		{store.Run{FinishedAt: &done, StopReason: "parser_broke"}, "lever", brief.StateParserBroke},
		{store.Run{FinishedAt: &done, StopReason: "throttled"}, "lever", brief.StateThrottled},
		{store.Run{FinishedAt: &done, Note: "lever: failed: boom"}, "lever", "failed"},
		{store.Run{FinishedAt: &done, Note: "lever: failed: boom"}, "", brief.StateOK},
	} {
		if got := collectState(c.run, c.ats); got != c.want {
			t.Errorf("collectState(%+v, %q) = %s, want %s", c.run, c.ats, got, c.want)
		}
	}
	if !canaryPassed(store.Run{FinishedAt: &done, Note: "10 cards"}) {
		t.Error("a passed canary read as failed")
	}
	for _, r := range []store.Run{
		{FinishedAt: &done, Note: "failed: status 500"},
		{FinishedAt: &done, StopReason: "parser_broke"},
		{FinishedAt: &done, StopReason: "throttled", Note: "paused today: throttled"},
		{},
	} {
		if canaryPassed(r) {
			t.Errorf("canaryPassed(%+v) = true", r)
		}
	}
}

// TestBriefJobMapping: criteria in order with keyword notes, evidence without contact
// lines, and what is unclear from flagged gates and unclear criteria.
func TestBriefJobMapping(t *testing.T) {
	c := scoring.Candidate{
		JobID: 7, Title: "Rust Engineer", Company: "Northwind Labs",
		Description: "Write us at jobs@northwind.example about Rust.\nYou will use Rust and Postgres daily.\nSenior role.",
		Gates: []scoring.GateVerdict{
			{Gate: gates.GateLanguage, Verdict: gates.Pass},
			{Gate: gates.GateWorkplace, Verdict: gates.Flag, Snippet: "hybrid, 2 days in Exampleville"},
		},
		Criteria: []scoring.CriterionVerdict{
			{Criterion: "work_rights", Verdict: scoring.Pass},
			{Criterion: "stack", Verdict: scoring.Pass},
			{Criterion: "title", Verdict: scoring.Pass},
			{Criterion: "seniority", Verdict: scoring.Unclear},
			{Criterion: "location", Verdict: scoring.Pass},
		},
	}
	kws := map[string][]string{"stack": {"rust", "postgres"}, "seniority": {"senior"}}
	j := briefJob(c, "https://www.linkedin.com/jobs/view/7", kws)
	var names []string
	for _, cr := range j.Criteria {
		names = append(names, cr.Name)
	}
	if !slices.Equal(names, []string{"title", "stack", "seniority", "location", "work_rights"}) {
		t.Errorf("criteria order = %v", names)
	}
	if j.Criteria[1].Note != "rust, postgres" || j.Criteria[2].Note != "senior" || j.Criteria[0].Note != "" {
		t.Errorf("notes = %+v", j.Criteria)
	}
	for _, e := range j.Evidence {
		if strings.Contains(e, "@") {
			t.Errorf("evidence quotes a contact: %q", e)
		}
	}
	if len(j.Evidence) != 2 || !strings.Contains(j.Evidence[0], "Rust and Postgres") || j.Evidence[1] != "Senior role." {
		t.Errorf("evidence = %q", j.Evidence)
	}
	want := []brief.Point{{Name: gates.GateWorkplace, Snippet: "hybrid, 2 days in Exampleville"}, {Name: "seniority"}}
	if !slices.Equal(j.Unclear, want) {
		t.Errorf("unclear = %+v, want %+v", j.Unclear, want)
	}
}

// TestGateSnippetContactFilter: a flagged gate's snippet that looks like a contact is not
// quoted; the gate is still named as unclear.
func TestGateSnippetContactFilter(t *testing.T) {
	c := scoring.Candidate{JobID: 8, Title: "Go Engineer", Company: "Northwind Labs", Gates: []scoring.GateVerdict{
		{Gate: gates.GateLocation, Verdict: gates.Flag, Snippet: "remote, ask jobs@northwind.example"},
		{Gate: gates.GateWorkplace, Verdict: gates.Flag, Snippet: "hybrid, see example.invalid/in/sample"},
		{Gate: gates.GateWorkRights, Verdict: gates.Flag, Snippet: "EU work permit needed"},
	}}
	j := briefJob(c, "", nil)
	want := []brief.Point{{Name: gates.GateLocation}, {Name: gates.GateWorkplace}, {Name: gates.GateWorkRights, Snippet: "EU work permit needed"}}
	if !slices.Equal(j.Unclear, want) {
		t.Errorf("unclear = %+v, want %+v", j.Unclear, want)
	}
}

func TestScoringAndRecheckState(t *testing.T) {
	done := time.Now()
	for _, c := range []struct {
		run  store.Run
		want string
	}{
		{store.Run{FinishedAt: &done, Note: "3 scored"}, brief.ScoringOK},
		{store.Run{FinishedAt: &done, StopReason: scoring.StopSpendCap}, brief.ScoringSpendCap},
		{store.Run{FinishedAt: &done, StopReason: scoring.StopThrottled}, brief.ScoringThrottled},
		{store.Run{FinishedAt: &done, StopReason: scoring.StopBudget, Note: "120 scored"}, brief.ScoringBudget},
		{store.Run{FinishedAt: &done, StopReason: scoring.StopBudget, Note: "1 scored · error"}, brief.ScoringFailed},
		{store.Run{FinishedAt: &done, Note: "1 scored · error"}, brief.ScoringFailed},
		{store.Run{FinishedAt: &done, StopReason: "other"}, brief.ScoringFailed},
		{store.Run{}, brief.ScoringFailed},
	} {
		if got := scoringState(c.run); got != c.want {
			t.Errorf("scoringState(%+v) = %s, want %s", c.run, got, c.want)
		}
	}
	for _, c := range []struct {
		run  store.Run
		want string
	}{
		{store.Run{FinishedAt: &done, Note: "2 due, 2 open, 0 expired, 0 unreachable"}, brief.RecheckOK},
		{store.Run{FinishedAt: &done, Note: "failed: boom"}, brief.RecheckFailed},
		{store.Run{FinishedAt: &done, StopReason: "parser_broke"}, "parser_broke"},
		{store.Run{FinishedAt: &done, StopReason: "throttled", Note: "paused today: throttled; 2 left out"}, "throttled"},
		{store.Run{FinishedAt: &done, StopReason: "budget"}, "budget"},
		{store.Run{}, brief.RecheckFailed},
	} {
		if got := recheckState(c.run); got != c.want {
			t.Errorf("recheckState(%+v) = %s, want %s", c.run, got, c.want)
		}
	}
}

func TestBriefAndTrackUsage(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"brief", "extra"}, "usage: leadhound brief"},
		{[]string{"brief", "--day", "23/10/2026"}, "--day must be YYYY-MM-DD"},
		{[]string{"track"}, "usage: leadhound track"},
		{[]string{"track", "1"}, "usage: leadhound track"},
		{[]string{"track", "1", "skip", "--day", "yesterday"}, "--day must be YYYY-MM-DD"},
		{[]string{"track", "list", "--status", "nope"}, "--status must be"},
		{[]string{"recheck", "extra"}, "usage: leadhound recheck"},
	} {
		var stderr bytes.Buffer
		captureStdout(t)
		if code := run(context.Background(), c.args, &stderr); code != 2 || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", c.args, code, stderr.String(), c.want)
		}
	}
}

// TestRankAccNotSeen: an open ATS job its board has not listed since the cutoff is set
// apart and counted per source only when the ranking would list it; a LinkedIn job, a
// closed listing or one seen since the cutoff is ranked.
func TestRankAccNotSeen(t *testing.T) {
	day := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	old, recent := day.AddDate(0, 0, -brief.BoardStaleDays-1), day.AddDate(0, 0, -1)
	row := func(id int64, src string, lastSeen time.Time) store.RankRow {
		r := store.RankRow{JobID: id, Source: src, Status: "new", ListingStatus: store.ListingOpen, LastSeenAt: lastSeen}
		for _, n := range candidate.CriterionNames {
			r.Decisions = append(r.Decisions, store.DecisionVerdictRow{Criterion: n, Verdict: scoring.Pass})
		}
		return r
	}
	failed := row(4, "lever", old)
	failed.Gates = []store.GateVerdictRow{{Gate: gates.GateLanguage, Verdict: gates.Fail}}
	expired := row(6, "greenhouse", old)
	expired.ListingStatus = "expired"
	a := newRankAcc(time.UTC, day, rankOptions{caps: scoring.Caps{Apply: 10, YourCall: 10}}, scoring.CompiledRules{})
	for _, r := range []store.RankRow{row(1, "greenhouse", old), row(2, "greenhouse", old), row(3, "ashby-fixture", old),
		failed, row(5, "linkedin", old), expired, row(7, "lever", recent)} {
		a.add(r)
	}
	if want := map[string]int{"greenhouse": 2, "ashby-fixture": 1}; !reflect.DeepEqual(a.notSeen, want) {
		t.Errorf("not seen = %v, want %v", a.notSeen, want)
	}
	r := a.ranker.Ranking()
	var listed []int64
	for _, rk := range r.Apply {
		listed = append(listed, rk.JobID)
	}
	if want := []int64{5, 7}; !slices.Equal(listed, want) || r.Skipped[scoring.SkipClosed] != 1 {
		t.Errorf("ranked %v, skipped %v; want %v listed and the expired one skipped", listed, r.Skipped, want)
	}
}

func TestATSSources(t *testing.T) {
	want := []string{"greenhouse", "lever", "ashby", "smartrecruiters", "teamtailor", "personio",
		"greenhouse-fixture", "lever-fixture", "ashby-fixture", "smartrecruiters-fixture", "teamtailor-fixture", "personio-fixture"}
	if got := atsSources(); !slices.Equal(got, want) {
		t.Errorf("atsSources = %v, want %v", got, want)
	}
	// Every ATS has a health line, live and in a dry run, so the not-seen rule covers it.
	for _, name := range ats.Names {
		for _, src := range []string{name, ats.SourceFixture(name)} {
			if !slices.ContainsFunc(briefSources, func(bs briefSource) bool { return bs.source == src && bs.ats == name && bs.name != "" }) {
				t.Errorf("no brief source for %s", src)
			}
		}
	}
}
