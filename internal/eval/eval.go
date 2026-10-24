// Package eval measures the decider's judgement against a hand-labelled golden set.
//
// Each golden job is a raw detail page and the labels a person gave it. Eval parses the
// page, cleans it and runs the gates exactly as the pipeline does, then asks the decider
// on every job, whatever the gates said: it measures judgement, not the pipeline's
// spending. It runs the set several times, because a model's answer can swing between
// runs, and reports agreement per criterion and per run. It writes nothing anywhere, and
// it names jobs by their golden id (the page's file stem), never by title or company.
package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/scoring"
)

// GoldenFiles are the golden set's names inside its directory, tried in order: the
// private set, then the repository's fictional demo.
var GoldenFiles = []string{"jobs.jsonl", "golden.demo.jsonl"}

// CostPerMillionInput is the estimated price of a million input tokens, in US dollars.
const CostPerMillionInput = 0.042

// MaxRuns is the most runs one eval makes: a live eval is not capped, so its size is.
const MaxRuns = 5

// PlannedCalls is how many decider calls an eval of jobs golden jobs makes over runs runs:
// every job is asked on every run, whatever its gates said. A request too long for the
// model adds one retry on top.
func PlannedCalls(jobs, runs int) int { return jobs * runs }

// ErrNoGolden means the directory holds no golden set, or an empty one.
var ErrNoGolden = errors.New("no golden set")

// GateLabels are the gates a golden job may be labelled on apart from the criteria.
// Location and work rights are both a gate and a criterion; their labels are the
// criterion's.
var GateLabels = []string{gates.GateLanguage, gates.GateWorkplace}

// Job is one golden line. A nil label is not yet labelled: it is skipped and counted.
type Job struct {
	ID      string             `json:"id"`
	Raw     string             `json:"raw"` // relative to the golden directory
	Labels  map[string]*string `json:"labels"`
	Overall *string            `json:"overall"`
	Note    string             `json:"note"`
}

// Load reads the golden set in dir and checks every line: a known label with a value
// its kind allows, a unique id and a raw page inside dir.
func Load(dir string) ([]Job, string, error) {
	var path string
	for _, name := range GoldenFiles {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		return nil, "", fmt.Errorf("eval: %s: %w: want %s with one labelled job per line",
			dir, ErrNoGolden, strings.Join(GoldenFiles, " or "))
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own golden directory
	if err != nil {
		return nil, "", fmt.Errorf("eval: %w", err)
	}
	var jobs []Job
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var j Job
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&j); err != nil {
			return nil, "", fmt.Errorf("eval: %s line %d: %w", filepath.Base(path), n, err)
		}
		if err := j.check(); err != nil {
			return nil, "", fmt.Errorf("eval: %s line %d: %w", filepath.Base(path), n, err)
		}
		if seen[j.ID] {
			return nil, "", fmt.Errorf("eval: %s line %d: id %q appears twice", filepath.Base(path), n, j.ID)
		}
		seen[j.ID] = true
		jobs = append(jobs, j)
	}
	if err := sc.Err(); err != nil {
		return nil, "", fmt.Errorf("eval: %s: %w", filepath.Base(path), err)
	}
	if len(jobs) == 0 {
		return nil, "", fmt.Errorf("eval: %s: %w: the file has no jobs", path, ErrNoGolden)
	}
	return jobs, filepath.Base(path), nil
}

var (
	criterionValues = []string{scoring.Pass, scoring.Fail, scoring.Unclear}
	gateValues      = []string{gates.Pass, gates.Fail, gates.Flag}
	overallValues   = []string{scoring.TierApply, scoring.TierYourCall, scoring.TierSkip}
)

func (j Job) check() error {
	if j.ID == "" || strings.ContainsAny(j.ID, "/\\") {
		return fmt.Errorf("id %q must be a plain file stem", j.ID)
	}
	if j.Raw == "" || filepath.IsAbs(j.Raw) || !filepath.IsLocal(j.Raw) {
		return fmt.Errorf("job %s: raw %q must be a path inside the golden directory", j.ID, j.Raw)
	}
	for name, v := range j.Labels {
		var allowed []string
		switch {
		case slices.Contains(candidate.CriterionNames, name):
			allowed = criterionValues
		case slices.Contains(GateLabels, name):
			allowed = gateValues
		default:
			return fmt.Errorf("job %s: unknown label %q (want a criterion or %s)", j.ID, name, strings.Join(GateLabels, ", "))
		}
		if v != nil && !slices.Contains(allowed, *v) {
			return fmt.Errorf("job %s: label %s is %q, want one of %s or null", j.ID, name, *v, strings.Join(allowed, ", "))
		}
	}
	if j.Overall != nil && !slices.Contains(overallValues, *j.Overall) {
		return fmt.Errorf("job %s: overall is %q, want one of %s or null", j.ID, *j.Overall, strings.Join(overallValues, ", "))
	}
	return nil
}

// Options configure a Run.
type Options struct {
	Runs    int
	Decider ai.Decider // never wrapped in ai.Capped: eval writes nothing, usage included; MaxRuns bounds it
	Profile candidate.Config
	Now     time.Time // the fetch time the parser estimates posting dates from
}

// Report is everything one eval measured.
type Report struct {
	GoldenFile string
	Decider    string
	Runs       int
	Jobs       []JobResult
	Tokens     int
}

// JobResult is one golden job's labels and what each run said.
type JobResult struct {
	Job
	Gates map[string]string   // gate → verdict (gates are deterministic: one per job)
	Runs  []map[string]string // per run: criterion → verdict, plus "overall"
}

// Run evaluates every job Runs times.
func Run(ctx context.Context, dir string, file string, jobs []Job, opt Options) (Report, error) {
	if opt.Runs < 1 || opt.Runs > MaxRuns {
		return Report{}, fmt.Errorf("eval: runs must be 1 to %d", MaxRuns)
	}
	rep := Report{GoldenFile: file, Decider: opt.Decider.Name(), Runs: opt.Runs}
	criteria := opt.Profile.Candidate.Criteria.List()
	qs := scoring.Questions(criteria)
	for _, j := range jobs {
		p, gs, err := prepare(dir, j, opt)
		if err != nil {
			return Report{}, err
		}
		jr := JobResult{Job: j, Gates: map[string]string{}}
		var gv []scoring.GateVerdict
		for _, g := range gs {
			jr.Gates[g.Gate] = g.Verdict // eval applies no stored rules, so each gate gives one result
			gv = append(gv, scoring.GateVerdict{Gate: g.Gate, Verdict: g.Verdict})
		}
		for range opt.Runs {
			answers, tokens, err := decide(ctx, opt.Decider, p, qs)
			if err != nil {
				return Report{}, fmt.Errorf("eval: job %s: %w", j.ID, err)
			}
			rep.Tokens += tokens
			verdicts := map[string]string{}
			var cv []scoring.CriterionVerdict
			for _, c := range criteria {
				v := scoring.Verdict(answers[c.Name], opt.Profile.ConfidenceThreshold)
				verdicts[c.Name] = v
				cv = append(cv, scoring.CriterionVerdict{Criterion: c.Name, Verdict: v})
			}
			verdicts["overall"] = scoring.TierOf(gv, cv)
			jr.Runs = append(jr.Runs, verdicts)
		}
		rep.Jobs = append(rep.Jobs, jr)
	}
	return rep, nil
}

// prepare reads, parses and cleans the job's page and runs the gates over it, as the
// pipeline does (stored rules aside: eval reads no database).
func prepare(dir string, j Job, opt Options) (scoring.Posting, []gates.Result, error) {
	b, err := os.ReadFile(filepath.Join(dir, j.Raw)) // #nosec G304 -- a checked path inside the golden directory
	if err != nil {
		return scoring.Posting{}, nil, fmt.Errorf("eval: job %s: %w", j.ID, err)
	}
	d, err := linkedin.ParseDetail(b, opt.Now)
	if err != nil {
		return scoring.Posting{}, nil, fmt.Errorf("eval: job %s: %w", j.ID, err)
	}
	title, company, location := postingguard.CleanField(d.Title), postingguard.CleanField(d.Company), postingguard.CleanField(d.Location)
	desc := postingguard.Clean(d.Description, true).Visible
	langText := desc
	if strings.TrimSpace(langText) == "" {
		langText = title
	}
	lang, conf := gates.DetectLanguage(langText)
	// Every golden row is a LinkedIn detail page (parsed above), and LinkedIn's search lists
	// remote jobs only, as the collector tells the gates.
	gs := gates.Evaluate(gates.Observation{Title: title, Company: company, Location: location, Description: desc,
		Lang: lang, LangConfidence: conf, ListedRemote: true}, opt.Profile.Gates(), nil)
	return scoring.Posting{Title: title, Company: company, Location: location, Seniority: d.Seniority,
		EmploymentType: d.EmploymentType, Description: desc}, gs, nil
}

// decide asks once, retrying once with the shorter description when the request is too
// long, as the scorer does.
func decide(ctx context.Context, d ai.Decider, p scoring.Posting, qs []ai.Question) (map[string]ai.Answer, int, error) {
	answers, u, err := d.Decide(ctx, scoring.State(p, scoring.MaxDescription), qs)
	tokens := u.InputTokens
	if errors.Is(err, ai.ErrTooLong) {
		answers, u, err = d.Decide(ctx, scoring.State(p, scoring.RetryDescription), qs)
		tokens += u.InputTokens
	}
	return answers, tokens, err
}
