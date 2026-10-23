package scoring

import (
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
)

var (
	day1 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	day2 = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
)

func ptr[T any](v T) *T { return &v }

// cand is an open, new job whose gates and criteria all pass.
func cand(id int64) Candidate {
	c := Candidate{JobID: id, ListingStatus: "open", Status: "new", ApplyType: "offsite"}
	for _, g := range []string{"language", "location", "workplace", "work_rights"} {
		c.Gates = append(c.Gates, GateVerdict{Gate: g, Verdict: "pass"})
	}
	for _, n := range []string{"title", "stack", "seniority", "location", "work_rights"} {
		c.Criteria = append(c.Criteria, CriterionVerdict{Criterion: n, Verdict: Pass})
	}
	return c
}

func with(c Candidate, f func(*Candidate)) Candidate { f(&c); return c }

func setGate(gate, verdict, snippet string) func(*Candidate) {
	return func(c *Candidate) {
		c.Gates = slices.Clone(c.Gates)
		for i := range c.Gates {
			if c.Gates[i].Gate == gate {
				c.Gates[i].Verdict, c.Gates[i].Snippet = verdict, snippet
				return
			}
		}
		c.Gates = append(c.Gates, GateVerdict{Gate: gate, Verdict: verdict, Snippet: snippet})
	}
}

func setCrit(name, verdict string) func(*Candidate) {
	return func(c *Candidate) {
		c.Criteria = slices.Clone(c.Criteria)
		for i := range c.Criteria {
			if c.Criteria[i].Criterion == name {
				c.Criteria[i].Verdict = verdict
			}
		}
	}
}

func ids(rs []Ranked) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.JobID
	}
	return out
}

var noCaps = Caps{Apply: 100, YourCall: 100}

func TestRankTieBreaks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cands []Candidate
		want  []int64
	}{
		{"newest day first", []Candidate{
			with(cand(1), func(c *Candidate) { c.PostedAtEst = &day1 }),
			with(cand(2), func(c *Candidate) { c.PostedAtEst = &day2 }),
		}, []int64{2, 1}},
		{"same day: by day, not time, so applicants decide", []Candidate{
			with(cand(1), func(c *Candidate) { c.PostedAtEst = ptr(day1.Add(5 * time.Hour)); c.ApplicantCount = ptr(80) }),
			with(cand(2), func(c *Candidate) { c.PostedAtEst = &day1; c.ApplicantCount = ptr(10) }),
		}, []int64{2, 1}},
		{"unknown posted date last", []Candidate{
			cand(1),
			with(cand(2), func(c *Candidate) { c.PostedAtEst = &day1 }),
		}, []int64{2, 1}},
		{"fewest applicants first, NULL last", []Candidate{
			cand(1),
			with(cand(2), func(c *Candidate) { c.ApplicantCount = ptr(200) }),
			with(cand(3), func(c *Candidate) { c.ApplicantCount = ptr(25) }),
		}, []int64{3, 2, 1}},
		{"apply type: ats and offsite, then onsite, then simple, then unknown", []Candidate{
			with(cand(1), func(c *Candidate) { c.ApplyType = "simple" }),
			with(cand(2), func(c *Candidate) { c.ApplyType = "" }),
			with(cand(3), func(c *Candidate) { c.ApplyType = "onsite" }),
			with(cand(4), func(c *Candidate) { c.ApplyType = "offsite" }),
			with(cand(5), func(c *Candidate) { c.ApplyType = "ats" }),
		}, []int64{4, 5, 3, 1, 2}},
		{"job id last", []Candidate{cand(9), cand(3), cand(5)}, []int64{3, 5, 9}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(Rank(tc.cands, time.UTC, noCaps).Apply)
			if !slices.Equal(got, tc.want) {
				t.Errorf("order %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRankDayInZone: the day bucket is the local day, so two postings an hour apart
// across local midnight fall on different days.
func TestRankDayInZone(t *testing.T) {
	loc := time.FixedZone("UTC+3", 3*3600)
	early := time.Date(2026, 3, 1, 20, 30, 0, 0, time.UTC) // 23:30 local, 1 March
	late := time.Date(2026, 3, 1, 21, 30, 0, 0, time.UTC)  // 00:30 local, 2 March
	cs := []Candidate{
		with(cand(1), func(c *Candidate) { c.PostedAtEst = &early; c.ApplicantCount = ptr(1) }),
		with(cand(2), func(c *Candidate) { c.PostedAtEst = &late; c.ApplicantCount = ptr(100) }),
	}
	if got := ids(Rank(cs, loc, noCaps).Apply); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("in zone: %v", got)
	}
	if got := ids(Rank(cs, time.UTC, noCaps).Apply); !slices.Equal(got, []int64{1, 2}) {
		t.Errorf("in UTC: %v", got)
	}
}

func TestRankTiersAndExclusions(t *testing.T) {
	cs := []Candidate{
		cand(1), // apply
		with(cand(2), setGate("workplace", "flag", "hybrid, 2 days in Exampleville")), // your call
		with(cand(3), setCrit("seniority", Unclear)),                                  // your call
		with(cand(4), setGate("language", "fail", "")),
		with(cand(5), setGate("location", "fail", "")),
		with(cand(6), setGate("workplace", "fail", "")),
		with(cand(7), setGate("work_rights", "fail", "")),
		with(cand(8), setGate("rule", "fail", "")),
		with(cand(9), setCrit("stack", Fail)),
		with(cand(10), func(c *Candidate) { c.ListingStatus = "expired" }),
		with(cand(11), func(c *Candidate) { c.Closed = true }),
		with(cand(12), func(c *Candidate) { c.Status = "applied" }),
		with(cand(13), func(c *Candidate) { c.Status = "shortlisted" }), // still listed
		with(cand(14), func(c *Candidate) { c.SkipRuleID = ptr(int64(3)) }),
		with(cand(15), func(c *Candidate) { c.Criteria = nil }),
		// Several reasons: counted once, under the first in precedence.
		with(cand(16), func(c *Candidate) {
			c.Status = "skipped"
			c.Closed = true
			setGate("language", "fail", "")(c)
		}),
		with(cand(17), func(c *Candidate) { setGate("language", "fail", "")(c); setCrit("title", Fail)(c) }),
		with(cand(18), func(c *Candidate) { c.ListingStatus = "unreachable" }),
		// The posting said closed: closed, whatever the recheck could reach.
		with(cand(19), func(c *Candidate) { c.ListingStatus = "unreachable"; c.Closed = true }),
	}
	r := Rank(cs, time.UTC, noCaps)
	if got := ids(r.Apply); !slices.Equal(got, []int64{1, 13}) {
		t.Errorf("apply %v", got)
	}
	if got := ids(r.YourCall); !slices.Equal(got, []int64{2, 3}) {
		t.Errorf("your call %v", got)
	}
	if got := r.YourCall[0].Unclear; !slices.Equal(got, []string{`workplace ("hybrid, 2 days in Exampleville")`}) {
		t.Errorf("unclear %q", got)
	}
	if got := r.YourCall[1].Unclear; !slices.Equal(got, []string{"seniority"}) {
		t.Errorf("unclear %q", got)
	}
	want := map[string]int{"language": 2, "location": 1, "workplace": 1, "work_rights": 1, "rule": 2,
		"criterion": 1, "closed": 3, "unreachable": 1, "status": 2, "unscored": 1}
	if len(r.Skipped) != len(want) {
		t.Errorf("skipped %v, want %v", r.Skipped, want)
	}
	for k, v := range want {
		if r.Skipped[k] != v {
			t.Errorf("skipped[%s] = %d, want %d (all %v)", k, r.Skipped[k], v, r.Skipped)
		}
	}
	if r.ApplyTotal != 2 || r.YourCallTotal != 2 {
		t.Errorf("totals %d %d", r.ApplyTotal, r.YourCallTotal)
	}
}

func TestRunnerUp(t *testing.T) {
	cs := []Candidate{
		cand(1),
		// Excluded by one criterion: candidates for runner-up; 3 was posted later.
		with(cand(2), func(c *Candidate) { setCrit("stack", Fail)(c); c.PostedAtEst = &day1 }),
		with(cand(3), func(c *Candidate) { setCrit("seniority", Fail)(c); c.PostedAtEst = &day2 }),
		// Two criteria failed, or a gate failed too: never the runner-up, however new.
		with(cand(4), func(c *Candidate) {
			setCrit("stack", Fail)(c)
			setCrit("title", Fail)(c)
			c.PostedAtEst = ptr(day2.AddDate(0, 0, 1))
		}),
		with(cand(5), func(c *Candidate) {
			setCrit("stack", Fail)(c)
			setGate("location", "fail", "")(c)
			c.PostedAtEst = ptr(day2.AddDate(0, 0, 1))
		}),
		with(cand(6), func(c *Candidate) {
			setCrit("stack", Fail)(c)
			c.Status = "applied"
			c.PostedAtEst = ptr(day2.AddDate(0, 0, 1))
		}),
	}
	r := Rank(cs, time.UTC, noCaps)
	if r.RunnerUp == nil || r.RunnerUp.JobID != 3 || r.RunnerUp.FailedCriterion != "seniority" {
		t.Fatalf("runner-up %+v", r.RunnerUp)
	}
	if r := Rank([]Candidate{cand(1)}, time.UTC, noCaps); r.RunnerUp != nil {
		t.Errorf("runner-up with none excluded by one criterion: %+v", r.RunnerUp)
	}
}

func TestRankCaps(t *testing.T) {
	var cs []Candidate
	for i := int64(1); i <= 7; i++ {
		cs = append(cs, cand(i), with(cand(100+i), setCrit("stack", Unclear)))
	}
	r := Rank(cs, time.UTC, Caps{Apply: 5, YourCall: 3})
	if got := ids(r.Apply); !slices.Equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Errorf("apply %v", got)
	}
	if got := ids(r.YourCall); !slices.Equal(got, []int64{101, 102, 103}) {
		t.Errorf("your call %v", got)
	}
	if r.ApplyTotal != 7 || r.YourCallTotal != 7 {
		t.Errorf("totals %d %d", r.ApplyTotal, r.YourCallTotal)
	}
	if r := Rank(cs, time.UTC, Caps{}); len(r.Apply) != 0 || len(r.YourCall) != 0 {
		t.Errorf("zero caps listed %d %d", len(r.Apply), len(r.YourCall))
	}
}

func TestTierOf(t *testing.T) {
	pass := []GateVerdict{{Gate: "language", Verdict: "pass"}}
	flag := []GateVerdict{{Gate: "language", Verdict: "pass"}, {Gate: "workplace", Verdict: "flag"}}
	fail := []GateVerdict{{Gate: "language", Verdict: "fail"}}
	ok := cand(0).Criteria
	unc := with(cand(0), setCrit("title", Unclear)).Criteria
	bad := with(cand(0), setCrit("title", Fail)).Criteria
	partial := ok[:4] // four of five criteria decided, all pass
	onlyFail := []CriterionVerdict{{Criterion: "title", Verdict: Fail}}
	for _, tc := range []struct {
		g    []GateVerdict
		c    []CriterionVerdict
		want string
	}{
		{pass, ok, TierApply}, {flag, ok, TierYourCall}, {pass, unc, TierYourCall},
		{fail, ok, TierSkip}, {pass, bad, TierSkip}, {flag, bad, TierSkip}, {pass, nil, TierSkip},
		{pass, partial, TierSkip}, {flag, partial, TierSkip}, {pass, onlyFail, TierSkip},
	} {
		if got := TierOf(tc.g, tc.c); got != tc.want {
			t.Errorf("TierOf(%v, %v) = %s, want %s", tc.g, tc.c, got, tc.want)
		}
	}
}

func TestCandidatesFromMatchesNewSkipRules(t *testing.T) {
	in := []store.RankInput{{JobID: 1, Company: "Contoso Staffing", Title: "Rust Engineer", Status: "new", ListingStatus: "open", SeenAt: day1,
		Gates:     []store.GateResult{{Gate: "language", Verdict: "pass", Snippet: "x"}},
		Decisions: []store.Decision{{Criterion: "title", Verdict: Pass}}}}
	disabled := time.Now()
	rules := []store.Rule{
		{ID: 1, Kind: "flag", Field: "company", Pattern: "staffing"},
		{ID: 2, Kind: "skip", Field: "company", Pattern: "staffing", DisabledAt: &disabled},
		{ID: 3, Kind: "skip", Field: "title", Pattern: "(unclosed"},
		{ID: 4, Kind: "skip", Field: "company", Pattern: `contoso\s+STAFFING`},
	}
	got := CandidatesFrom(in, rules)
	if got[0].SkipRuleID == nil || *got[0].SkipRuleID != 4 {
		t.Fatalf("skip rule %v", got[0].SkipRuleID)
	}
	if len(got[0].Gates) != 1 || got[0].Gates[0].Snippet != "x" || len(got[0].Criteria) != 1 || !got[0].SeenAt.Equal(day1) {
		t.Errorf("converted %+v", got[0])
	}
	if r := Rank(got, time.UTC, noCaps); r.Skipped[SkipRule] != 1 {
		t.Errorf("skipped %v", r.Skipped)
	}
	if got := CandidatesFrom(in, rules[:3]); got[0].SkipRuleID != nil {
		t.Errorf("flag, disabled and broken rules matched: %v", *got[0].SkipRuleID)
	}
}

// TestIncompleteCriteriaAreUnscored: a job missing any criterion's decision is never
// listed, Apply least of all; it counts as unscored and is no runner-up.
func TestIncompleteCriteriaAreUnscored(t *testing.T) {
	missing := with(cand(1), func(c *Candidate) { c.Criteria = c.Criteria[:4] })
	none := with(cand(2), func(c *Candidate) { c.Criteria = nil })
	failedAndMissing := with(cand(3), func(c *Candidate) {
		c.Criteria = []CriterionVerdict{{Criterion: "title", Verdict: Fail}}
	})
	r := Rank([]Candidate{missing, none, failedAndMissing, cand(4)}, time.UTC, noCaps)
	if got := ids(r.Apply); !slices.Equal(got, []int64{4}) || len(r.YourCall) != 0 {
		t.Fatalf("apply %v, your call %v; want only the complete job 4", got, ids(r.YourCall))
	}
	if r.Skipped[SkipUnscored] != 2 || r.Skipped[SkipCriterion] != 1 {
		t.Errorf("skipped %v, want unscored 2 and criterion 1", r.Skipped)
	}
	if r.RunnerUp != nil {
		t.Errorf("runner-up %d: a job with undecided criteria is not one fail away", r.RunnerUp.JobID)
	}
}

// TestSkippedOn: only the jobs seen on the brief's day count, by their day in loc, and a
// job seen again that day counts again.
func TestSkippedOn(t *testing.T) {
	loc := time.FixedZone("UTC+3", 3*3600)
	seen := func(at time.Time) func(*Candidate) { return func(c *Candidate) { c.SeenAt = at } }
	cs := []Candidate{
		with(cand(1), seen(day2)), // listed: not counted
		with(with(cand(2), setGate("language", "fail", "")), seen(day2)),
		with(with(cand(3), setGate("language", "fail", "")), seen(day1)),                                          // yesterday
		with(with(cand(4), func(c *Candidate) { c.ListingStatus = "unreachable" }), seen(day1.Add(16*time.Hour))), // 04:00 on day 2 in loc
		with(with(cand(5), func(c *Candidate) { c.Criteria = nil }), seen(day2)),
		with(with(cand(6), func(c *Candidate) { c.Status = "applied" }), seen(day2.Add(15*time.Hour))), // day 3 in loc
	}
	got := SkippedOn(cs, loc, day2)
	want := map[string]int{"language": 1, "unreachable": 1, "unscored": 1}
	if len(got) != len(want) {
		t.Fatalf("skipped on day 2 %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("skipped[%s] = %d, want %d (all %v)", k, got[k], v, got)
		}
	}
	if got := SkippedOn(cs, loc, day1); got["language"] != 1 || len(got) != 1 {
		t.Errorf("skipped on day 1 %v", got)
	}
	// Rank still counts every day.
	if r := Rank(cs, loc, noCaps); r.Skipped["language"] != 2 || r.Skipped["status"] != 1 {
		t.Errorf("rank skipped %v", r.Skipped)
	}
}
