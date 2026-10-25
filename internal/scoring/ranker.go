package scoring

import (
	"container/heap"
	"maps"
	"regexp"
	"slices"
	"time"

	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// CompiledRules are the active skip rules compiled once, for matching many candidates.
type CompiledRules struct {
	rules    []compiledRule
	needDesc bool
}

type compiledRule struct {
	field string
	re    *regexp.Regexp
	id    int64
}

// CompileRules keeps the active skip rules on a field the ranking reads, in rule order, as
// the rule gate reads them (RE2, case-insensitive). A pattern that does not compile is
// dropped here; the rule gate already flags it.
func CompileRules(rules []store.Rule) CompiledRules {
	var out CompiledRules
	for _, r := range rules {
		if r.Kind != gates.KindSkip || r.DisabledAt != nil {
			continue
		}
		switch r.Field {
		case "company", "title", "description", "location":
		default:
			continue
		}
		re, err := regexp.Compile("(?i)" + r.Pattern)
		if err != nil {
			continue
		}
		out.rules = append(out.rules, compiledRule{field: r.Field, re: re, id: r.ID})
		if r.Field == "description" {
			out.needDesc = true
		}
	}
	return out
}

// NeedsDescription reports whether an active skip rule reads the description.
func (r CompiledRules) NeedsDescription() bool { return r.needDesc }

// Match returns the id of the first skip rule matching c's text, or nil.
func (r CompiledRules) Match(c Candidate) *int64 {
	for _, cr := range r.rules {
		var text string
		switch cr.field {
		case "company":
			text = c.Company
		case "title":
			text = c.Title
		case "description":
			text = c.Description
		case "location":
			text = c.Location
		}
		if cr.re.MatchString(text) {
			id := cr.id
			return &id
		}
	}
	return nil
}

// Listed reports whether c would be listed in a tier: no skip reason applies.
func Listed(c Candidate) bool {
	reason, _ := skipReason(c)
	return reason == ""
}

// UnclearOf names each flagged gate (with its snippet) and each unclear criterion of c, as
// Ranked.Unclear does. A caller that fills a kept candidate's gate details after ranking
// recomputes Unclear with it.
func UnclearOf(c Candidate) []string { return unclear(c) }

// RankerOptions configure a Ranker.
type RankerOptions struct {
	Loc  *time.Location
	Caps Caps
	Day  time.Time // SkippedOn's day; zero counts no day
	// UnreachableAsOpen ranks an unreachable listing as if it were open (recheckIDs' view).
	UnreachableAsOpen bool
}

// Ranker ranks candidates one at a time and keeps only what Rank returns: the top Caps
// per tier, the best runner-up, the totals and the skip counts, so memory follows the
// caps, not the store. Job ids are unique, so compareCandidates is a total order and the
// kept lists are exactly Rank's.
type Ranker struct {
	opt               RankerOptions
	countDay          bool
	y                 int
	m                 time.Month
	d                 int
	apply, yourCall   tierHeap
	applyN, yourCallN int
	runnerUp          *Candidate
	skipped           map[string]int
	skippedOn         map[string]int
}

// NewRanker returns an empty Ranker.
func NewRanker(opt RankerOptions) *Ranker {
	return newRanker(opt, !opt.Day.IsZero())
}

func newRanker(opt RankerOptions, countDay bool) *Ranker {
	r := &Ranker{
		opt: opt, countDay: countDay,
		apply:    tierHeap{loc: opt.Loc, max: max(opt.Caps.Apply, 0)},
		yourCall: tierHeap{loc: opt.Loc, max: max(opt.Caps.YourCall, 0)},
		skipped:  map[string]int{}, skippedOn: map[string]int{},
	}
	if countDay {
		r.y, r.m, r.d = opt.Day.In(opt.Loc).Date()
	}
	return r
}

// Add ranks one candidate.
func (r *Ranker) Add(c Candidate) {
	if r.opt.UnreachableAsOpen && c.ListingStatus == store.ListingUnreachable {
		c.ListingStatus = store.ListingOpen
	}
	reason, failed := skipReason(c)
	if reason != "" {
		r.skipped[reason]++
		if r.countDay {
			if y, m, d := c.SeenAt.In(r.opt.Loc).Date(); y == r.y && m == r.m && d == r.d {
				r.skippedOn[reason]++
			}
		}
		// The runner-up failed one criterion and nothing else: every other one is decided.
		if reason == SkipCriterion && failed == 1 && complete(c.Criteria) &&
			(r.runnerUp == nil || compareCandidates(c, *r.runnerUp, r.opt.Loc) < 0) {
			r.runnerUp = &c
		}
		return
	}
	if TierOf(c.Gates, c.Criteria) == TierApply {
		r.applyN++
		r.apply.offer(c)
	} else {
		r.yourCallN++
		r.yourCall.offer(c)
	}
}

// Ranking is Rank over every candidate added.
func (r *Ranker) Ranking() Ranking {
	out := Ranking{
		Apply:         r.apply.ranked(TierApply, r.applyN),
		YourCall:      r.yourCall.ranked(TierYourCall, r.yourCallN),
		ApplyTotal:    r.applyN,
		YourCallTotal: r.yourCallN,
		Skipped:       maps.Clone(r.skipped),
	}
	if r.runnerUp != nil {
		out.RunnerUp = &Ranked{Candidate: *r.runnerUp, Tier: TierSkip, FailedCriterion: failedCriterion(*r.runnerUp)}
	}
	return out
}

// SkippedOn is SkippedOn over every candidate added, for the options' Day.
func (r *Ranker) SkippedOn() map[string]int { return maps.Clone(r.skippedOn) }

// tierHeap keeps the best max candidates of a tier, worst at the root.
type tierHeap struct {
	items []Candidate
	loc   *time.Location
	max   int
}

func (h *tierHeap) Len() int           { return len(h.items) }
func (h *tierHeap) Less(i, j int) bool { return compareCandidates(h.items[i], h.items[j], h.loc) > 0 }
func (h *tierHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *tierHeap) Push(x any)         { h.items = append(h.items, x.(Candidate)) }
func (h *tierHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

// offer keeps c when the tier has room or c ranks before its worst kept candidate.
func (h *tierHeap) offer(c Candidate) {
	switch {
	case h.max == 0:
	case len(h.items) < h.max:
		heap.Push(h, c)
	case compareCandidates(c, h.items[0], h.loc) < 0:
		h.items[0] = c
		heap.Fix(h, 0)
	}
}

// ranked is the kept candidates in rank order. Like Rank's capped slice, it is nil only
// when the tier had no candidate at all.
func (h *tierHeap) ranked(tier string, total int) []Ranked {
	if total == 0 {
		return nil
	}
	cs := slices.Clone(h.items)
	slices.SortFunc(cs, func(a, b Candidate) int { return compareCandidates(a, b, h.loc) })
	out := make([]Ranked, 0, len(cs))
	for _, c := range cs {
		out = append(out, Ranked{Candidate: c, Tier: tier, Unclear: unclear(c)})
	}
	return out
}
