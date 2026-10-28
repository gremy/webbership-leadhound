package outreach

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestGateThresholds walks each threshold just below and exactly at its value: at passes,
// below closes the gate with a reason naming that threshold only.
func TestGateThresholds(t *testing.T) {
	good := GateEval{Reviewed: 20, FirstTryPass: 0.90, AfterRetryPass: 0.98}
	ok := GateReviews{Reviewed: 20, Approved: 16} // exactly 80%
	for _, tc := range []struct {
		name    string
		eval    *GateEval
		rv      GateReviews
		trusted bool
		reason  string
	}{
		{"all at threshold", &good, ok, true, ""},
		{"eval 19 reviewed leads", &GateEval{Reviewed: 19, FirstTryPass: 0.9, AfterRetryPass: 0.98}, ok, false, "19 of 20 reviewed leads"},
		{"first try 89%", &GateEval{Reviewed: 20, FirstTryPass: 0.89, AfterRetryPass: 0.98}, ok, false, "89% on the first try"},
		{"first try 18/20", &GateEval{Reviewed: 20, FirstTryPass: 18.0 / 20, AfterRetryPass: 0.98}, ok, true, ""},
		{"after retry 97%", &GateEval{Reviewed: 20, FirstTryPass: 0.9, AfterRetryPass: 0.97}, ok, false, "97% after the retry"},
		{"after retry 49/50", &GateEval{Reviewed: 20, FirstTryPass: 0.9, AfterRetryPass: 49.0 / 50}, ok, true, ""},
		{"approved 15 of 20", &good, GateReviews{Reviewed: 20, Approved: 15}, false, "75% of the last 20 reviewed drafts approved"},
		{"19 reviews", &good, GateReviews{Reviewed: 19, Approved: 19}, false, "19 of 20 drafts reviewed"},
		{"no eval", nil, ok, false, "no eval recorded yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := JudgeGate(tc.eval, tc.rv)
			if s.Trusted != tc.trusted {
				t.Fatalf("trusted %v, want %v (reasons %q)", s.Trusted, tc.trusted, s.Reasons)
			}
			if tc.trusted {
				if len(s.Reasons) != 0 {
					t.Fatalf("trusted with reasons %q", s.Reasons)
				}
				return
			}
			if len(s.Reasons) != 1 || !strings.Contains(s.Reasons[0], tc.reason) {
				t.Fatalf("reasons %q, want exactly one naming %q", s.Reasons, tc.reason)
			}
		})
	}
}

func TestGateSummary(t *testing.T) {
	s := JudgeGate(&GateEval{Reviewed: 20, FirstTryPass: 0.95, AfterRetryPass: 1}, GateReviews{Reviewed: 14, Approved: 12})
	if got, want := s.Summary(), "Claude drafts: not trusted yet: 14 of 20 reviewed, 85% approved"; got != want {
		t.Fatalf("summary %q, want %q", got, want)
	}
	s = JudgeGate(nil, GateReviews{})
	if got := s.Summary(); !strings.HasPrefix(got, "Claude drafts: not trusted yet: 0 of 20 reviewed, 0% approved; no eval recorded yet") {
		t.Fatalf("summary %q", got)
	}
	s = JudgeGate(&GateEval{Reviewed: 25, FirstTryPass: 0.95, AfterRetryPass: 1}, GateReviews{Reviewed: 20, Approved: 18})
	if got := s.Summary(); got != "Claude drafts: trusted: 20 of 20 reviewed, 90% approved, last eval 95% first try, 100% after retry" {
		t.Fatalf("summary %q", got)
	}
}

type gateSrc struct {
	e   GateEval
	ok  bool
	rv  GateReviews
	err error
	n   int
}

func (g *gateSrc) LatestDraftEval(context.Context, int64) (GateEval, bool, error) {
	return g.e, g.ok, g.err
}
func (g *gateSrc) LastDraftReviews(_ context.Context, _ int64, n int) (GateReviews, error) {
	g.n = n
	return g.rv, nil
}

func TestGateReadsSource(t *testing.T) {
	src := &gateSrc{e: GateEval{Reviewed: 20, FirstTryPass: 1, AfterRetryPass: 1}, ok: true, rv: GateReviews{Reviewed: 20, Approved: 20}}
	s, err := Gate(context.Background(), src, 3)
	if err != nil || !s.Trusted || src.n != GateLastReviews || s.Eval == nil {
		t.Fatalf("state %+v err %v n %d", s, err, src.n)
	}
	src.err = errors.New("boom")
	if _, err := Gate(context.Background(), src, 3); err == nil {
		t.Fatal("a store error must surface")
	}
}
