package leadrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/eval"
)

// The trust gate (D9): the number may order the list only when the latest eval had at
// least TrustMinN labelled leads, within-one-band at least TrustMinWithinOne and
// Spearman's rho at least TrustMinSpearman.
const (
	TrustMinN         = 30
	TrustMinWithinOne = 0.8
	TrustMinSpearman  = 0.5
)

// ErrNoLabels means the golden set has no labelled line, so there is nothing to measure.
var ErrNoLabels = errors.New("leadrate: no labelled golden lines")

// Report is everything one eval measured. Shares are 0..1 and MAE is in points (0..100),
// as lead_relevance_eval stores them.
type Report struct {
	Decider       string
	Runs          int
	N             int // labelled leads, the ones measured
	Unlabelled    int // lines with a null label: skipped, never asked
	Exact         float64
	WithinOne     float64
	MAE           float64   // mean absolute error of the score against the label's weight
	Spearman      float64   // the mean over the runs
	SpearmanRuns  []float64 // one per run
	Disagreements []string  // "<id>: likely / maybe / likely", one per lead whose runs differ
	Tokens        int       // input tokens over every call, a billed bad answer included
	Cost          float64   // estimated US dollars at eval.CostPerMillionInput
	Results       []Result
}

// Result is one labelled lead and what each run said.
type Result struct {
	ID     string
	Label  string
	Scores []int    // per run
	Bands  []string // per run
}

// Evaluate asks the decider about every labelled golden lead runs times (1 to
// eval.MaxRuns) and measures the answers against the labels. Unlabelled lines are skipped
// and never asked. It writes nothing anywhere and names leads by golden id only. A decider
// error stops the eval; the report returned with it counts the tokens spent so far.
//
// The decider is not wrapped in a cap here: eval.MaxRuns bounds the calls, and the caller
// announces them first.
func Evaluate(ctx context.Context, d ai.Decider, golden []Golden, runs int) (Report, error) {
	if runs < 1 || runs > eval.MaxRuns {
		return Report{}, fmt.Errorf("leadrate: eval: runs must be 1 to %d", eval.MaxRuns)
	}
	if d == nil {
		return Report{}, errors.New("leadrate: eval: no decider")
	}
	rep := Report{Decider: d.Name(), Runs: runs}
	qs := []ai.Question{AIQuestion()}
	for _, g := range golden {
		if g.Label == nil {
			rep.Unlabelled++
			continue
		}
		if _, ok := Weight(*g.Label); !ok {
			return rep, fmt.Errorf("leadrate: eval: lead %s: label %q is not a band", g.ID, *g.Label)
		}
		state := State(g.Input)
		r := Result{ID: g.ID, Label: *g.Label}
		for range runs {
			answers, u, err := d.Decide(ctx, state, qs)
			rep.Tokens += u.InputTokens
			if err != nil {
				rep.Cost = cost(rep.Tokens)
				return rep, fmt.Errorf("leadrate: eval: lead %s: %w", g.ID, err)
			}
			s, b := Score(answers[questionName])
			r.Scores = append(r.Scores, s)
			r.Bands = append(r.Bands, b)
		}
		rep.Results = append(rep.Results, r)
	}
	rep.N = len(rep.Results)
	rep.Cost = cost(rep.Tokens)
	if rep.N == 0 {
		return rep, ErrNoLabels
	}
	rep.measure()
	return rep, nil
}

func cost(tokens int) float64 { return float64(tokens) * eval.CostPerMillionInput / 1e6 }

// measure fills the metrics from Results: exact, within-one and MAE pooled over every run
// (each run has the same leads, so this is the mean over runs), and rho per run.
func (r *Report) measure() {
	var exact, within int
	var absErr float64
	labels := make([]float64, r.N)
	for i, res := range r.Results {
		lw, _ := Weight(res.Label)
		labels[i] = float64(lw)
		li := bandIndex(res.Label)
		for run := range r.Runs {
			bi := bandIndex(res.Bands[run])
			if bi == li {
				exact++
			}
			if bi >= 0 && abs(bi-li) <= 1 {
				within++
			}
			absErr += math.Abs(float64(res.Scores[run] - lw))
		}
		if slices.ContainsFunc(res.Bands, func(b string) bool { return b != res.Bands[0] }) {
			r.Disagreements = append(r.Disagreements, res.ID+": "+strings.Join(res.Bands, " / "))
		}
	}
	asked := float64(r.N * r.Runs)
	r.Exact = float64(exact) / asked
	r.WithinOne = float64(within) / asked
	r.MAE = absErr / asked
	r.SpearmanRuns = make([]float64, r.Runs)
	var sum float64
	for run := range r.Runs {
		scores := make([]float64, r.N)
		for i, res := range r.Results {
			scores[i] = float64(res.Scores[run])
		}
		r.SpearmanRuns[run] = Spearman(labels, scores)
		sum += r.SpearmanRuns[run]
	}
	r.Spearman = sum / float64(r.Runs)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Trusted reports whether the eval passes D9's trust gate.
func (r Report) Trusted() bool { return len(r.untrusted()) == 0 }

// untrusted lists why the eval fails the trust gate, in plain words; none when it passes.
func (r Report) untrusted() []string {
	var why []string
	if r.N < TrustMinN {
		why = append(why, fmt.Sprintf("%d labelled leads, need %d", r.N, TrustMinN))
	}
	if r.WithinOne < TrustMinWithinOne {
		why = append(why, fmt.Sprintf("within one band %.0f%%, need %.0f%%", 100*r.WithinOne, 100*TrustMinWithinOne))
	}
	if r.Spearman < TrustMinSpearman {
		why = append(why, fmt.Sprintf("Spearman rho %.2f, need %.2f", r.Spearman, TrustMinSpearman))
	}
	return why
}

// Write prints the report in plain lines. It names leads by golden id only.
func (r Report) Write(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "relevance eval: %d labelled lead(s), %d unlabelled skipped · question %s · decider %s · %d run(s)\n",
		r.N, r.Unlabelled, Question, r.Decider, r.Runs)
	runs := make([]string, len(r.SpearmanRuns))
	for i, v := range r.SpearmanRuns {
		runs[i] = fmt.Sprintf("%.2f", v)
	}
	fmt.Fprintf(&b, "exact band: %.0f%% · within one band: %.0f%% · mean absolute error: %.1f points · Spearman rho: %.2f (per run: %s)\n",
		100*r.Exact, 100*r.WithinOne, r.MAE, r.Spearman, strings.Join(runs, ", "))
	if why := r.untrusted(); len(why) == 0 {
		b.WriteString("trust gate: passed, so the list may sort by relevance\n")
	} else {
		fmt.Fprintf(&b, "trust gate: not passed (%s), so the number stays an early number\n", strings.Join(why, "; "))
	}
	switch {
	case r.Runs < 2:
		b.WriteString("disagreements between runs: one run only; rerun with --runs 3 before believing a swing\n")
	case len(r.Disagreements) == 0:
		b.WriteString("disagreements between runs: none\n")
	default:
		fmt.Fprintf(&b, "disagreements between runs: %d\n", len(r.Disagreements))
		for _, l := range r.Disagreements {
			b.WriteString("  " + l + "\n")
		}
	}
	fmt.Fprintf(&b, "input tokens: %d · estimated cost: $%.6f (at $%.3f per million input tokens)\n",
		r.Tokens, r.Cost, eval.CostPerMillionInput)
	_, err := io.WriteString(w, b.String())
	return err
}

// Spearman is Spearman's rank correlation of x and y: both are ranked, ties taking the
// average of the ranks they span, and the result is Pearson's correlation of the ranks.
// It is 0 when it is undefined (fewer than two pairs, unequal lengths, or either list all
// one value), which never passes the trust gate and is a number the store accepts.
func Spearman(x, y []float64) float64 {
	if len(x) != len(y) || len(x) < 2 {
		return 0
	}
	return pearson(ranks(x), ranks(y))
}

// ranks gives each value its 1-based rank in ascending order; tied values share the
// average of the ranks they span.
func ranks(v []float64) []float64 {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		switch {
		case v[a] < v[b]:
			return -1
		case v[a] > v[b]:
			return 1
		}
		return 0
	})
	out := make([]float64, len(v))
	for i := 0; i < len(idx); {
		j := i
		for j+1 < len(idx) && v[idx[j+1]] == v[idx[i]] {
			j++
		}
		avg := float64(i+j)/2 + 1 // ranks i+1 .. j+1
		for k := i; k <= j; k++ {
			out[idx[k]] = avg
		}
		i = j + 1
	}
	return out
}

// pearson is the correlation of x and y, or 0 when either has no spread.
func pearson(x, y []float64) float64 {
	n := float64(len(x))
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= n
	my /= n
	var cov, vx, vy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx == 0 || vy == 0 {
		return 0
	}
	rho := cov / math.Sqrt(vx*vy)
	return min(max(rho, -1), 1)
}
