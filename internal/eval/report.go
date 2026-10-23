package eval

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/gremy/webbership-leadhound/internal/candidate"
)

// tally is one table cell: how many labelled jobs matched.
type tally struct{ match, labelled int }

func (t tally) String() string {
	if t.labelled == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d %3.0f%%", t.match, t.labelled, 100*float64(t.match)/float64(t.labelled))
}

// row is one table line: a tally per run and how many jobs had no label.
type row struct {
	name  string
	runs  []tally
	nulls int
}

// rows builds the table: each criterion, each gate label, the overall tier, then every
// criterion label together.
func (r Report) rows() []row {
	var out []row
	all := row{name: "all criteria", runs: make([]tally, r.Runs)}
	for _, c := range candidate.CriterionNames {
		rw := row{name: c, runs: make([]tally, r.Runs)}
		for _, j := range r.Jobs {
			label := j.Labels[c]
			if label == nil {
				rw.nulls++
				all.nulls++
				continue
			}
			for i, run := range j.Runs {
				m := b2i(run[c] == *label)
				rw.runs[i].labelled++
				rw.runs[i].match += m
				all.runs[i].labelled++
				all.runs[i].match += m
			}
		}
		out = append(out, rw)
	}
	for _, g := range GateLabels {
		rw := row{name: "gate " + g, runs: make([]tally, r.Runs)}
		for _, j := range r.Jobs {
			label := j.Labels[g]
			if label == nil {
				rw.nulls++
				continue
			}
			for i := range r.Runs {
				rw.runs[i].labelled++
				rw.runs[i].match += b2i(j.Gates[g] == *label)
			}
		}
		out = append(out, rw)
	}
	ov := row{name: "overall", runs: make([]tally, r.Runs)}
	for _, j := range r.Jobs {
		if j.Overall == nil {
			ov.nulls++
			continue
		}
		for i, run := range j.Runs {
			ov.runs[i].labelled++
			ov.runs[i].match += b2i(run["overall"] == *j.Overall)
		}
	}
	return append(out, ov, all)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// disagreements lists, per job and criterion (and the overall tier), the verdicts of runs
// that did not all agree.
func (r Report) disagreements() []string {
	var out []string
	names := append(append([]string{}, candidate.CriterionNames...), "overall")
	for _, j := range r.Jobs {
		for _, n := range names {
			vs := make([]string, len(j.Runs))
			differ := false
			for i, run := range j.Runs {
				vs[i] = run[n]
				differ = differ || vs[i] != vs[0]
			}
			if differ {
				out = append(out, fmt.Sprintf("%s %s: %s", j.ID, n, strings.Join(vs, " / ")))
			}
		}
	}
	return out
}

// Write prints the report as a plain table. It names jobs by id only.
func (r Report) Write(w io.Writer) error {
	var b strings.Builder
	fmt.Fprintf(&b, "eval: %d golden jobs from %s · decider %s · %d run(s)\n", len(r.Jobs), r.GoldenFile, r.Decider, r.Runs)
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	head := []string{"label"}
	for i := range r.Runs {
		head = append(head, fmt.Sprintf("run %d", i+1))
	}
	head = append(head, "null (skipped)")
	_, _ = fmt.Fprintln(tw, strings.Join(head, "\t")) // a strings.Builder never fails; Flush reports for tw
	for _, rw := range r.rows() {
		cells := []string{rw.name}
		for _, t := range rw.runs {
			cells = append(cells, t.String())
		}
		cells = append(cells, fmt.Sprint(rw.nulls))
		_, _ = fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	switch d := r.disagreements(); {
	case r.Runs < 2:
		b.WriteString("disagreements between runs: one run only; rerun with --runs 3 before believing a swing\n")
	case len(d) == 0:
		b.WriteString("disagreements between runs: none\n")
	default:
		fmt.Fprintf(&b, "disagreements between runs: %d\n", len(d))
		for _, l := range d {
			b.WriteString("  " + l + "\n")
		}
	}
	fmt.Fprintf(&b, "input tokens: %d · estimated cost: $%.6f (at $%.3f per million input tokens)\n",
		r.Tokens, float64(r.Tokens)*CostPerMillionInput/1e6, CostPerMillionInput)
	_, err := io.WriteString(w, b.String())
	return err
}
