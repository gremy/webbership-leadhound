package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["rate"] = runLeadsRate }

// The run row of a relevance rating; its source is the decider (jev or fake).
const commandLeadsRate = "leads-rate"

const (
	// defaultRateMax is the most calls one rating makes without --max.
	defaultRateMax = 10000
	// rateFailuresInARow is how many failed calls in a row stop a rating, so a provider that
	// refuses every call (a bad key, an outage that is not a 429) is not asked about every lead.
	rateFailuresInARow = 5
	// rateModelRunes is the most of the response's model name a rating stores.
	rateModelRunes = 100
	// rateLockKey is the session advisory lock a rating holds for its length, so the timer
	// and a hand run never ask about the same lead twice.
	rateLockKey int64 = 0x4c48_5241_5445 // "LHRATE"
)

// Stop reasons of a rating besides scoring's spend_cap and throttled, from the set
// run.stop_reason accepts.
const (
	rateStopMax      = scoring.StopBudget            // --max calls were made; the rest waits for the next run
	rateStopFailures = string(fetch.StopFailureRate) // rateFailuresInARow calls in a row failed
)

// The deciders a lead_relevance row names (lead_relevance.decider).
const (
	rateDeciderJev  = "jev"
	rateDeciderFake = "fake"
)

// ratingOffWords is the refusal while relevance rating is switched off.
const ratingOffWords = "Relevance rating is off: turn it on in Leads → Settings."

const leadsRateUsage = "usage: leadhound leads rate [--max N (at least 1)] [--decider fake|jev] [--campaign SLUG]"

// runLeadsRate is `leads rate [--max 10000] [--decider fake|jev]`: it asks the relevance
// question (leadrate, D8) about every open lead whose input changed since its latest rating,
// starred leads first in the operator's order, then the rest in the queue's order, and
// stores each answer as the lead's latest rating. Calls go through the leads cap
// (ai.Capped, purpose leads, at the effective leads cap), so they never touch the jobs cap.
// It refuses while rating is off in the effective settings. The default decider is jev,
// which needs TYPESAFE_API_KEY and LEADHOUND_LIVE=1; without the live switch it asks
// nothing and exits 0. A rating stopped by --max (stop budget), the cap (spend_cap), an
// unavailable provider (throttled) or failures in a row (failure_rate) is recorded and
// exits 0.
func runLeadsRate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads rate", flag.ContinueOnError)
	maxCalls := fs.Int("max", defaultRateMax, "most decider calls in this run")
	deciderName := fs.String("decider", "", "jev or fake (default jev, which needs TYPESAFE_API_KEY and LEADHOUND_LIVE=1)")
	slug := fs.String("campaign", campaign.LegacySlug, campaignFlagUsage)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || *maxCalls < 1 {
		return usageError(leadsRateUsage)
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	if off, err := switchedOff(ctx, s, leadstore.JobContacts, commandLeadsRate, cmp.Or(*deciderName, rateDeciderJev)); off || err != nil {
		return err
	}
	c, err := activeCampaign(ctx, leadstore.NewQueries(s.Pool()), *slug)
	if err != nil {
		return err
	}
	if !setup.leads.Relevance.RatingOn() {
		return usageError("%s", ratingOffWords)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	name := cmp.Or(*deciderName, rateDeciderJev)
	if name == rateDeciderJev && cfg.Live != config.LiveAll {
		_, _ = fmt.Fprintf(stdout, "leads rate: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	decider, deciderKind, err := relevanceDecider(cfg, name, "leads rate")
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return usageError("LEADHOUND_TZ: %v", err)
	}
	lock, err := leadstore.TryLock(ctx, s.Pool(), rateLockKey)
	if errors.Is(err, leadstore.ErrLockHeld) {
		_, _ = fmt.Fprintln(stdout, "leads rate: another rating run is in progress; skipped")
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()

	lq := leadstore.NewQueries(s.Pool())
	w, err := weighLeads(ctx, lq, setup)
	if err != nil {
		return err
	}
	pf, err := leadstore.ListProfileJSON(w.profile)
	if err != nil {
		return err
	}
	leads, err := lq.LeadsToRate(ctx, leadstore.ListContext{ProfileJSON: pf, SortByRelevance: w.trusted, CampaignID: c.ID})
	if err != nil {
		return err
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsRate, deciderKind)
	if err != nil {
		return err
	}
	rr := rateRun{
		lq:      lq,
		decider: &ai.Capped{Decider: decider, Store: q, DailyCalls: setup.jevLeadsDailyCap(), Loc: loc, Purpose: ai.PurposeLeads},
		kind:    deciderKind,
		name:    decider.Name(),
		runID:   runID,
		max:     *maxCalls,
		cfg:     setup.leads,
	}
	sum, runErr := rr.rate(ctx, leads)
	result := store.RunResult{
		Requests: sum.Calls, Failures: sum.Failures, StopReason: sum.StopReason,
		Note: fmt.Sprintf("%d rated, %d unchanged, %d still due", sum.Rated, sum.Unchanged, sum.Due),
	}
	switch {
	case runErr != nil:
		result.Note += "; " + runErr.Error()
	case ctx.Err() != nil:
		result.Note += "; interrupted"
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil {
		return runErr
	}
	printRateSummary(stdout, runID, deciderKind, rr.decider.DailyCalls, sum)
	return ctx.Err()
}

// relevanceDecider resolves --decider for the relevance commands as pickDecider does
// (score.go: the key check and the model pin), with the relevance stand-in in place of the
// jobs fake. kind is the decider as lead_relevance names it.
func relevanceDecider(cfg config.Config, name, command string) (d ai.Decider, kind string, err error) {
	d, err = pickDecider(cfg, name, command)
	if err != nil {
		return nil, "", err
	}
	if _, ok := d.(*ai.Fake); ok {
		return relevanceFake(), rateDeciderFake, nil
	}
	return d, rateDeciderJev, nil
}

// relevanceFake is the keyless stand-in for the relevance question: every lead is a
// "maybe", spread so its number is 50.
func relevanceFake() *ai.Fake {
	return &ai.Fake{Answers: map[string]ai.Answer{leadrate.AIQuestion().Name: {
		Choice: leadstore.BandMaybe, Confidence: 0.4,
		Probabilities: map[string]float64{
			leadstore.BandStrong: 0.1, leadstore.BandLikely: 0.2, leadstore.BandMaybe: 0.4,
			leadstore.BandUnlikely: 0.2, leadstore.BandNo: 0.1,
		},
	}}}
}

// rateInput is what the relevance question reads about lead l: its facts, and its major
// version's end-of-life date from the effective config.
func rateInput(l leadstore.RateLead, cfg leadconfig.Config) leadrate.Input {
	in := leadrate.Input{
		Kind: l.Kind, Major: l.Major, Version: l.ExactVersion, Country: l.Country,
		SiteName: l.SiteName, Description: l.Description, HasPhone: l.HasPhone, SocialLinks: l.SocialLinks,
	}
	if l.ListRank != nil {
		in.ListRank = *l.ListRank
	}
	if d, ok := cfg.Facts.EOL(l.Major); ok && !d.IsZero() {
		in.EOL = d.Time
	}
	if l.HasEmail {
		in.Channels = append(in.Channels, leadstore.ChannelEmail)
	}
	if l.HasForm {
		in.Channels = append(in.Channels, leadstore.ChannelForm)
	}
	return in
}

// rateSummary is what one rating did.
type rateSummary struct {
	Rated      int // ratings stored
	Unchanged  int // open leads whose latest rating has the same input
	Due        int // open leads still to rate when the run ended
	Calls      int // calls the decider was asked (the cap's refusals are not calls)
	Failures   int // calls that gave no rating
	Tokens     int // input tokens over every call
	StopReason string
}

// rateRun holds what every lead's rating shares.
type rateRun struct {
	lq      leadstore.Queries
	decider *ai.Capped
	kind    string // rateDeciderJev or rateDeciderFake
	name    string // the decider's name (the pinned model), which the input hash covers
	runID   int64
	max     int
	cfg     leadconfig.Config
}

// rate asks about every lead in leads, in order, whose input hash differs from its latest
// rating's, until --max calls, the cap, an unavailable provider, rateFailuresInARow failures
// in a row or a cancel. Only a store error (the rating's or the cap's usage store) is
// returned. The writes run without the run's cancel.
func (rr rateRun) rate(ctx context.Context, leads []leadstore.RateLead) (sum rateSummary, err error) {
	type due struct {
		lead leadstore.RateLead
		in   leadrate.Input
		hash []byte
	}
	var todo []due
	for _, l := range leads {
		in := rateInput(l, rr.cfg)
		h := leadrate.InputHash(rr.name, in)
		if bytes.Equal(h, l.LatestSHA) {
			sum.Unchanged++
			continue
		}
		todo = append(todo, due{lead: l, in: in, hash: h})
	}
	qs := []ai.Question{leadrate.AIQuestion()}
	wctx := context.WithoutCancel(ctx)
	inARow := 0
	defer func() { sum.Due = len(todo) - sum.Rated }() // a failed lead stays due
	for _, d := range todo {
		if ctx.Err() != nil {
			return sum, nil
		}
		if sum.Calls >= rr.max {
			sum.StopReason = rateStopMax
			return sum, nil
		}
		answers, usage, err := rr.decider.Decide(ctx, leadrate.State(d.in), qs)
		if !errors.Is(err, ai.ErrSpendCap) {
			sum.Calls++
		}
		sum.Tokens += usage.InputTokens
		if err != nil {
			switch {
			case errors.Is(err, ai.ErrUsageStore):
				return sum, err
			case errors.Is(err, ai.ErrSpendCap):
				sum.StopReason = scoring.StopSpendCap
				return sum, nil
			case errors.Is(err, ai.ErrUnavailable):
				sum.Failures++
				sum.StopReason = scoring.StopThrottled
				return sum, nil
			case ctx.Err() != nil:
				return sum, nil
			}
			sum.Failures++
			if inARow++; inARow >= rateFailuresInARow {
				sum.StopReason = rateStopFailures
				return sum, nil
			}
			continue
		}
		answer := answers[leadrate.AIQuestion().Name]
		score, band := leadrate.Score(answer)
		if band == "" {
			sum.Failures++ // an answer outside the bands; the decider checks its options, so never expected
			continue
		}
		inARow = 0
		if _, err := rr.lq.InsertRelevance(wctx, leadstore.RelevanceRow{
			LeadID: d.lead.LeadID, Question: leadrate.Question, Decider: rr.kind,
			Model: cutRunes(cmp.Or(usage.Model, rr.name), rateModelRunes), InputSHA256: d.hash,
			Band: band, Score: score, Probabilities: answer.Probabilities, InputTokens: usage.InputTokens, RunID: rr.runID,
		}); err != nil {
			return sum, err
		}
		sum.Rated++
	}
	return sum, nil
}

// cutRunes is s cut to at most n runes.
func cutRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// printRateSummary prints the rating's line, then what is left.
func printRateSummary(w io.Writer, runID int64, decider string, dailyCap int, sum rateSummary) {
	stop := cmp.Or(sum.StopReason, "none")
	_, _ = fmt.Fprintf(w, "leads rate: run %d · decider %s · rated %d · unchanged %d · stop %s\n",
		runID, decider, sum.Rated, sum.Unchanged, stop)
	_, _ = fmt.Fprintf(w, "  still due: %d · calls: %d · failed calls: %d · input tokens: %d · leads cap: %d calls a day\n",
		sum.Due, sum.Calls, sum.Failures, sum.Tokens, dailyCap)
}
