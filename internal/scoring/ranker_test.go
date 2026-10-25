package scoring

import (
	"cmp"
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The ref* functions are verbatim copies of Rank, SkippedOn, matchSkipRule and the helpers
// they called before the Ranker, kept as the oracle the Ranker must equal.

func refMatchSkipRule(c Candidate, rules []store.Rule) *int64 {
	for _, r := range rules {
		if r.Kind != gates.KindSkip || r.DisabledAt != nil {
			continue
		}
		var text string
		switch r.Field {
		case "company":
			text = c.Company
		case "title":
			text = c.Title
		case "description":
			text = c.Description
		case "location":
			text = c.Location
		default:
			continue
		}
		re, err := regexp.Compile("(?i)" + r.Pattern)
		if err != nil || !re.MatchString(text) {
			continue
		}
		id := r.ID
		return &id
	}
	return nil
}

func refTierOf(gs []GateVerdict, cs []CriterionVerdict) string {
	if !refComplete(cs) {
		return TierSkip
	}
	allPass := true
	for _, g := range gs {
		switch g.Verdict {
		case gates.Fail:
			return TierSkip
		case gates.Pass:
		default:
			allPass = false
		}
	}
	for _, c := range cs {
		switch c.Verdict {
		case Fail:
			return TierSkip
		case Pass:
		default:
			allPass = false
		}
	}
	if allPass {
		return TierApply
	}
	return TierYourCall
}

func refComplete(cs []CriterionVerdict) bool {
	for _, n := range candidate.CriterionNames {
		if !slices.ContainsFunc(cs, func(c CriterionVerdict) bool { return c.Criterion == n }) {
			return false
		}
	}
	return true
}

func refSkipReason(c Candidate) (reason string, failedCriteria int) {
	for _, cv := range c.Criteria {
		if cv.Verdict == Fail {
			failedCriteria++
		}
	}
	switch {
	case c.Status != "new" && c.Status != "shortlisted":
		return SkipStatus, failedCriteria
	case c.ListingStatus == store.ListingUnreachable && !c.Closed:
		return SkipUnreachable, failedCriteria
	case c.ListingStatus != store.ListingOpen || c.Closed:
		return SkipClosed, failedCriteria
	case c.SkipRuleID != nil:
		return SkipRule, failedCriteria
	}
	for _, gate := range []string{gates.GateRule, gates.GateLanguage, gates.GateLocation, gates.GateWorkplace, gates.GateWorkRights} {
		for _, g := range c.Gates {
			if g.Gate == gate && g.Verdict == gates.Fail {
				return gate, failedCriteria
			}
		}
	}
	if failedCriteria > 0 {
		return SkipCriterion, failedCriteria
	}
	if !refComplete(c.Criteria) {
		return SkipUnscored, 0
	}
	return "", 0
}

func refRank(cands []Candidate, loc *time.Location, caps Caps) Ranking {
	r := Ranking{Skipped: map[string]int{}}
	var apply, yourCall, runnerUps []Ranked
	for _, c := range cands {
		reason, failed := refSkipReason(c)
		if reason != "" {
			r.Skipped[reason]++
			// The runner-up failed one criterion and nothing else: every other one is decided.
			if reason == SkipCriterion && failed == 1 && refComplete(c.Criteria) {
				runnerUps = append(runnerUps, Ranked{Candidate: c, Tier: TierSkip, FailedCriterion: refFailedCriterion(c)})
			}
			continue
		}
		rk := Ranked{Candidate: c, Tier: refTierOf(c.Gates, c.Criteria), Unclear: refUnclear(c)}
		if rk.Tier == TierApply {
			apply = append(apply, rk)
		} else {
			yourCall = append(yourCall, rk)
		}
	}
	order := func(a, b Ranked) int { return refCompareCandidates(a.Candidate, b.Candidate, loc) }
	slices.SortFunc(apply, order)
	slices.SortFunc(yourCall, order)
	slices.SortFunc(runnerUps, order)
	r.ApplyTotal, r.YourCallTotal = len(apply), len(yourCall)
	r.Apply = apply[:min(len(apply), max(caps.Apply, 0))]
	r.YourCall = yourCall[:min(len(yourCall), max(caps.YourCall, 0))]
	if len(runnerUps) > 0 {
		ru := runnerUps[0]
		r.RunnerUp = &ru
	}
	return r
}

func refSkippedOn(cands []Candidate, loc *time.Location, day time.Time) map[string]int {
	y, m, d := day.In(loc).Date()
	out := map[string]int{}
	for _, c := range cands {
		if sy, sm, sd := c.SeenAt.In(loc).Date(); sy != y || sm != m || sd != d {
			continue
		}
		if reason, _ := refSkipReason(c); reason != "" {
			out[reason]++
		}
	}
	return out
}

func refFailedCriterion(c Candidate) string {
	for _, cv := range c.Criteria {
		if cv.Verdict == Fail {
			return cv.Criterion
		}
	}
	return ""
}

func refUnclear(c Candidate) []string {
	var out []string
	for _, g := range c.Gates {
		if g.Verdict != gates.Flag {
			continue
		}
		if g.Snippet != "" {
			out = append(out, fmt.Sprintf("%s (%q)", g.Gate, g.Snippet))
		} else {
			out = append(out, g.Gate)
		}
	}
	for _, cv := range c.Criteria {
		if cv.Verdict == Unclear {
			out = append(out, cv.Criterion)
		}
	}
	return out
}

func refCompareCandidates(a, b Candidate, loc *time.Location) int {
	if c := refCmpNilLast(refPostedDay(a.PostedAtEst, loc), refPostedDay(b.PostedAtEst, loc), func(x, y int) int { return cmp.Compare(y, x) }); c != 0 {
		return c
	}
	if c := refCmpNilLast(a.ApplicantCount, b.ApplicantCount, cmp.Compare[int]); c != 0 {
		return c
	}
	if c := cmp.Compare(refApplyRank(a.ApplyType), refApplyRank(b.ApplyType)); c != 0 {
		return c
	}
	return cmp.Compare(a.JobID, b.JobID)
}

func refPostedDay(t *time.Time, loc *time.Location) *int {
	if t == nil {
		return nil
	}
	l := t.In(loc)
	d := l.Year()*10000 + int(l.Month())*100 + l.Day()
	return &d
}

func refCmpNilLast(a, b *int, f func(x, y int) int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return f(*a, *b)
}

func refApplyRank(t string) int {
	switch t {
	case "ats", "offsite":
		return 0
	case "onsite":
		return 1
	case "simple":
		return 2
	}
	return 3
}

// rankWith feeds cands to a fresh Ranker.
func rankWith(opt RankerOptions, cands []Candidate) *Ranker {
	r := NewRanker(opt)
	for _, c := range cands {
		r.Add(c)
	}
	return r
}

var testCaps = []Caps{{}, {Apply: 1, YourCall: 1}, {Apply: 5, YourCall: 5}, {Apply: 50, YourCall: 50},
	{Apply: 0, YourCall: 5}, {Apply: 50, YourCall: 1}, noCaps, {Apply: -1, YourCall: 1 << 30}}

// assertSameAsRef checks Rank, SkippedOn and a Ranker against the reference for every cap
// set and the given days.
func assertSameAsRef(t *testing.T, name string, cands []Candidate, loc *time.Location, days ...time.Time) {
	t.Helper()
	for _, caps := range testCaps {
		want := refRank(cands, loc, caps)
		if got := Rank(cands, loc, caps); !reflect.DeepEqual(got, want) {
			t.Errorf("%s caps %+v: Rank differs from the reference\n got %s\nwant %s", name, caps, summary(got), summary(want))
		}
		for _, day := range days {
			r := rankWith(RankerOptions{Loc: loc, Caps: caps, Day: day}, cands)
			if got := r.Ranking(); !reflect.DeepEqual(got, want) {
				t.Errorf("%s caps %+v day %s: Ranking differs\n got %s\nwant %s", name, caps, day, summary(got), summary(want))
			}
			if got, want := r.SkippedOn(), refSkippedOn(cands, loc, day); !reflect.DeepEqual(got, want) {
				t.Errorf("%s day %s: Ranker.SkippedOn %v, want %v", name, day, got, want)
			}
		}
	}
	for _, day := range days {
		if got, want := SkippedOn(cands, loc, day), refSkippedOn(cands, loc, day); !reflect.DeepEqual(got, want) {
			t.Errorf("%s day %s: SkippedOn %v, want %v", name, day, got, want)
		}
	}
}

// summary is a short form of a ranking for failure messages.
func summary(r Ranking) string {
	ru := int64(-1)
	if r.RunnerUp != nil {
		ru = r.RunnerUp.JobID
	}
	return fmt.Sprintf("apply %v (%d) your call %v (%d) runner-up %d skipped %v", ids(r.Apply), r.ApplyTotal, ids(r.YourCall), r.YourCallTotal, ru, r.Skipped)
}

// TestRankerEqualsRefOnFixtures: every fixture of rank_test.go ranks the same as before.
func TestRankerEqualsRefOnFixtures(t *testing.T) {
	for _, tc := range tieBreakCases() {
		assertSameAsRef(t, tc.name, tc.cands, time.UTC, day1)
	}
	for _, loc := range []*time.Location{time.UTC, zoneUTC3} {
		assertSameAsRef(t, "day in zone", dayInZoneCands(), loc, day1, day2)
		assertSameAsRef(t, "skipped on", skippedOnCands(), loc, day1, day2, day2.AddDate(0, 0, 1), time.Time{})
	}
	assertSameAsRef(t, "tiers", tiersCands(), time.UTC, day1)
	assertSameAsRef(t, "runner-up", runnerUpCands(), time.UTC, day1)
	assertSameAsRef(t, "caps", capsCands(), time.UTC, day1)
	assertSameAsRef(t, "incomplete", incompleteCands(), time.UTC, day1)
	assertSameAsRef(t, "title check fail", []Candidate{with(cand(1), func(c *Candidate) {
		c.Criteria = []CriterionVerdict{{Criterion: "title", Verdict: Fail}}
	})}, time.UTC, day1)
}

// TestRankerColdStart: no candidates rank to empty lists, empty counts and no runner-up.
func TestRankerColdStart(t *testing.T) {
	r := NewRanker(RankerOptions{Loc: time.UTC, Caps: noCaps, Day: day1})
	got := r.Ranking()
	if got.Apply != nil || got.YourCall != nil || got.RunnerUp != nil || got.ApplyTotal != 0 || got.YourCallTotal != 0 ||
		got.Skipped == nil || len(got.Skipped) != 0 {
		t.Errorf("cold ranking %+v", got)
	}
	if on := r.SkippedOn(); on == nil || len(on) != 0 {
		t.Errorf("cold skipped on %v", on)
	}
	assertSameAsRef(t, "empty", nil, time.UTC, day1)
}

// randCands are n candidates in random order with unique job ids, drawn from few values
// so that every ordering key ties often and the job id decides.
func randCands(rng *rand.Rand, n int) []Candidate {
	posted := []*time.Time{nil, &day1, &day2, ptr(day1.Add(5 * time.Hour)), ptr(day2.Add(20 * time.Hour))}
	applicants := []*int{nil, ptr(3), ptr(25), ptr(200)}
	applyTypes := []string{"ats", "offsite", "onsite", "simple", ""}
	statuses := []string{"new", "new", "new", "shortlisted", "applied", "skipped"}
	listing := []string{"open", "open", "open", "open", "expired", "unreachable"}
	gateVerdicts := []string{"pass", "pass", "pass", "pass", "pass", "flag", "fail"}
	critVerdicts := []string{Pass, Pass, Pass, Pass, Unclear, Fail}
	seen := []time.Time{day1, day2, day1.Add(16 * time.Hour), day2.Add(15 * time.Hour)}
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }

	out := make([]Candidate, n)
	for i, id := range rng.Perm(n) {
		c := Candidate{
			JobID: int64(id + 1), Status: pick(statuses), ListingStatus: pick(listing),
			Closed: rng.Intn(20) == 0, ApplyType: pick(applyTypes),
			PostedAtEst: posted[rng.Intn(len(posted))], ApplicantCount: applicants[rng.Intn(len(applicants))],
			SeenAt: seen[rng.Intn(len(seen))],
		}
		if rng.Intn(25) == 0 {
			c.SkipRuleID = ptr(int64(rng.Intn(3) + 1))
		}
		for _, g := range []string{"rule", "language", "location", "workplace", "work_rights"} {
			v := pick(gateVerdicts)
			if g == "rule" && v == "fail" && rng.Intn(2) == 0 {
				v = "pass"
			}
			snippet := ""
			if v == "flag" && rng.Intn(2) == 0 {
				snippet = "hybrid"
			}
			c.Gates = append(c.Gates, GateVerdict{Gate: g, Verdict: v, Snippet: snippet})
		}
		for _, name := range candidate.CriterionNames {
			if rng.Intn(15) == 0 {
				continue // undecided
			}
			c.Criteria = append(c.Criteria, CriterionVerdict{Criterion: name, Verdict: pick(critVerdicts)})
		}
		out[i] = c
	}
	return out
}

// TestRankerEqualsRefOnRandomSets: seeded random sets of 10k candidates, with many ties,
// rank the same as the reference at every cap, in two zones.
func TestRankerEqualsRefOnRandomSets(t *testing.T) {
	for seed := int64(1); seed <= 3; seed++ {
		cands := randCands(rand.New(rand.NewSource(seed)), 10_000) //nolint:gosec // G404: a seeded source, so each random set repeats
		for _, loc := range []*time.Location{time.UTC, zoneUTC3} {
			assertSameAsRef(t, fmt.Sprintf("seed %d", seed), cands, loc, day1, day2)
		}
	}
}

// TestRankerUnreachableAsOpen: the unreachable view equals recheckIDs' rewrite before Rank.
func TestRankerUnreachableAsOpen(t *testing.T) {
	cands := append(randCands(rand.New(rand.NewSource(7)), 2_000), tiersCands()...) //nolint:gosec // G404: a seeded source, so each random set repeats
	for i := range cands[2_000:] {
		cands[2_000+i].JobID += 100_000
	}
	rewritten := slices.Clone(cands)
	for i := range rewritten {
		if rewritten[i].ListingStatus == store.ListingUnreachable {
			rewritten[i].ListingStatus = store.ListingOpen
		}
	}
	for _, caps := range testCaps {
		r := rankWith(RankerOptions{Loc: time.UTC, Caps: caps, Day: day2, UnreachableAsOpen: true}, cands)
		if got, want := r.Ranking(), refRank(rewritten, time.UTC, caps); !reflect.DeepEqual(got, want) {
			t.Errorf("caps %+v: unreachable view differs\n got %s\nwant %s", caps, summary(got), summary(want))
		}
		if got, want := r.SkippedOn(), refSkippedOn(rewritten, time.UTC, day2); !reflect.DeepEqual(got, want) {
			t.Errorf("unreachable view skipped on %v, want %v", got, want)
		}
	}
	if r := rankWith(RankerOptions{Loc: time.UTC, Caps: noCaps, UnreachableAsOpen: true}, tiersCands()).Ranking(); r.Skipped[SkipUnreachable] != 0 {
		t.Errorf("unreachable still skipped: %v", r.Skipped)
	}
}

// TestRankerIsReadAgain: Ranking can be read more than once, and adding goes on after it.
func TestRankerIsReadAgain(t *testing.T) {
	cands := randCands(rand.New(rand.NewSource(11)), 500) //nolint:gosec // G404: a seeded source, so each random set repeats
	r := NewRanker(RankerOptions{Loc: time.UTC, Caps: Caps{Apply: 5, YourCall: 5}})
	for _, c := range cands[:250] {
		r.Add(c)
	}
	first := r.Ranking()
	first.Skipped["mutated"] = 1
	for _, c := range cands[250:] {
		r.Add(c)
	}
	if got, want := r.Ranking(), refRank(cands, time.UTC, Caps{Apply: 5, YourCall: 5}); !reflect.DeepEqual(got, want) {
		t.Errorf("after a read\n got %s\nwant %s", summary(got), summary(want))
	}
}

// TestCompileRulesMatchesAsBefore: compiled rules pick the same rule as matching each
// rule afresh, and say when one reads the description.
func TestCompileRulesMatchesAsBefore(t *testing.T) {
	disabled := time.Now()
	rules := []store.Rule{
		{ID: 1, Kind: "flag", Field: "company", Pattern: "staffing"},
		{ID: 2, Kind: "skip", Field: "company", Pattern: "staffing", DisabledAt: &disabled},
		{ID: 3, Kind: "skip", Field: "title", Pattern: "(unclosed"},
		{ID: 4, Kind: "skip", Field: "other", Pattern: "."},
		{ID: 5, Kind: "skip", Field: "title", Pattern: `^senior\b`},
		{ID: 6, Kind: "skip", Field: "location", Pattern: "onsite only"},
		{ID: 7, Kind: "skip", Field: "description", Pattern: `clearance\s+required`},
		{ID: 8, Kind: "skip", Field: "company", Pattern: "contoso"},
	}
	cands := []Candidate{
		{JobID: 1, Company: "Contoso Staffing", Title: "Senior Engineer"},
		{JobID: 2, Company: "Fabrikam", Title: "Engineer", Location: "Exampleville, ONSITE ONLY"},
		{JobID: 3, Company: "Fabrikam", Title: "Engineer", Description: "Security CLEARANCE  required."},
		{JobID: 4, Company: "Fabrikam", Title: "Engineer", Description: "nothing"},
		{JobID: 5, Company: "contoso", Title: "Engineer"},
	}
	for _, set := range [][]store.Rule{nil, rules, rules[:4], rules[:6], rules[6:]} {
		cr := CompileRules(set)
		wantDesc := slices.ContainsFunc(set, func(r store.Rule) bool {
			return r.Kind == gates.KindSkip && r.DisabledAt == nil && r.Field == "description"
		})
		if cr.NeedsDescription() != wantDesc {
			t.Errorf("rules %d: needs description %v", len(set), cr.NeedsDescription())
		}
		for _, c := range cands {
			got, want := cr.Match(c), refMatchSkipRule(c, set)
			if (got == nil) != (want == nil) || (got != nil && *got != *want) {
				t.Errorf("rules %d, job %d: match %v, want %v", len(set), c.JobID, got, want)
			}
		}
	}
}

// TestListedAndUnclearOf: Listed is "no skip reason", UnclearOf is a listed job's Unclear.
func TestListedAndUnclearOf(t *testing.T) {
	cands := randCands(rand.New(rand.NewSource(5)), 1_000) //nolint:gosec // G404: a seeded source, so each random set repeats
	r := Rank(cands, time.UTC, noCaps)
	listed := 0
	for _, c := range cands {
		if Listed(c) {
			listed++
		}
	}
	if listed != r.ApplyTotal+r.YourCallTotal {
		t.Errorf("listed %d, ranking lists %d", listed, r.ApplyTotal+r.YourCallTotal)
	}
	for _, rk := range append(r.Apply, r.YourCall...) {
		if !reflect.DeepEqual(UnclearOf(rk.Candidate), rk.Unclear) {
			t.Errorf("job %d: UnclearOf %q, Unclear %q", rk.JobID, UnclearOf(rk.Candidate), rk.Unclear)
		}
	}
}
