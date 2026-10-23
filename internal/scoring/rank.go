package scoring

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Tiers, as the brief and the golden set name them.
const (
	TierApply    = "apply"
	TierYourCall = "your_call"
	TierSkip     = "skip"
)

// Skip reasons, the top reason a job is left out of the brief. A job counts once, under
// the first of these that applies.
const (
	SkipStatus     = "status"      // applied, skipped or past them
	SkipClosed     = "closed"      // the listing is no longer open
	SkipRule       = "rule"        // a skip rule matched
	SkipLanguage   = "language"    // the language gate failed
	SkipLocation   = "location"    // the location gate failed
	SkipWorkplace  = "workplace"   // the workplace gate failed
	SkipWorkRights = "work_rights" // the work-rights gate failed
	SkipCriterion  = "criterion"   // a criterion failed
	SkipUnscored   = "unscored"    // the gates passed but not every criterion is decided yet
)

// SkipReasons lists the skip reasons in precedence order.
var SkipReasons = []string{SkipStatus, SkipClosed, SkipRule, SkipLanguage, SkipLocation, SkipWorkplace, SkipWorkRights, SkipCriterion, SkipUnscored}

// Verdict pairs, as the ranking reads them.
type (
	// GateVerdict is one gate result of the current observation.
	GateVerdict struct{ Gate, Verdict, Reason, Snippet string }
	// CriterionVerdict is the latest decision for one criterion.
	CriterionVerdict struct{ Criterion, Verdict, Reason string }
)

// Candidate is one job as the ranking sees it.
type Candidate struct {
	JobID          int64
	Title          string
	Company        string
	Location       string
	Description    string
	ListingStatus  string
	Status         string
	Closed         bool
	PostedAtEst    *time.Time
	ApplicantCount *int
	ApplyType      string // the effective apply type
	Gates          []GateVerdict
	Criteria       []CriterionVerdict
	SkipRuleID     *int64 // an active skip rule matches the current text
}

// Ranked is a listed job and what is unclear about it.
type Ranked struct {
	Candidate
	Tier string
	// Unclear names each flagged gate (with its snippet) and each unclear criterion.
	Unclear []string
	// FailedCriterion is set on the runner-up: the one criterion that failed.
	FailedCriterion string
}

// Caps are the brief's limits per tier.
type Caps struct{ Apply, YourCall int }

// Ranking is the ordered, capped brief lists and what was left out.
type Ranking struct {
	Apply, YourCall []Ranked
	// ApplyTotal and YourCallTotal count every job in the tier, before the caps.
	ApplyTotal, YourCallTotal int
	// RunnerUp is the best job left out by exactly one failed criterion and nothing else.
	RunnerUp *Ranked
	// Skipped counts the jobs left out, by their top reason.
	Skipped map[string]int
}

// CandidatesFrom converts the store's rows, matching every active skip rule against the
// current text, so a rule added since the last observation already applies.
func CandidatesFrom(ins []store.RankInput, rules []store.Rule) []Candidate {
	out := make([]Candidate, len(ins))
	for i, in := range ins {
		c := Candidate{
			JobID: in.JobID, Title: in.Title, Company: in.Company, Location: in.Location,
			Description: in.Description, ListingStatus: in.ListingStatus, Status: in.Status,
			Closed: in.Closed, PostedAtEst: in.PostedAtEst, ApplicantCount: in.ApplicantCount,
			ApplyType: in.ApplyType,
		}
		for _, g := range in.Gates {
			c.Gates = append(c.Gates, GateVerdict{Gate: g.Gate, Verdict: g.Verdict, Reason: g.Reason, Snippet: g.Snippet})
		}
		for _, d := range in.Decisions {
			c.Criteria = append(c.Criteria, CriterionVerdict{Criterion: d.Criterion, Verdict: d.Verdict, Reason: d.Reason})
		}
		c.SkipRuleID = matchSkipRule(c, rules)
		out[i] = c
	}
	return out
}

// matchSkipRule returns the id of the first skip rule matching the candidate's text, as
// the rule gate reads it (RE2, case-insensitive). A pattern that does not compile is
// ignored here; the rule gate already flags it.
func matchSkipRule(c Candidate, rules []store.Rule) *int64 {
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

// TierOf is the tier the gates and criteria alone give: skip on any fail, apply when all
// pass, your call otherwise. A verdict never acts alone: unless every criterion in
// candidate.CriterionNames is decided, it is skip (the ranking counts it as unscored).
func TierOf(gs []GateVerdict, cs []CriterionVerdict) string {
	if !complete(cs) {
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

// complete reports whether every criterion in candidate.CriterionNames has a verdict.
func complete(cs []CriterionVerdict) bool {
	for _, n := range candidate.CriterionNames {
		if !slices.ContainsFunc(cs, func(c CriterionVerdict) bool { return c.Criterion == n }) {
			return false
		}
	}
	return true
}

// skipReason is the candidate's top reason to be left out, or "" when it is listed.
// failedCriteria is how many criteria failed.
func skipReason(c Candidate) (reason string, failedCriteria int) {
	for _, cv := range c.Criteria {
		if cv.Verdict == Fail {
			failedCriteria++
		}
	}
	switch {
	case c.Status != "new" && c.Status != "shortlisted":
		return SkipStatus, failedCriteria
	case c.ListingStatus != "open" || c.Closed:
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
	if !complete(c.Criteria) {
		return SkipUnscored, 0
	}
	return "", 0
}

// Rank applies the ranking: it leaves out what cannot be applied for, splits the rest
// into Apply and Your call, sorts each tier, caps it, and picks the runner-up.
func Rank(cands []Candidate, loc *time.Location, caps Caps) Ranking {
	r := Ranking{Skipped: map[string]int{}}
	var apply, yourCall, runnerUps []Ranked
	for _, c := range cands {
		reason, failed := skipReason(c)
		if reason != "" {
			r.Skipped[reason]++
			// The runner-up failed one criterion and nothing else: every other one is decided.
			if reason == SkipCriterion && failed == 1 && complete(c.Criteria) {
				runnerUps = append(runnerUps, Ranked{Candidate: c, Tier: TierSkip, FailedCriterion: failedCriterion(c)})
			}
			continue
		}
		rk := Ranked{Candidate: c, Tier: TierOf(c.Gates, c.Criteria), Unclear: unclear(c)}
		if rk.Tier == TierApply {
			apply = append(apply, rk)
		} else {
			yourCall = append(yourCall, rk)
		}
	}
	order := func(a, b Ranked) int { return compareCandidates(a.Candidate, b.Candidate, loc) }
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

func failedCriterion(c Candidate) string {
	for _, cv := range c.Criteria {
		if cv.Verdict == Fail {
			return cv.Criterion
		}
	}
	return ""
}

// unclear names each flagged gate, with its snippet when there is one, then each unclear
// criterion.
func unclear(c Candidate) []string {
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

// compareCandidates is the order within a tier: posted newest first by day in loc (unknown
// last), then fewest applicants (unknown last), then the apply type (ats or offsite, then
// onsite, then simple), then job id.
func compareCandidates(a, b Candidate, loc *time.Location) int {
	if c := cmpNilLast(postedDay(a.PostedAtEst, loc), postedDay(b.PostedAtEst, loc), func(x, y int) int { return cmp.Compare(y, x) }); c != 0 {
		return c
	}
	if c := cmpNilLast(a.ApplicantCount, b.ApplicantCount, cmp.Compare[int]); c != 0 {
		return c
	}
	if c := cmp.Compare(applyRank(a.ApplyType), applyRank(b.ApplyType)); c != 0 {
		return c
	}
	return cmp.Compare(a.JobID, b.JobID)
}

// postedDay is the posting's calendar day in loc as yyyymmdd, or nil when unknown.
func postedDay(t *time.Time, loc *time.Location) *int {
	if t == nil {
		return nil
	}
	l := t.In(loc)
	d := l.Year()*10000 + int(l.Month())*100 + l.Day()
	return &d
}

// cmpNilLast compares two optional values with less, a nil value sorting last.
func cmpNilLast(a, b *int, f func(x, y int) int) int {
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

// applyRank orders apply types: an applicant tracking system or the employer's own site
// first, LinkedIn's onsite form next, its one-click apply after, unknown last.
func applyRank(t string) int {
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
