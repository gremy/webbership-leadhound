package outreach

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// The trust gate's thresholds (plan CP4): a campaign may draft with Claude only when its
// latest recorded eval regenerated at least GateMinReviewed reviewed leads, passed the
// grounding check first try in at least GateFirstTry of its runs and after the one retry in
// at least GateAfterRetry, and the operator approved at least GateApproved of the last
// GateLastReviews reviewed Claude drafts (which needs that many reviews).
const (
	GateMinReviewed = 20
	GateFirstTry    = 0.90
	GateAfterRetry  = 0.98
	GateApproved    = 0.80
	GateLastReviews = 20
)

// gateEpsilon keeps a share that is exactly a threshold (18 of 20 = 0.9) on the passing side
// whatever the float rounding.
const gateEpsilon = 1e-9

// GateEval is what the gate reads from a campaign's latest recorded eval.
type GateEval struct {
	Reviewed       int     // reviewed leads the eval regenerated
	FirstTryPass   float64 // share of runs grounded on the first try, 0..1
	AfterRetryPass float64 // share grounded after the one retry, 0..1
	At             time.Time
}

// GateReviews is what the gate reads from the last GateLastReviews reviewed Claude drafts:
// how many there are (at most GateLastReviews) and how many the operator approved.
type GateReviews struct {
	Reviewed, Approved int
}

// Share is Approved / Reviewed, 0 when nothing was reviewed.
func (r GateReviews) Share() float64 {
	if r.Reviewed <= 0 {
		return 0
	}
	return float64(r.Approved) / float64(r.Reviewed)
}

// GateSource is the slice of the store the gate reads. This package never reads the store
// itself (the store imports it), so the caller adapts its own queries.
type GateSource interface {
	// LatestDraftEval returns the campaign's latest recorded eval; ok false when none.
	LatestDraftEval(ctx context.Context, campaignID int64) (e GateEval, ok bool, err error)
	// LastDraftReviews counts the latest verdict of each of the last n reviewed Claude
	// drafts of the campaign.
	LastDraftReviews(ctx context.Context, campaignID int64, n int) (GateReviews, error)
}

// GateState is the gate's verdict for one campaign, with what it read and, when it is
// closed, every reason in plain words.
type GateState struct {
	Trusted bool
	Eval    *GateEval // nil: no eval recorded yet
	Reviews GateReviews
	Reasons []string // empty exactly when Trusted
}

// Gate reads the campaign's latest eval and last reviews from src and judges them.
func Gate(ctx context.Context, src GateSource, campaignID int64) (GateState, error) {
	e, ok, err := src.LatestDraftEval(ctx, campaignID)
	if err != nil {
		return GateState{}, fmt.Errorf("outreach: gate: campaign %d: %w", campaignID, err)
	}
	rv, err := src.LastDraftReviews(ctx, campaignID, GateLastReviews)
	if err != nil {
		return GateState{}, fmt.Errorf("outreach: gate: campaign %d: %w", campaignID, err)
	}
	var ep *GateEval
	if ok {
		ep = &e
	}
	return JudgeGate(ep, rv), nil
}

// JudgeGate is the gate's rule on plain values: every threshold is "at least", so a value
// exactly at it passes.
func JudgeGate(e *GateEval, rv GateReviews) GateState {
	s := GateState{Eval: e, Reviews: rv}
	if rv.Reviewed < GateLastReviews {
		s.Reasons = append(s.Reasons, fmt.Sprintf("%d of %d drafts reviewed", rv.Reviewed, GateLastReviews))
	}
	if rv.Reviewed > 0 && rv.Share()+gateEpsilon < GateApproved {
		s.Reasons = append(s.Reasons, fmt.Sprintf("%d%% of the last %d reviewed drafts approved (needs %d%%)",
			pct(rv.Share()), rv.Reviewed, pct(GateApproved)))
	}
	if e == nil {
		s.Reasons = append(s.Reasons, "no eval recorded yet: run leadhound leads drafts eval --decider claude")
	} else {
		if e.Reviewed < GateMinReviewed {
			s.Reasons = append(s.Reasons, fmt.Sprintf("the last eval covered %d of %d reviewed leads", e.Reviewed, GateMinReviewed))
		}
		if e.FirstTryPass+gateEpsilon < GateFirstTry {
			s.Reasons = append(s.Reasons, fmt.Sprintf("the last eval passed %d%% on the first try (needs %d%%)", pct(e.FirstTryPass), pct(GateFirstTry)))
		}
		if e.AfterRetryPass+gateEpsilon < GateAfterRetry {
			s.Reasons = append(s.Reasons, fmt.Sprintf("the last eval passed %d%% after the retry (needs %d%%)", pct(e.AfterRetryPass), pct(GateAfterRetry)))
		}
	}
	s.Trusted = len(s.Reasons) == 0
	return s
}

// Summary is the gate in one line, as the Campaigns tab and `campaigns check` show it:
// "Claude drafts: not trusted yet: 14 of 20 reviewed, 86% approved", then any eval reason.
func (s GateState) Summary() string {
	stats := fmt.Sprintf("%d of %d reviewed, %d%% approved", s.Reviews.Reviewed, GateLastReviews, pct(s.Reviews.Share()))
	if s.Trusted {
		return fmt.Sprintf("Claude drafts: trusted: %s, last eval %d%% first try, %d%% after retry",
			stats, pct(s.Eval.FirstTryPass), pct(s.Eval.AfterRetryPass))
	}
	var evalWhy []string
	for _, r := range s.Reasons {
		if strings.Contains(r, "eval") {
			evalWhy = append(evalWhy, r)
		}
	}
	out := "Claude drafts: not trusted yet: " + stats
	if len(evalWhy) > 0 {
		out += "; " + strings.Join(evalWhy, "; ")
	}
	return out
}

// pct is a share as a whole percentage, rounded down (so 0.895 never reads as a passing 90%).
func pct(v float64) int {
	return int(math.Floor(v*100 + gateEpsilon))
}
