package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["drafts"] = runLeadsDrafts }

const (
	leadsDraftsUsage  = "usage: leadhound leads drafts sample --campaign S --n N [--decider claude|fake] | eval [--campaign S] [--runs N] [--decider fake|claude] | golden-export --out FILE [--campaign S]"
	draftsSampleUsage = "usage: leadhound leads drafts sample --campaign S --n N (at least 1) [--decider claude|fake]"
	draftsEvalUsage   = "usage: leadhound leads drafts eval [--campaign S] [--runs N (at least 1)] [--decider fake|claude]"
	draftsGoldenUsage = "usage: leadhound leads drafts golden-export --out FILE [--campaign S] (FILE outside the repository, never overwritten)"
)

// Deciders a drafts command takes.
const (
	deciderClaude = leadstore.DeciderClaude
	deciderFake   = leadstore.DeciderFake
)

// defaultEvalRuns is how many times an eval regenerates each reviewed lead without --runs.
const defaultEvalRuns = 1

// runLeadsDrafts is `leads drafts sample|eval|golden-export`.
func runLeadsDrafts(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsDraftsUsage)
	}
	switch args[0] {
	case "sample":
		return runDraftsSample(ctx, args[1:])
	case "eval":
		return runDraftsEval(ctx, args[1:])
	case "golden-export":
		return runDraftsGolden(ctx, args[1:])
	}
	return usageError("unknown drafts subcommand %q; %s", args[0], leadsDraftsUsage)
}

// draftsEnv is what every drafts command reads its campaign with.
type draftsEnv struct {
	s     *store.Store
	setup leadsSetup
	lq    leadstore.Queries
	camp  leadstore.Campaign
	plan  batchPlan
	bs    batchSetup
	loc   *time.Location
}

// openDrafts opens the store and campaign slug's batch plan (its registered spec, templates
// and rules). The caller closes env.s.
func openDrafts(ctx context.Context, slug string) (draftsEnv, error) {
	s, setup, err := leadsOpen(ctx, leadsForOutreach)
	if err != nil {
		return draftsEnv{}, err
	}
	env := draftsEnv{s: s, setup: setup, lq: leadstore.NewQueries(s.Pool())}
	fail := func(err error) (draftsEnv, error) { s.Close(); return draftsEnv{}, err }
	if env.camp, err = activeCampaign(ctx, env.lq, slug); err != nil {
		return fail(err)
	}
	if env.loc, err = leadsZone(); err != nil {
		return fail(err)
	}
	env.bs = newBatchSetup(setup, env.loc)
	if env.plan, err = resolveBatchPlan(ctx, env.lq, env.bs, env.camp.ID); err != nil {
		return fail(err)
	}
	return env, nil
}

// draftsCaller is the caller a drafts command writes with: the fake decider's, or the capped
// Claude caller for purpose (nil when live calls are off: the command then stops, exit 0).
func draftsCaller(cmd, decider string, q store.Queries, purpose string) (outreach.Caller, error) {
	if decider == deciderFake {
		return draftFake{}, nil
	}
	return newDraftCaller(cmd, q, purpose)
}

func parseDecider(v string) (string, error) {
	if v != deciderClaude && v != deciderFake {
		return "", usageError("--decider %q is not claude or fake", v)
	}
	return v, nil
}

// worstCase is the most writing ins can cost: per lead the first try and the retry, the
// retry's prompt grown by the previous answer.
func worstCase(ins []outreach.WriteInput) (int64, error) {
	var total int64
	for _, in := range ins {
		req := claude.WriteRequest{Model: claude.ModelSonnet, System: outreach.SystemPrompt(in), Prompt: outreach.UserTurn(in),
			Schema: outreach.WriterSchema(in.Rules), MaxTokens: outreach.DefaultWriterMaxTokens}
		first, err := claude.WorstWrite(req)
		if err != nil {
			return 0, err
		}
		req.Prompt += strings.Repeat(" ", 4*req.MaxTokens)
		retry, err := claude.WorstWrite(req)
		if err != nil {
			return 0, err
		}
		total += first + retry
	}
	return total, nil
}

func usdWords(micros int64) string { return fmt.Sprintf("$%.4f", float64(micros)/1e6) }

// printEstimate prints what writing ins may cost before anything is called: the worst case
// and today's spend against the purpose's caps.
func printEstimate(ctx context.Context, env draftsEnv, decider, purpose string, ins []outreach.WriteInput) error {
	if decider == deciderFake {
		_, _ = fmt.Fprintf(stdout, "Estimate: %s with the fake decider: no Claude call, no cost.\n", plural(len(ins), "lead", "leads"))
		return nil
	}
	worst, err := worstCase(ins)
	if err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	caps, err := cfg.ClaudeCapsFor(purpose, 0, 0)
	if err != nil {
		return err
	}
	spent, err := env.s.Q().ClaudeSpendOn(ctx, leadstore.DefaultAccountID, purpose, startOfDay(leadsNow(), env.loc))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Estimate: %s, at most %d calls, worst case %s (most cost far less); %s spent today %s of %s, at most %s a lead.\n",
		plural(len(ins), "lead", "leads"), 2*len(ins), usdWords(worst), purpose, usdWords(spent), usdWords(caps.DailyMicros), usdWords(caps.LeadMicros))
	return nil
}

// --- sample ---

func runDraftsSample(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads drafts sample", flag.ContinueOnError)
	slug := fs.String("campaign", "", "the campaign's short name")
	n := fs.Int("n", 0, "how many leads to draft for review")
	dec := fs.String("decider", deciderClaude, "claude (LEADHOUND_LIVE=1 and the key) or fake")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *slug == "" || *n < 1 {
		return usageError(draftsSampleUsage)
	}
	decider, err := parseDecider(*dec)
	if err != nil {
		return err
	}
	env, err := openDrafts(ctx, *slug)
	if err != nil {
		return err
	}
	defer env.s.Close()
	jobs, err := sampleJobs(ctx, env, *n)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		_, _ = fmt.Fprintf(stdout, "leads drafts sample: no lead of %s with an email route is left to sample; nothing drafted.\n", *slug)
		return nil
	}
	ins := make([]outreach.WriteInput, len(jobs))
	for i, j := range jobs {
		ins[i] = j.in
	}
	if err := printEstimate(ctx, env, decider, claude.PurposeDrafting, ins); err != nil {
		return err
	}
	caller, err := draftsCaller("leads drafts sample", decider, env.s.Q(), claude.PurposeDrafting)
	if err != nil || caller == nil {
		return err
	}
	res, err := sampleDrafts(ctx, env.lq, &outreach.Writer{Caller: caller, Log: slog.Default()}, env.camp.ID, jobs)
	for _, line := range res.lines {
		_, _ = fmt.Fprintln(stdout, line)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "leads drafts sample: %s drafted, %d passed the check, nothing sendable; review them on the console's Drafts tab (/leads/drafts).\n",
		plural(res.drafted, "lead", "leads"), res.passed)
	return nil
}

// draftJob is one lead to write, with its input.
type draftJob struct {
	c  leadstore.Candidate
	in outreach.WriteInput
}

// sampleJobs picks at most n of the campaign's candidate leads with an email route that have
// never been sampled, in the batch's order (starred or not), each with its input.
func sampleJobs(ctx context.Context, env draftsEnv, n int) ([]draftJob, error) {
	opts, err := batchCandidateOptions(ctx, env.lq, env.bs, leadsNow())
	if err != nil {
		return nil, err
	}
	opts.StarredOnly = false
	opts.CampaignID, opts.AccountID, opts.Countries = env.camp.ID, leadstore.DefaultAccountID, env.plan.candidateCountries()
	cands, err := env.lq.CandidateLeads(ctx, opts)
	if err != nil {
		return nil, err
	}
	ex, _, err := approvedExamples(ctx, env.lq, env.camp.ID)
	if err != nil {
		return nil, err
	}
	dc := draftCtx{cfg: env.setup.leads, plan: env.plan, campaignID: env.camp.ID, examples: ex}
	var picked []leadstore.Candidate
	for _, c := range cands {
		if len(picked) >= n {
			break
		}
		if c.Channel != leadconfig.ChannelEmail || c.RouteKind != leadstore.RouteEmail || c.Route == "" {
			continue
		}
		gens, err := env.lq.GenerationsForLead(ctx, c.LeadID, env.camp.ID)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(gens, func(g leadstore.Generation) bool { return g.Purpose == leadstore.PurposeSample }) {
			continue
		}
		picked = append(picked, c)
	}
	return draftJobs(ctx, env.lq, dc, picked)
}

// draftJobs builds each candidate's input; a lead whose input cannot be built is left out.
func draftJobs(ctx context.Context, lq leadstore.Queries, dc draftCtx, cands []leadstore.Candidate) ([]draftJob, error) {
	if len(cands) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(cands))
	for i, c := range cands {
		ids[i] = c.LeadID
	}
	ev, err := lq.LeadEvidence(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []draftJob
	now := leadsNow()
	for _, c := range cands {
		// The lead's own plan and number, by the batch's rules: a call-first lead is written
		// to call_first's steps, and without a number it gets no email at all.
		phone, leftOut, err := leadDraftPhone(ctx, lq, dc.plan, c, now)
		if err != nil {
			return nil, err
		}
		plan, seqName := dc.plan.forLead(c.Country)
		if seqName != "" && phone == nil {
			_, _ = fmt.Fprintf(stdout, "lead %d %s: left out: call first (email needs their yes on a call): %s\n",
				c.LeadID, c.Domain, cmpOr(leftOut, callNoNumber))
			continue
		}
		in, err := dc.writeInput(ctx, lq, plan, c, ev[c.LeadID], phone != nil)
		if err != nil {
			_, _ = fmt.Fprintf(stdout, "lead %d %s: left out: %v\n", c.LeadID, c.Domain, err)
			continue
		}
		out = append(out, draftJob{c: c, in: in})
	}
	return out, nil
}

// sampleResult is what a sample did, in lines for stdout.
type sampleResult struct {
	drafted, passed int
	lines           []string
}

// sampleDrafts writes each job with w and records every attempt as a sample generation. A
// cap stops the sample (what is done stays); a usage-store or recording failure is an error.
func sampleDrafts(ctx context.Context, lq leadstore.Queries, w *outreach.Writer, campaignID int64, jobs []draftJob) (sampleResult, error) {
	var r sampleResult
	for _, j := range jobs {
		res, err := w.WriteLead(ctx, j.in)
		ids, rerr := recordAttempts(ctx, lq, j.c.LeadID, campaignID, j.in.Sheet, res.Attempts, leadstore.PurposeSample)
		if rerr != nil {
			return r, rerr
		}
		switch {
		case err != nil && errors.Is(err, ai.ErrSpendCap):
			r.lines = append(r.lines, fmt.Sprintf("Stopped: %s (%v).", capReason(err), err))
			return r, nil
		case err != nil:
			return r, err
		}
		r.drafted++
		if res.Written != nil {
			r.passed++
			r.lines = append(r.lines, fmt.Sprintf("lead %d %s: passed, generation %d", j.c.LeadID, j.c.Domain, ids[len(ids)-1]))
		} else {
			r.lines = append(r.lines, fmt.Sprintf("lead %d %s: %s", j.c.LeadID, j.c.Domain, res.Fallback))
		}
	}
	return r, nil
}

// --- eval ---

func runDraftsEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads drafts eval", flag.ContinueOnError)
	slug := fs.String("campaign", campaign.LegacySlug, campaignFlagUsage)
	runs := fs.Int("runs", defaultEvalRuns, "how many times each reviewed lead is regenerated")
	dec := fs.String("decider", deciderFake, "fake (replays each reviewed draft) or claude (LEADHOUND_LIVE=1 and the key)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *runs < 1 {
		return usageError(draftsEvalUsage)
	}
	decider, err := parseDecider(*dec)
	if err != nil {
		return err
	}
	env, err := openDrafts(ctx, *slug)
	if err != nil {
		return err
	}
	defer env.s.Close()
	reviewed, err := reviewedForEval(ctx, env.lq, env.camp.ID)
	if err != nil {
		return err
	}
	if decider == deciderClaude && len(reviewed) < leadstore.MinEvalReviewed {
		return usageError("leads drafts eval: %d of %d reviewed leads; a Claude eval is recorded only with at least %d, so nothing was run. Review more sampled drafts on the Drafts tab first.",
			len(reviewed), leadstore.MinEvalReviewed, leadstore.MinEvalReviewed)
	}
	if len(reviewed) == 0 {
		_, _ = fmt.Fprintf(stdout, "leads drafts eval: no reviewed drafts in %s yet; nothing to evaluate.\n", *slug)
		return nil
	}
	ex, _, err := approvedExamples(ctx, env.lq, env.camp.ID)
	if err != nil {
		return err
	}
	dc := draftCtx{cfg: env.setup.leads, plan: env.plan, campaignID: env.camp.ID, examples: ex}
	var cands []leadstore.Candidate
	for _, rl := range reviewed {
		c, err := leadCandidate(ctx, env.lq, rl.leadID)
		if err != nil {
			return err
		}
		cands = append(cands, c)
	}
	jobs, err := draftJobs(ctx, env.lq, dc, cands) // prints each lead it leaves out, and why
	if err != nil {
		return err
	}
	// The gate counts the leads the eval really regenerated, never the reviewed ones it could
	// not build an input for.
	if left := len(reviewed) - distinctLeads(jobs); left > 0 {
		_, _ = fmt.Fprintf(stdout, "Left out: %d of %d reviewed leads (named above); the eval counts the other %d.\n",
			left, len(reviewed), len(reviewed)-left)
	}
	if n := distinctLeads(jobs); decider == deciderClaude && n < leadstore.MinEvalReviewed {
		return usageError("leads drafts eval: %d of %d reviewed leads can be evaluated; a Claude eval is recorded only with at least %d, so nothing was run. Fix or replace the leads left out above.",
			n, len(reviewed), leadstore.MinEvalReviewed)
	}
	ins := make([]outreach.WriteInput, 0, len(jobs)*(*runs))
	for range *runs {
		for _, j := range jobs {
			ins = append(ins, j.in)
		}
	}
	if err := printEstimate(ctx, env, decider, claude.PurposeDraftsEval, ins); err != nil {
		return err
	}
	var caller outreach.Caller
	if decider == deciderClaude {
		if caller, err = newDraftCaller("leads drafts eval", env.s.Q(), claude.PurposeDraftsEval); err != nil || caller == nil {
			return err
		}
	}
	replays := map[int64]json.RawMessage{}
	for _, rl := range reviewed {
		replays[rl.leadID] = rl.output
	}
	rep, err := evalDrafts(ctx, env.lq, caller, replays, env.camp.ID, jobs, *runs)
	if err != nil {
		return err
	}
	printDraftEval(stdout, *slug, decider, rep)
	if decider != deciderClaude {
		_, _ = fmt.Fprintln(stdout, "Not recorded: only a Claude eval counts toward the gate.")
	} else if rep.stopped != "" {
		_, _ = fmt.Fprintf(stdout, "Not recorded: the eval stopped early (%s).\n", rep.stopped)
	} else if rep.reviewed < leadstore.MinEvalReviewed {
		_, _ = fmt.Fprintf(stdout, "Not recorded: only %d reviewed leads were evaluated, at least %d are needed.\n",
			rep.reviewed, leadstore.MinEvalReviewed)
	} else {
		id, err := env.lq.RecordEval(ctx, rep.row(env.camp.ID))
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "Recorded as eval %d.\n", id)
	}
	st, err := draftGate(ctx, env.lq, env.camp.ID)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, st.Summary())
	return nil
}

// reviewedLead is one reviewed lead of an eval, with its latest reviewed draft's answer (the
// fake decider replays it).
type reviewedLead struct {
	leadID int64
	output json.RawMessage
}

// reviewedForEval are the campaign's leads with a reviewed Claude draft, in lead id order.
func reviewedForEval(ctx context.Context, lq leadstore.Queries, campaignID int64) ([]reviewedLead, error) {
	rs, gens, err := claudeReviews(ctx, lq, campaignID)
	if err != nil {
		return nil, err
	}
	seen := map[int64]json.RawMessage{}
	for _, r := range rs { // newest first: the first per lead is its latest
		if _, ok := seen[r.LeadID]; !ok {
			seen[r.LeadID] = gens[r.GenerationID].Output
		}
	}
	ids := slices.Sorted(maps.Keys(seen))
	out := make([]reviewedLead, len(ids))
	for i, id := range ids {
		out[i] = reviewedLead{leadID: id, output: seen[id]}
	}
	return out, nil
}

// distinctLeads is how many different leads jobs hold.
func distinctLeads(jobs []draftJob) int {
	seen := map[int64]bool{}
	for _, j := range jobs {
		seen[j.c.LeadID] = true
	}
	return len(seen)
}

// draftEvalReport is what an eval measured: reviewed is the distinct leads it really
// regenerated at least once (a lead left out, or not reached before a stop, is not one).
type draftEvalReport struct {
	runs, firstTry, afterRetry, reviewed int
	failures                             map[string]int // code -> attempts failing it
	words                                map[int][]int  // step -> words of each grounded draft
	tokens, cost                         int64
	stopped                              string // why the eval stopped before its last run
}

func (r draftEvalReport) share(n int) float64 {
	if r.runs == 0 {
		return 0
	}
	return float64(n) / float64(r.runs)
}

func (r draftEvalReport) meanWords() map[string]float64 {
	out := map[string]float64{}
	for step, ws := range r.words {
		sum := 0
		for _, w := range ws {
			sum += w
		}
		out[strconv.Itoa(step)] = float64(sum) / float64(len(ws))
	}
	return out
}

func (r draftEvalReport) row(campaignID int64) leadstore.DraftEvalRow {
	return leadstore.DraftEvalRow{CampaignID: campaignID, Runs: r.runs, Reviewed: r.reviewed,
		FirstTryPass: r.share(r.firstTry), AfterRetryPass: r.share(r.afterRetry), Failures: r.failures,
		MeanWords: r.meanWords(), Tokens: r.tokens, CostMicros: r.cost, Decider: leadstore.DeciderClaude}
}

// evalDrafts regenerates each job runs times: with caller (Claude, every attempt recorded as
// an eval generation), or, when caller is nil, by replaying the lead's reviewed answer.
func evalDrafts(ctx context.Context, lq leadstore.Queries, caller outreach.Caller, replays map[int64]json.RawMessage,
	campaignID int64, jobs []draftJob, runs int) (rep draftEvalReport, _ error) {
	rep = draftEvalReport{failures: map[string]int{}, words: map[int][]int{}}
	evaluated := map[int64]bool{}
	defer func() { rep.reviewed = len(evaluated) }()
	for range runs {
		for _, j := range jobs {
			w := &outreach.Writer{Caller: caller, Log: slog.Default()}
			if caller == nil {
				w.Caller = replayCaller{out: replays[j.c.LeadID]}
			}
			res, err := w.WriteLead(ctx, j.in)
			if caller != nil {
				if _, rerr := recordAttempts(ctx, lq, j.c.LeadID, campaignID, j.in.Sheet, res.Attempts, leadstore.PurposeEval); rerr != nil {
					return rep, rerr
				}
			}
			for _, a := range res.Attempts {
				rep.cost += a.CostMicros
				rep.tokens += int64(a.Usage.InputTokens + a.Usage.OutputTokens + a.Usage.CacheReadTokens + a.Usage.CacheWriteTokens)
			}
			switch {
			case err != nil && errors.Is(err, ai.ErrSpendCap):
				rep.stopped = capReason(err)
				return rep, nil
			case err != nil:
				return rep, err
			}
			rep.runs++
			evaluated[j.c.LeadID] = true
			if len(res.Attempts) > 0 && res.Attempts[0].OK() {
				rep.firstTry++
			}
			if res.Written != nil {
				rep.afterRetry++
				for _, st := range res.Written.Steps {
					var text []string
					for _, p := range st.Paragraphs {
						text = append(text, p.Text)
					}
					rep.words[st.Step] = append(rep.words[st.Step], len(strings.Fields(strings.Join(text, " "))))
				}
			}
			for _, a := range res.Attempts {
				codes := map[string]bool{}
				for _, f := range a.Failures {
					codes[f.Code] = true
				}
				if !a.OK() && len(a.Failures) == 0 {
					codes[failStop+" ("+a.StopReason+")"] = true
				}
				for c := range codes {
					rep.failures[c]++
				}
			}
		}
	}
	return rep, nil
}

func pctOf(n, of int) string {
	if of == 0 {
		return "0%"
	}
	return strconv.Itoa(n*100/of) + "%"
}

func printDraftEval(w io.Writer, slug, decider string, r draftEvalReport) {
	_, _ = fmt.Fprintf(w, "leads drafts eval: campaign %s · decider %s · %s · %s\n", slug, decider,
		plural(r.reviewed, "reviewed lead", "reviewed leads"), plural(r.runs, "run", "runs"))
	_, _ = fmt.Fprintf(w, "First try: %d of %d (%s) · after retry: %d of %d (%s)\n",
		r.firstTry, r.runs, pctOf(r.firstTry, r.runs), r.afterRetry, r.runs, pctOf(r.afterRetry, r.runs))
	if len(r.failures) == 0 {
		_, _ = fmt.Fprintln(w, "Attempts failing each rule: none")
	} else {
		var parts []string
		for _, c := range slices.Sorted(maps.Keys(r.failures)) {
			parts = append(parts, fmt.Sprintf("%s %d", c, r.failures[c]))
		}
		_, _ = fmt.Fprintf(w, "Attempts failing each rule: %s\n", strings.Join(parts, ", "))
	}
	if mw := r.meanWords(); len(mw) > 0 {
		steps := slices.SortedFunc(maps.Keys(mw), func(a, b string) int {
			x, _ := strconv.Atoi(a)
			y, _ := strconv.Atoi(b)
			return cmp.Compare(x, y)
		})
		var parts []string
		for _, s := range steps {
			parts = append(parts, fmt.Sprintf("step %s %.1f", s, mw[s]))
		}
		_, _ = fmt.Fprintf(w, "Mean words: %s\n", strings.Join(parts, " · "))
	}
	_, _ = fmt.Fprintf(w, "Tokens: %d · cost %s\n", r.tokens, usdWords(r.cost))
	if r.stopped != "" {
		_, _ = fmt.Fprintf(w, "Stopped early: %s\n", r.stopped)
	}
}

// --- golden export ---

// goldenDraft is one line of a golden export: a reviewed Claude draft with its facts and the
// operator's verdict.
type goldenDraft struct {
	GenerationID int64              `json:"generation_id"`
	LeadID       int64              `json:"lead_id"`
	Campaign     string             `json:"campaign"`
	Model        string             `json:"model"`
	Verdict      string             `json:"verdict"`
	Reasons      []string           `json:"reasons"`
	Note         string             `json:"note,omitempty"`
	FactSHA256   string             `json:"fact_sha256"`
	Facts        outreach.FactSheet `json:"facts"`
	Output       json.RawMessage    `json:"output"`
	ReviewedAt   time.Time          `json:"reviewed_at"`
}

func runDraftsGolden(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads drafts golden-export", flag.ContinueOnError)
	out := fs.String("out", "", "the file to write (outside the repository; never overwritten)")
	slug := fs.String("campaign", campaign.LegacySlug, campaignFlagUsage)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *out == "" {
		return usageError(draftsGoldenUsage)
	}
	path, err := goldenOutPath(*out)
	if err != nil {
		return err
	}
	s, err := openLeadsStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	c, err := activeCampaign(ctx, lq, *slug)
	if err != nil {
		return err
	}
	n, err := goldenExport(ctx, lq, c, path)
	if err != nil {
		return err
	}
	if n == 0 {
		_, _ = fmt.Fprintf(stdout, "leads drafts golden-export: no reviewed Claude drafts in %s; nothing written\n", c.Slug)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "leads drafts golden-export: %s written to %s, mode 0600\n", plural(n, "reviewed draft", "reviewed drafts"), path)
	return nil
}

// goldenOutPath is out made absolute, refused inside a checkout of this repository (the
// drafts name real organisations) or when it exists.
func goldenOutPath(out string) (string, error) {
	path, err := filepath.Abs(out)
	if err != nil {
		return "", usageError("--out %s: %v", out, err)
	}
	if root, inside := insideRepo(path); inside {
		return "", usageError("--out %s is inside the leadhound repository at %s; the drafts name real organisations, so write it outside, e.g. under $LEADHOUND_DATA_DIR/golden/", path, root)
	}
	if _, err := os.Lstat(path); err == nil {
		return "", usageError("--out %s exists; a golden export never overwrites a file, so move it or choose another name", path)
	}
	return path, nil
}

// goldenExport writes every reviewed Claude draft of campaign c (its latest verdict) to path,
// oldest review first, one JSON line each; nothing is written when there are none.
func goldenExport(ctx context.Context, lq leadstore.Queries, c leadstore.Campaign, path string) (int, error) {
	path, err := goldenOutPath(path)
	if err != nil {
		return 0, err
	}
	rs, gens, err := claudeReviews(ctx, lq, c.ID)
	if err != nil {
		return 0, err
	}
	if len(rs) == 0 {
		return 0, nil
	}
	var b strings.Builder
	for _, r := range slices.Backward(rs) {
		g := gens[r.GenerationID]
		line, err := json.Marshal(goldenDraft{GenerationID: g.ID, LeadID: g.LeadID, Campaign: c.Slug, Model: g.Model,
			Verdict: r.Verdict, Reasons: r.Reasons, Note: r.Note, FactSHA256: g.FactSHA256, Facts: g.Facts,
			Output: g.Output, ReviewedAt: r.At})
		if err != nil {
			return 0, fmt.Errorf("golden export: %w", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := writeNew(path, []byte(b.String())); err != nil {
		return 0, fmt.Errorf("golden export: %w", errors.Unwrap(err))
	}
	return len(rs), nil
}
