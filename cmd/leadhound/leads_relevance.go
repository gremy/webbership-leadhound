package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/eval"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["relevance"] = runLeadsRelevance }

// leadhoundModule is this repository's module path: a directory whose go.mod names it is a
// checkout of the repository, where a golden set (private labels) must never be written.
const leadhoundModule = "github.com/gremy/webbership-leadhound"

const (
	leadsRelevanceUsage = "usage: leadhound leads relevance golden-draft --n N --out FILE | eval [--golden FILE] [--runs 3] [--decider fake|jev]"
	goldenDraftUsage    = "usage: leadhound leads relevance golden-draft --n N (at least 1) --out FILE"
	relevanceEvalUsage  = "usage: leadhound leads relevance eval [--golden FILE] [--runs 3] [--decider fake|jev]"
)

// defaultGoldenN is how many leads a golden draft holds without --n.
const defaultGoldenN = 30

// runLeadsRelevance is `leads relevance golden-draft|eval ...`.
func runLeadsRelevance(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsRelevanceUsage)
	}
	switch args[0] {
	case "golden-draft":
		return runGoldenDraft(ctx, args[1:])
	case "eval":
		return runRelevanceEval(ctx, args[1:])
	}
	return usageError("unknown relevance subcommand %q; %s", args[0], leadsRelevanceUsage)
}

// runGoldenDraft is `leads relevance golden-draft --n 30 --out FILE`: it writes n open leads,
// spread over kinds, Drupal majors and countries, as golden lines with a null label for the
// operator to fill in, at mode 0600. It refuses a path inside a checkout of this repository
// (the labels are the operator's private data) and never overwrites a file. Each line's id
// is the lead's domain, so the operator can look the site up; the input is what the
// relevance question reads, with the end-of-life dates of the effective settings.
func runGoldenDraft(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads relevance golden-draft", flag.ContinueOnError)
	n := fs.Int("n", defaultGoldenN, "how many leads the draft holds")
	out := fs.String("out", "", "the file to write (outside the repository; never overwritten)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *n < 1 || *out == "" {
		return usageError(goldenDraftUsage)
	}
	path, err := filepath.Abs(*out)
	if err != nil {
		return usageError("--out %s: %v", *out, err)
	}
	if root, inside := insideRepo(path); inside {
		return usageError("--out %s is inside the leadhound repository at %s; the golden set holds your private labels, so write it outside, e.g. under $LEADHOUND_DATA_DIR/golden/", path, root)
	}
	if _, err := os.Lstat(path); err == nil {
		return usageError("--out %s exists; a golden draft never overwrites a file (it may hold your labels), so move it or choose another name", path)
	}

	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	w, err := weighLeads(ctx, lq, setup)
	if err != nil {
		return err
	}
	pf, err := leadstore.ListProfileJSON(w.profile)
	if err != nil {
		return err
	}
	leads, err := lq.LeadsToRate(ctx, leadstore.ListContext{ProfileJSON: pf})
	if err != nil {
		return err
	}
	picked := spreadSample(leads, *n)
	if len(picked) == 0 {
		_, _ = fmt.Fprintln(stdout, "leads relevance golden-draft: no open leads to draft from; nothing written")
		return nil
	}
	var b strings.Builder
	for _, l := range picked {
		line, err := json.Marshal(leadrate.Golden{ID: l.Domain, Input: rateInput(l, setup.leads), Note: "lead " + strconv.FormatInt(l.LeadID, 10)})
		if err != nil {
			return fmt.Errorf("golden draft: %w", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := writeNew(path, []byte(b.String())); err != nil {
		return err
	}
	kinds, majors, countries := map[string]bool{}, map[int]bool{}, map[string]bool{}
	for _, l := range picked {
		kinds[l.Kind], majors[l.Major], countries[l.Country] = true, true, true
	}
	_, _ = fmt.Fprintf(stdout, "leads relevance golden-draft: %d leads (of %d open) written to %s, mode 0600 · %d kinds · %d majors · %d countries\n",
		len(picked), len(leads), path, len(kinds), len(majors), len(countries))
	_, _ = fmt.Fprintf(stdout, "  set each line's \"label\" to one of %s, then run leads relevance eval --golden %s\n",
		strings.Join(leadrate.Bands(), ", "), path)
	return nil
}

// insideRepo reports whether path lies under a checkout of this repository: a directory,
// above it, whose go.mod names leadhoundModule. Symbolic links on the way are followed, so a
// link that points into a checkout counts as inside it. root is that directory.
func insideRepo(path string) (root string, inside bool) {
	dirs := []string{filepath.Dir(path)}
	// The deepest directory that exists, with its links resolved, and the rest of the path.
	for d := dirs[0]; ; {
		if real, err := filepath.EvalSymlinks(d); err == nil {
			rest, err := filepath.Rel(d, dirs[0])
			if err == nil {
				dirs = append(dirs, filepath.Join(real, rest))
			}
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	for _, start := range dirs {
		for d := start; ; {
			if isLeadhoundModule(filepath.Join(d, "go.mod")) {
				return d, true
			}
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	return "", false
}

// isLeadhoundModule reports whether the go.mod at path declares leadhoundModule.
func isLeadhoundModule(path string) bool {
	f, err := os.Open(path) // #nosec G304 -- a go.mod above the operator's own --out path, read for its module line
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if mod, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(mod), `"`) == leadhoundModule
		}
	}
	return false
}

// writeNew writes b to a new file at path, mode 0600, creating its directory (0700) when
// missing; an existing file is never overwritten.
func writeNew(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("golden draft: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator's own --out path, checked outside the repository
	if err != nil {
		return fmt.Errorf("golden draft: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("golden draft: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("golden draft: %w", err)
	}
	return nil
}

// spreadSample picks at most n of leads spread over kinds, Drupal majors and countries:
// the leads are grouped by the three together, the groups taken largest first (then by
// name), and one lead taken from each group in turn until n are picked. Within a group the
// order is a hash of the lead's domain, so the sample is the same for the same leads and is
// not the top of the queue.
func spreadSample(leads []leadstore.RateLead, n int) []leadstore.RateLead {
	groups := map[string][]leadstore.RateLead{}
	for _, l := range leads {
		key := l.Kind + "\x00" + strconv.Itoa(l.Major) + "\x00" + l.Country
		groups[key] = append(groups[key], l)
	}
	keys := make([]string, 0, len(groups))
	for k, g := range groups {
		slices.SortFunc(g, func(a, b leadstore.RateLead) int {
			ha, hb := sha256.Sum256([]byte(a.Domain)), sha256.Sum256([]byte(b.Domain))
			return cmp.Or(slices.Compare(ha[:], hb[:]), cmp.Compare(a.LeadID, b.LeadID))
		})
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(groups[b]), len(groups[a])), strings.Compare(a, b))
	})
	var out []leadstore.RateLead
	for i := 0; len(out) < n; i++ {
		took := false
		for _, k := range keys {
			if len(out) == n {
				break
			}
			if i < len(groups[k]) {
				out = append(out, groups[k][i])
				took = true
			}
		}
		if !took {
			break
		}
	}
	return out
}

// runRelevanceEval is `leads relevance eval [--golden FILE] [--runs 3] [--decider fake|jev]`:
// it asks the relevance question about every labelled lead runs times and prints how the
// answers measure against the labels (leadrate.Evaluate). The labels are the operator's
// calls in the database (lead_label: the latest call per lead, on the facts he saw), or
// with --golden a golden file instead (the demo set, tests). The decider is the fake
// unless --decider jev is given; a jev eval needs TYPESAFE_API_KEY and LEADHOUND_LIVE=1, is
// uncapped (eval.MaxRuns bounds it), and announces its calls before the first one. Only a
// jev eval on at least leadrate.TrustMinN labelled leads is recorded in lead_relevance_eval,
// where the list and the queue read the trust gate from.
func runRelevanceEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads relevance eval", flag.ContinueOnError)
	golden := fs.String("golden", "", "a golden file, one JSON lead per line (default: your calls in the database)")
	runs := fs.Int("runs", 3, "how many times to ask about each labelled lead")
	deciderName := fs.String("decider", "", "fake or jev (default fake; jev is live and paid)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError(relevanceEvalUsage)
	}
	if *runs < 1 || *runs > eval.MaxRuns {
		return usageError("--runs must be 1 to %d", eval.MaxRuns)
	}
	var lines []leadrate.Golden
	if *golden != "" {
		if lines, err = leadrate.LoadGolden(*golden); err != nil {
			return usageError("%v", err)
		}
	}
	name := cmp.Or(*deciderName, rateDeciderFake)
	var required []string
	if name == rateDeciderJev || *golden == "" {
		required = append(required, "LEADHOUND_DATABASE_URL") // where the calls are, and a measured eval is recorded
	}
	cfg, err := loadConfig(required...)
	if err != nil {
		return err
	}
	if *golden == "" {
		if lines, err = dbGolden(ctx, cfg); err != nil {
			return err
		}
		if len(lines) == 0 {
			return usageError("%s", noCallsWords)
		}
	}
	if name == rateDeciderJev && cfg.Live != config.LiveAll {
		_, _ = fmt.Fprintf(stdout, "leads relevance eval: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	decider, kind, err := relevanceDecider(cfg, name, "eval")
	if err != nil {
		return err
	}
	labelled := 0
	for _, g := range lines {
		if g.Label != nil {
			labelled++
		}
	}
	announceRelevanceEval(decider, labelled, *runs)
	rep, err := leadrate.Evaluate(ctx, decider, lines, *runs)
	if errors.Is(err, leadrate.ErrNoLabels) && *golden == "" {
		return usageError("%s", noCallsWords)
	}
	if errors.Is(err, leadrate.ErrNoLabels) {
		return usageError("%s has no labelled line: set each \"label\" to one of %s", *golden, strings.Join(leadrate.Bands(), ", "))
	}
	if err != nil {
		return err
	}
	if err := rep.Write(stdout); err != nil {
		return err
	}
	if why := evalNotRecorded(kind, rep.N); why != "" {
		_, _ = fmt.Fprintf(stdout, "not recorded: %s\n", why)
		return nil
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	id, err := leadstore.NewQueries(s.Pool()).RecordRelevanceEval(context.WithoutCancel(ctx), leadstore.RelevanceEval{
		Question: leadrate.Question, Model: decider.Name(), N: rep.N, Exact: rep.Exact, WithinOne: rep.WithinOne,
		MAE: rep.MAE, Spearman: rep.Spearman, Runs: rep.Runs, Trusted: rep.Trusted(),
	})
	if err != nil {
		return err
	}
	verdict := "the trust gate is not passed, so the list and the queue keep sorting by points"
	if rep.Trusted() {
		verdict = "the trust gate is passed, so the list and the queue sort leads that are not starred by relevance"
	}
	_, _ = fmt.Fprintf(stdout, "recorded as eval %d: %s\n", id, verdict)
	return nil
}

// noCallsWords is the refusal of an eval from the database when no lead has a call.
const noCallsWords = "No lead has your call yet: open Leads, choose To label, and give each lead your call on its page."

// dbGolden is the operator's calls in the database as golden lines: the latest call on each
// lead that names a band, on the facts he saw when he made it.
func dbGolden(ctx context.Context, cfg config.Config) ([]leadrate.Golden, error) {
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return nil, err
	}
	defer s.Close()
	labels, err := leadstore.NewQueries(s.Pool()).LabelledLeads(ctx)
	if err != nil {
		return nil, err
	}
	return leadrate.GoldenFromLabels(labels)
}

// evalNotRecorded says why an eval of kind on n labelled leads is not recorded; "" when it
// is: only a jev eval on at least leadrate.TrustMinN labelled leads measures the number the
// list may sort by.
func evalNotRecorded(kind string, n int) string {
	switch {
	case kind != rateDeciderJev:
		return "only an eval with --decider jev measures the number the list shows"
	case n < leadrate.TrustMinN:
		return fmt.Sprintf("%d labelled leads; an eval is recorded from %d", n, leadrate.TrustMinN)
	}
	return ""
}

// announceRelevanceEval prints, before a live eval's first call, how many calls it will make,
// as announceEval does for the jobs eval.
func announceRelevanceEval(d ai.Decider, labelled, runs int) {
	if _, ok := d.(*ai.Jev); !ok {
		return
	}
	_, _ = fmt.Fprintf(notes, "leads relevance eval: %s will make %d calls (%d labelled leads × %d runs), uncapped\n",
		d.Name(), eval.PlannedCalls(labelled, runs), labelled, runs)
}
