package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
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
// exits 0. With Jev as the decider, a rating that finished with calls left then asks the
// place-type question about the open café leads and the business-kind question about every
// open lead (rateAll), within what is left.
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
	leads, err := lq.LeadsToRate(ctx, leadstore.ListContext{ProfileJSON: pf, TrustedTypes: w.trusted, CampaignID: c.ID})
	if err != nil {
		return err
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsRate, deciderKind)
	if err != nil {
		return err
	}
	rr := rateRun{
		lq:       lq,
		decider:  &ai.Capped{Decider: decider, Store: q, DailyCalls: setup.jevLeadsDailyCap(), Loc: loc, Purpose: ai.PurposeLeads},
		kind:     deciderKind,
		name:     decider.Name(),
		runID:    runID,
		max:      *maxCalls,
		cfg:      setup.leads,
		campaign: c.ID,
	}
	sum, ps, ks, runErr := rr.rateAll(ctx, leads)
	result := rateResult(sum, ps, ks)
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
	printPlaceTypeSummary(stdout, ps)
	printBusinessKindSummary(stdout, ks)
	return ctx.Err()
}

// rateResult is the run row of a rating: the calls and failures of every pass that ran, the
// first stop reason among them, and a note of what each did.
func rateResult(sum rateSummary, ps placeSummary, ks businessSummary) store.RunResult {
	result := store.RunResult{
		Requests: sum.Calls + ps.Calls + ks.Calls, Failures: sum.Failures + ps.Failures + ks.Failures,
		StopReason: cmp.Or(sum.StopReason, ps.StopReason, ks.StopReason),
		Note:       fmt.Sprintf("%d rated, %d unchanged, %d still due", sum.Rated, sum.Unchanged, sum.Due),
	}
	if ps.Ran {
		result.Note += fmt.Sprintf("; place type: %d coffee shop, %d roastery, %d unclear, %d unchanged, %d still due",
			ps.Cafe, ps.Roastery, ps.Unclear, ps.Unchanged, ps.Due)
	}
	if ks.Ran {
		result.Note += fmt.Sprintf("; business kind: %d named, %d unclear, %d unchanged, %d still due",
			ks.Named, ks.Unclear, ks.Unchanged, ks.Due)
	}
	return result
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

// relevanceFake is the keyless stand-in for the relevance question of every rating type
// (they share one question name): every lead is a "maybe", spread so its number is 50.
func relevanceFake() *ai.Fake {
	return &ai.Fake{Answers: map[string]ai.Answer{leadrate.AIQuestion(leadstore.TypeDrupal).Name: {
		Choice: leadstore.BandMaybe, Confidence: 0.4,
		Probabilities: map[string]float64{
			leadstore.BandStrong: 0.1, leadstore.BandLikely: 0.2, leadstore.BandMaybe: 0.4,
			leadstore.BandUnlikely: 0.2, leadstore.BandNo: 0.1,
		},
	}}}
}

// rateInput is what the relevance question of lead l's rating type reads about it: its
// facts, and its major version's end-of-life date from the effective config; a café lead
// also its café facts (cafeFacts). A Drupal lead's input is exactly what it was before café
// leads had their own question.
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
	if leadstore.TypeOf(l.Kind) == leadstore.TypeCafe {
		in.Cafe = cafeFacts(l)
	}
	return in
}

// cafeFacts is café lead l's own facts: its place's category, town and region, whether the
// build read a website of its own, and what its homepage showed (its finding's evidence read
// back through probe.CafeResult), vendors by the names signals.yaml gives them. A vendor
// signals.yaml does not name (the own vendor) is left out; a phrase still counts as seen.
func cafeFacts(l leadstore.RateLead) *leadrate.CafeFacts {
	sigs := make([]leadstore.Signal, 0, len(l.Signals))
	for _, pair := range l.Signals {
		name, value, _ := strings.Cut(pair, ":")
		sigs = append(sigs, leadstore.Signal{Signal: name, Value: value})
	}
	r := probe.CafeResult(sigs)
	var ordering []string
	for _, sg := range r.SignalsOf(cafe.SigOrdering) {
		if sg.Via != cafe.ViaWord {
			ordering = append(ordering, sg.Vendor)
		}
	}
	return &leadrate.CafeFacts{
		Category: l.PlaceCategory, Locality: l.PlaceLocality, Region: l.PlaceRegion,
		OwnWebsite:  l.CafeWebsite != "",
		LoyaltySeen: r.HasLoyalty, LoyaltyVendors: vendorNames(r.Vendors),
		OrderingSeen: r.HasOrdering, OrderingVendors: vendorNames(ordering),
		Instagram: r.HasInstagram, Language: cmp.Or(l.Language, r.Language),
	}
}

// vendorNames are the signals.yaml names of the vendors ids, in order; an id it does not
// list is left out, so an id never stands in for a name.
func vendorNames(ids []string) []string {
	var out []string
	for _, id := range ids {
		if v := cafe.DefaultConfig().Vendor(id); v != nil && !v.Own {
			out = append(out, v.Name)
		}
	}
	return out
}

// trustedTypes are the rating types whose relevance passed its trust gate: the latest eval
// of the type's own question (leadrate.QuestionFor) is trusted. The list, leads rate's order
// and the batch queue sort the leads of these types by relevance.
func trustedTypes(ctx context.Context, lq leadstore.Queries) ([]string, error) {
	var out []string
	for _, t := range leadstore.RatingTypes {
		e, ok, err := lq.LatestRelevanceEval(ctx, leadrate.QuestionFor(t))
		if err != nil {
			return nil, err
		}
		if ok && e.Trusted {
			out = append(out, t)
		}
	}
	return out, nil
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
	// campaign is the campaign whose café leads the place-type pass reads, 0 for every
	// campaign's, as LeadsToRate read the leads.
	campaign int64
}

// rate asks about every lead in leads, in order, whose input hash differs from its latest
// rating's under its own type's question (each lead is asked its type's question), until --max calls, the cap, an unavailable provider, rateFailuresInARow failures
// in a row or a cancel. Only a store error (the rating's or the cap's usage store) is
// returned. The writes run without the run's cancel.
func (rr rateRun) rate(ctx context.Context, leads []leadstore.RateLead) (sum rateSummary, err error) {
	type due struct {
		lead  leadstore.RateLead
		in    leadrate.Input
		hash  []byte
		rtype string // the lead's rating type, whose question it is asked
	}
	var todo []due
	for _, l := range leads {
		in := rateInput(l, rr.cfg)
		h := leadrate.InputHash(rr.name, in)
		if bytes.Equal(h, l.LatestSHA) {
			sum.Unchanged++
			continue
		}
		todo = append(todo, due{lead: l, in: in, hash: h, rtype: leadstore.TypeOf(l.Kind)})
	}
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
		q := leadrate.AIQuestion(d.rtype)
		answers, usage, err := rr.decider.Decide(ctx, leadrate.State(d.in), []ai.Question{q})
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
		answer := answers[q.Name]
		score, band := leadrate.Score(answer)
		if band == "" {
			sum.Failures++ // an answer outside the bands; the decider checks its options, so never expected
			continue
		}
		inARow = 0
		if _, err := rr.lq.InsertRelevance(wctx, leadstore.RelevanceRow{
			LeadID: d.lead.LeadID, Question: leadrate.QuestionFor(d.rtype), Decider: rr.kind,
			Model: cutRunes(cmp.Or(usage.Model, rr.name), rateModelRunes), InputSHA256: d.hash,
			Band: band, Score: score, Probabilities: answer.Probabilities, InputTokens: usage.InputTokens, RunID: rr.runID,
		}); err != nil {
			return sum, err
		}
		sum.Rated++
	}
	return sum, nil
}

// placeSummary is what one run's place-type pass did.
type placeSummary struct {
	Ran        bool // the pass ran: Jev was the decider and relevance finished with calls left
	Cafe       int  // coffee-shop answers stored
	Roastery   int  // roastery answers stored
	Unclear    int  // unclear answers stored
	Unchanged  int  // open café leads whose latest Jev answer has the same input
	Due        int  // open café leads still to ask when the pass ended
	Calls      int  // calls the decider was asked (the cap's refusals are not calls)
	Failures   int  // calls that gave no answer
	Tokens     int  // input tokens over every call
	StopReason string
}

// businessSummary is what one run's business-kind pass did.
type businessSummary struct {
	Ran        bool // the pass ran: Jev was the decider and both earlier passes finished with calls left
	Named      int  // answers stored as a kind other than unclear
	Unclear    int  // unclear answers stored, a low confidence's included
	Unchanged  int  // open leads whose latest answer has the same input
	Due        int  // open leads still to ask when the pass ended
	Calls      int  // calls the decider was asked (the cap's refusals are not calls)
	Failures   int  // calls that gave no answer
	Tokens     int  // input tokens over every call
	StopReason string
}

// rateAll is one run: the relevance pass over leads first, then, with Jev as the decider and
// only when relevance finished with calls left under --max, the place-type pass over the
// open café leads among them, then, only when that also finished with calls left, the
// business-kind pass over the open leads among them. The passes share the run's --max and the
// leads cap, in that order, so relevance always spends first. A fake decider stores no place
// type and no business kind, since its answer would stand in for Jev's.
func (rr rateRun) rateAll(ctx context.Context, leads []leadstore.RateLead) (rateSummary, placeSummary, businessSummary, error) {
	sum, err := rr.rate(ctx, leads)
	if err != nil || sum.StopReason != "" || ctx.Err() != nil || rr.kind != rateDeciderJev {
		return sum, placeSummary{}, businessSummary{}, err
	}
	ps, err := rr.placeTypes(ctx, leads, rr.max-sum.Calls)
	if err != nil || ps.StopReason != "" || ctx.Err() != nil {
		return sum, ps, businessSummary{}, err
	}
	ks, err := rr.businessKinds(ctx, leads, rr.max-sum.Calls-ps.Calls)
	return sum, ps, ks, err
}

// businessKinds asks the business-kind question of its own rating type (BusinessKindAIQuestion)
// about every open lead in leads, in their order, whose latest answer was asked on another input,
// and stores each answer with its input hash and confidence: the kind chosen, or unclear below
// leadrate.BusinessKindMinConfidence. An unclear answer on the same input is not asked again. It
// stops at maxCalls calls, the cap, an unavailable provider, rateFailuresInARow failures in a row
// or a cancel, as placeTypes does. Only a store error is returned. The writes run without the
// run's cancel.
func (rr rateRun) businessKinds(ctx context.Context, leads []leadstore.RateLead, maxCalls int) (ks businessSummary, err error) {
	ks.Ran = true
	open, err := rr.lq.BusinessKindLeads(ctx, rr.campaign)
	if err != nil {
		return ks, err
	}
	latest := make(map[int64][]byte, len(open))
	for _, o := range open {
		latest[o.LeadID] = o.LatestSHA
	}
	type due struct {
		lead  leadstore.RateLead
		in    leadrate.Input
		hash  []byte
		rtype string // the lead's rating type, whose question it is asked
	}
	var todo []due
	for _, l := range leads {
		sha, ok := latest[l.LeadID]
		if !ok {
			continue
		}
		in := rateInput(l, rr.cfg)
		h := leadrate.BusinessKindHash(rr.name, in)
		if bytes.Equal(h, sha) {
			ks.Unchanged++
			continue
		}
		todo = append(todo, due{lead: l, in: in, hash: h, rtype: leadstore.TypeOf(l.Kind)})
	}
	wctx := context.WithoutCancel(ctx)
	inARow, stored := 0, 0
	defer func() { ks.Due = len(todo) - stored }() // a failed lead stays due
	for _, d := range todo {
		if ctx.Err() != nil {
			return ks, nil
		}
		if ks.Calls >= maxCalls {
			ks.StopReason = rateStopMax
			return ks, nil
		}
		q := leadrate.BusinessKindAIQuestion(d.rtype)
		answers, usage, err := rr.decider.Decide(ctx, leadrate.State(d.in), []ai.Question{q})
		if !errors.Is(err, ai.ErrSpendCap) {
			ks.Calls++
		}
		ks.Tokens += usage.InputTokens
		if err != nil {
			switch {
			case errors.Is(err, ai.ErrUsageStore):
				return ks, err
			case errors.Is(err, ai.ErrSpendCap):
				ks.StopReason = scoring.StopSpendCap
				return ks, nil
			case errors.Is(err, ai.ErrUnavailable):
				ks.Failures++
				ks.StopReason = scoring.StopThrottled
				return ks, nil
			case ctx.Err() != nil:
				return ks, nil
			}
			ks.Failures++
			if inARow++; inARow >= rateFailuresInARow {
				ks.StopReason = rateStopFailures
				return ks, nil
			}
			continue
		}
		answer := answers[q.Name]
		kind, ok := leadrate.BusinessKindOf(answer.Choice, answer.Confidence)
		if !ok {
			ks.Failures++ // an answer outside the options; the decider checks its options, so never expected
			continue
		}
		inARow = 0
		if err := rr.lq.SetBusinessKind(wctx, d.lead.LeadID, kind, leadrate.BusinessKindQuestion, d.hash, answer.Confidence); err != nil {
			return ks, err
		}
		stored++
		if kind == leadrate.BusinessKindUnclear {
			ks.Unclear++
		} else {
			ks.Named++
		}
	}
	return ks, nil
}

// placeTypes asks the place-type question (leadrate.PlaceTypeQuestion) about every open café
// lead in leads, in their order, whose latest Jev place-type row was asked on another input,
// and stores each answer as a Jev row with its input hash: cafe, roastery or unclear. It
// stops at maxCalls calls, the cap, an unavailable provider, rateFailuresInARow failures in
// a row or a cancel, as rate does. Only a store error is returned. The writes run without
// the run's cancel.
func (rr rateRun) placeTypes(ctx context.Context, leads []leadstore.RateLead, maxCalls int) (ps placeSummary, err error) {
	ps.Ran = true
	open, err := rr.lq.JevPlaceTypeLeads(ctx, rr.campaign)
	if err != nil {
		return ps, err
	}
	latest := make(map[int64][]byte, len(open))
	for _, o := range open {
		latest[o.LeadID] = o.LatestSHA
	}
	type due struct {
		lead leadstore.RateLead
		in   leadrate.Input
		hash []byte
	}
	var todo []due
	for _, l := range leads {
		sha, ok := latest[l.LeadID]
		if !ok || leadstore.TypeOf(l.Kind) != leadstore.TypeCafe {
			continue
		}
		in := rateInput(l, rr.cfg)
		h := leadrate.PlaceTypeHash(rr.name, in)
		if bytes.Equal(h, sha) {
			ps.Unchanged++
			continue
		}
		todo = append(todo, due{lead: l, in: in, hash: h})
	}
	wctx := context.WithoutCancel(ctx)
	inARow, stored := 0, 0
	defer func() { ps.Due = len(todo) - stored }() // a failed lead stays due
	q := leadrate.PlaceTypeAIQuestion()
	for _, d := range todo {
		if ctx.Err() != nil {
			return ps, nil
		}
		if ps.Calls >= maxCalls {
			ps.StopReason = rateStopMax
			return ps, nil
		}
		answers, usage, err := rr.decider.Decide(ctx, leadrate.State(d.in), []ai.Question{q})
		if !errors.Is(err, ai.ErrSpendCap) {
			ps.Calls++
		}
		ps.Tokens += usage.InputTokens
		if err != nil {
			switch {
			case errors.Is(err, ai.ErrUsageStore):
				return ps, err
			case errors.Is(err, ai.ErrSpendCap):
				ps.StopReason = scoring.StopSpendCap
				return ps, nil
			case errors.Is(err, ai.ErrUnavailable):
				ps.Failures++
				ps.StopReason = scoring.StopThrottled
				return ps, nil
			case ctx.Err() != nil:
				return ps, nil
			}
			ps.Failures++
			if inARow++; inARow >= rateFailuresInARow {
				ps.StopReason = rateStopFailures
				return ps, nil
			}
			continue
		}
		placeType, ok := leadrate.PlaceTypeOf(answers[q.Name].Choice)
		if !ok {
			ps.Failures++ // an answer outside the options; the decider checks its options, so never expected
			continue
		}
		inARow = 0
		if err := rr.lq.SetPlaceType(wctx, d.lead.LeadID, placeType, leadstore.PlaceFromJev,
			leadrate.PlaceTypeQuestion, d.hash, leadstore.ActorSystem); err != nil {
			return ps, err
		}
		stored++
		switch placeType {
		case leadstore.PlaceCafe:
			ps.Cafe++
		case leadstore.PlaceRoastery:
			ps.Roastery++
		default:
			ps.Unclear++
		}
	}
	return ps, nil
}

// printPlaceTypeSummary prints the place-type pass's line when it ran.
func printPlaceTypeSummary(w io.Writer, ps placeSummary) {
	if !ps.Ran {
		return
	}
	_, _ = fmt.Fprintf(w, "  place type: coffee shop %d · roastery %d · unclear %d · unchanged %d · still due %d · calls %d · failed calls %d · stop %s\n",
		ps.Cafe, ps.Roastery, ps.Unclear, ps.Unchanged, ps.Due, ps.Calls, ps.Failures, cmp.Or(ps.StopReason, "none"))
}

// printBusinessKindSummary prints the business-kind pass's line when it ran.
func printBusinessKindSummary(w io.Writer, ks businessSummary) {
	if !ks.Ran {
		return
	}
	_, _ = fmt.Fprintf(w, "  business kind: named %d · unclear %d · unchanged %d · still due %d · calls %d · failed calls %d · stop %s\n",
		ks.Named, ks.Unclear, ks.Unchanged, ks.Due, ks.Calls, ks.Failures, cmp.Or(ks.StopReason, "none"))
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
