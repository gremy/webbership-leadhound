package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["recheck"] = runRecheck }

// recheckScoreBudget is the time budget of the rescore after the recheck: the brief waits
// for it.
const recheckScoreBudget = 10 * time.Minute

// recheckSlack is how many jobs past each tier's cap the recheck also looks at, so a
// listed job that turns out closed has a checked one to take its place.
const recheckSlack = 1 // times the cap

// recheckOutcome is what the recheck before a brief did.
type recheckOutcome struct {
	Off       bool // live fetching is off: nothing was requested
	Result    linkedin.RecheckResult
	Scored    bool           // the rechecked jobs were scored
	Score     scoring.Result // when Scored
	ScoreNote string         // why they were not, when there was something to score
	// Err is why the recheck or its rescore failed. The brief composes anyway: the failure
	// is in that run's row (a recheck that failed ends with the note "failed: …"), which
	// the brief's health reads, and a job left unchecked is left out by leaveOutStale.
	Err error
}

// summary is one line for stdout.
func (o recheckOutcome) summary() string {
	r := o.Result
	switch {
	case o.Off:
		return "recheck off (not live); nothing requested"
	case o.Err != nil && r.Requests == 0:
		return fmt.Sprintf("recheck: run %d · failed: %v", r.RunID, o.Err)
	case r.Due == 0:
		return "recheck: nothing due"
	case r.Paused:
		return fmt.Sprintf("recheck: run %d · %s", r.RunID, r.Note)
	}
	stop := r.StopReason
	if stop == "" {
		stop = "none"
	}
	s := fmt.Sprintf("recheck: run %d · %d requests (%d failed) · %s · stop %s", r.RunID, r.Requests, r.Failures, r.Note, stop)
	switch {
	case o.Err != nil && !strings.HasPrefix(r.Note, "failed: "):
		s += " · rescore failed: " + o.Err.Error()
	case o.Err != nil: // the note already says why
	case o.Scored:
		s += " · rescored: " + o.Score.Note()
	case o.ScoreNote != "":
		s += " · " + o.ScoreNote
	}
	return s
}

// rankNow reads the store and ranks it as the brief for day would, in one streamed pass.
func rankNow(ctx context.Context, q store.Queries, prof candidate.Config, loc *time.Location, day time.Time, allowFake bool) (scoring.Ranking, error) {
	pass, err := rankPass(ctx, q, loc, day, allowFake, rankOptions{caps: briefCaps(prof), desc: store.DescWhereRanked})
	return pass.ranking, err
}

func briefCaps(prof candidate.Config) scoring.Caps {
	return scoring.Caps{Apply: prof.Brief.MaxApply, YourCall: prof.Brief.MaxYourCall}
}

// recheckIDs are the jobs the brief is about to list, in rank order, Apply first: up to
// each tier's cap plus recheckSlack times it. An unreachable listing is ranked as if it
// were open, so the job left out last time is tried again. An ATS job the brief for day
// leaves out as not seen on its board takes no place.
func recheckIDs(ctx context.Context, q store.Queries, prof candidate.Config, loc *time.Location, day time.Time, allowFake bool) ([]int64, error) {
	caps := briefCaps(prof)
	pass, err := rankPass(ctx, q, loc, day, allowFake, rankOptions{
		caps:              scoring.Caps{Apply: caps.Apply * (1 + recheckSlack), YourCall: caps.YourCall * (1 + recheckSlack)},
		unreachableAsOpen: true,
		desc:              store.DescWhereRanked,
	})
	if err != nil {
		return nil, err
	}
	r := pass.ranking
	var ids []int64
	for _, rk := range append(r.Apply, r.YourCall...) {
		ids = append(ids, rk.JobID)
	}
	return ids, nil
}

// recheckListed rechecks the jobs about to be listed (LI4) through newClient, then scores
// the ones that got a new observation; the scorer's hash skips any whose text is
// unchanged. Scoring uses Jev when TYPESAFE_API_KEY is set, the fake when allowFake, and
// is otherwise left for the next score run. A failure is returned in the outcome's Err;
// one that came before the recheck wrote its run row still gets a recheck row, finished
// with the note "failed: …". day is the brief's day.
func recheckListed(ctx context.Context, s *store.Store, cfg config.Config, prof candidate.Config, loc *time.Location, day time.Time,
	allowFake bool, newClient func(fetch.Recorder) *fetch.Client, source string) recheckOutcome {
	out, err := recheckAndScore(ctx, s, cfg, prof, loc, day, allowFake, newClient, source)
	if err == nil {
		return out
	}
	out.Err = err
	if out.Result.RunID == 0 {
		q := s.Q()
		wctx := context.WithoutCancel(ctx)
		id, rerr := q.StartRun(wctx, linkedin.CommandRecheck, source)
		if rerr == nil {
			rerr = q.FinishRun(wctx, id, store.RunResult{Note: "failed: " + err.Error()})
		}
		if rerr != nil {
			out.Err = errors.Join(err, rerr)
		} else {
			out.Result.RunID = id
		}
	}
	return out
}

func recheckAndScore(ctx context.Context, s *store.Store, cfg config.Config, prof candidate.Config, loc *time.Location, day time.Time,
	allowFake bool, newClient func(fetch.Recorder) *fetch.Client, source string) (recheckOutcome, error) {
	ids, err := recheckIDs(ctx, s.Q(), prof, loc, day, allowFake)
	if err != nil {
		return recheckOutcome{}, err
	}
	rc := &linkedin.Rechecker{Store: s, NewClient: newClient, Loc: loc, Source: source, Gates: prof.Gates()}
	res, err := rc.Run(ctx, ids)
	out := recheckOutcome{Result: res}
	if err != nil {
		return out, err
	}
	if len(res.Observed) == 0 {
		return out, nil
	}
	var decider ai.Decider
	switch {
	case cfg.TypesafeAPIKey != "":
		decider = ai.NewJev(cfg.TypesafeAPIKey, cfg.JevModel)
	case allowFake:
		decider = fakeDecider()
	default:
		out.ScoreNote = "not rescored: TYPESAFE_API_KEY is not set"
		return out, nil
	}
	_, sr, err := scoreRun(ctx, s, cfg, prof, decider, loc, time.Now().Add(recheckScoreBudget))
	if errors.Is(err, errScoreBusy) {
		out.ScoreNote = "not rescored: another score run is in progress"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Scored, out.Score = true, sr
	return out, nil
}

// leaveOutStale marks unreachable every job from source the brief would list (both tiers
// and the runner-up) whose listing is still unchecked for longer than RecheckAfter, and
// ranks again until none is left: a job past the rechecked window, or one a failed recheck
// never reached, is left out today and rechecked next time, never sent unverified. It
// requests nothing and returns how many it marked. Live only: with live off nothing is
// rechecked and the brief says so instead. An ATS job is never marked: the brief for day
// leaves one out while its board has not listed it for brief.BoardStaleDays (see
// rankPass), and ranks without it here too. Each round is one streamed pass: a job marked
// leaves room for one below the caps, which the last pass did not keep.
func leaveOutStale(ctx context.Context, q store.Queries, prof candidate.Config, loc *time.Location, day time.Time, allowFake bool, source string) (int, error) {
	marked := 0
	for {
		r, err := rankNow(ctx, q, prof, loc, day, allowFake)
		if err != nil {
			return marked, err
		}
		var ids []int64
		for _, rk := range append(append([]scoring.Ranked{}, r.Apply...), r.YourCall...) {
			ids = append(ids, rk.JobID)
		}
		if r.RunnerUp != nil {
			ids = append(ids, r.RunnerUp.JobID)
		}
		due, err := q.RecheckTargets(ctx, source, ids, time.Now().Add(-linkedin.RecheckAfter))
		if err != nil {
			return marked, err
		}
		n := 0
		for _, t := range due {
			if t.ListingStatus != store.ListingOpen {
				continue // an unreachable job is not listed
			}
			if err := q.SetListingStatus(ctx, t.JobID, store.ListingUnreachable); err != nil {
				return marked, err
			}
			n++
		}
		if n == 0 {
			return marked, nil
		}
		marked += n
	}
}

// recheckBudgets: a recheck is a detail request, so it draws on the detail budget.
func recheckBudgets(cfg config.Config) fetch.Budgets {
	return fetch.Budgets{fetch.KindDetail: cfg.LinkedInDetailBudget}
}

// runRecheck is `leadhound recheck [--allow-fake] [--dry-run --fixtures DIR]`: the
// recheck the brief runs first, on its own. Live fetching off is said and exits 0, as
// does any recorded stop.
func runRecheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("recheck", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	allowFake := fs.Bool("allow-fake", false, "rank and rescore with the fake decider's verdicts (development only)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: leadhound recheck [--allow-fake] [--dry-run --fixtures DIR]")
	}
	if err := ff.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL", "LEADHOUND_DATA_DIR")
	if err != nil {
		return err
	}
	prof, err := loadProfileDir(cfg.DataDir)
	if err != nil {
		return err
	}
	newClient, source, err := linkedinClients(cfg, ff, recheckBudgets(cfg))
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "recheck: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	if err != nil {
		return err
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	out := recheckListed(ctx, s, cfg, prof, loc, time.Now(), *allowFake, newClient, source)
	_, _ = fmt.Fprintln(stdout, out.summary())
	return out.Err
}
