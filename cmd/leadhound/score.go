package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["score"] = runScore }

// demoProfileFile is the fictional profile examples/ ships; a directory without a
// leadhound.yaml is read through it, so the demo runs as it is.
const demoProfileFile = "leadhound.demo.yaml"

// loadProfileDir reads leadhound.yaml in dir, or the demo profile when that is all the
// directory holds. A broken or missing profile is a usage error.
func loadProfileDir(dir string) (candidate.Config, error) {
	path := filepath.Join(dir, candidate.FileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(filepath.Join(dir, demoProfileFile)); err == nil {
			path = filepath.Join(dir, demoProfileFile)
		}
	}
	prof, err := candidate.LoadFile(path)
	if err != nil {
		return candidate.Config{}, usageError("%v", err)
	}
	return prof, nil
}

// fakeDecider is the keyless stand-in: every criterion passes at 0.9, so a dry run takes
// a kept title to the Apply tier, and the title-only question (asked only about a title
// the screen is unsure of) always fails at 0.9, so a dry run shows the title check
// removing those jobs before full scoring.
func fakeDecider() *ai.Fake {
	ans := ai.Answer{Choice: scoring.Pass, Confidence: 0.9,
		Probabilities: map[string]float64{scoring.Pass: 0.9, scoring.Fail: 0.05, scoring.Unclear: 0.05}}
	m := make(map[string]ai.Answer, len(candidate.CriterionNames)+1)
	for _, n := range candidate.CriterionNames {
		m[n] = ans
	}
	m[scoring.TitleCheckName] = ai.Answer{Choice: scoring.Fail, Confidence: 0.9,
		Probabilities: map[string]float64{scoring.Fail: 0.9, scoring.Pass: 0.05, scoring.Unclear: 0.05}}
	return &ai.Fake{Answers: m}
}

// pickDecider resolves --decider: jev needs TYPESAFE_API_KEY, and fake is the scripted
// stand-in (every criterion passes). With no flag, score uses jev and refuses to run
// without a key, and eval uses the fake: neither command falls back to the fake on its
// own, and only an explicit --decider jev makes eval pay.
func pickDecider(cfg config.Config, name, command string) (ai.Decider, error) {
	if name == "" {
		name = "jev"
		if command == "eval" {
			name = "fake"
		} else if cfg.TypesafeAPIKey == "" {
			return nil, usageError("%s: set TYPESAFE_API_KEY or pass --decider fake", command)
		}
	}
	switch name {
	case "jev":
		if cfg.TypesafeAPIKey == "" {
			return nil, usageError("--decider jev needs TYPESAFE_API_KEY")
		}
		return ai.NewJev(cfg.TypesafeAPIKey, cfg.JevModel), nil
	case "fake":
		return fakeDecider(), nil
	}
	return nil, usageError("--decider must be jev or fake, not %q", name)
}

// runScore is `leadhound score [--decider jev|fake] [--max-duration D] [--check-config]`. A
// run stopped by the daily cap, an unavailable provider or its time budget is recorded and
// exits 0; with another score run in progress nothing is recorded and it exits 0 too. The
// ranking summary reads the fake decider's decisions only when this run used the fake.
func runScore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("score", flag.ContinueOnError)
	deciderName := fs.String("decider", "", "jev or fake (default jev, which needs TYPESAFE_API_KEY)")
	checkConfig := fs.Bool("check-config", false, "load the profile, print its counts and exit")
	maxDuration := fs.Duration("max-duration", 0, "stop before the next job once this long has passed (0: no limit)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usageError("usage: leadhound score [--decider jev|fake] [--max-duration D] [--check-config]")
	}
	if *maxDuration < 0 {
		return usageError("--max-duration must not be negative, not %v", *maxDuration)
	}
	var deadline time.Time
	if *maxDuration > 0 {
		deadline = time.Now().Add(*maxDuration)
	}
	if *checkConfig {
		cfg, err := loadConfig("LEADHOUND_DATA_DIR")
		if err != nil {
			return err
		}
		prof, err := loadProfileDir(cfg.DataDir)
		if err != nil {
			return err
		}
		printProfileCounts(prof)
		return nil
	}

	cfg, err := loadConfig("LEADHOUND_DATABASE_URL", "LEADHOUND_DATA_DIR")
	if err != nil {
		return err
	}
	prof, err := loadProfileDir(cfg.DataDir)
	if err != nil {
		return err
	}
	decider, err := pickDecider(cfg, *deciderName, "score")
	if err != nil {
		return err
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	runID, res, awaiting, err := scoreRunCounted(ctx, s, cfg, prof, decider, loc, deadline)
	if errors.Is(err, errScoreBusy) {
		_, _ = fmt.Fprintln(stdout, "score: another score run is in progress; skipped")
		return nil
	}
	if err != nil {
		return err
	}
	note := scoreNote(res, awaiting)
	stop := res.StopReason
	if stop == "" {
		stop = "none"
	}
	_, _ = fmt.Fprintf(stdout, "score: run %d · decider %s · %s · stop %s\n", runID, decider.Name(), note, stop)

	r, err := scoreSummary(ctx, s, prof, loc, decider.Name() == ai.FakeName)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "  ranked: apply %d · your call %d · skipped %s\n", r.ApplyTotal, r.YourCallTotal, skippedSummary(r.Skipped))
	if r.RunnerUp != nil {
		_, _ = fmt.Fprintf(stdout, "  runner-up: job %d (failed %s)\n", r.RunnerUp.JobID, r.RunnerUp.FailedCriterion)
	}
	return nil
}

// scoreSummary ranks the whole store for the score run's summary lines, in one streamed
// pass over every job (none set apart as not seen on its board) that reads every
// description while a description rule is active, so the skip split it prints is exact.
func scoreSummary(ctx context.Context, s *store.Store, prof candidate.Config, loc *time.Location, allowFake bool) (scoring.Ranking, error) {
	pass, err := rankPass(ctx, s.Q(), loc, time.Now(), allowFake, rankOptions{caps: briefCaps(prof), desc: store.DescAll, keepNotSeen: true})
	return pass.ranking, err
}

// scoreRun scores every job worth scoring as one recorded score run, behind the daily cap,
// until deadline (zero: none). A run stopped by the cap, an unavailable provider or the
// deadline is a result, not an error; with another score run in progress it records nothing
// and returns errScoreBusy.
func scoreRun(ctx context.Context, s *store.Store, cfg config.Config, prof candidate.Config, decider ai.Decider, loc *time.Location, deadline time.Time) (int64, scoring.Result, error) {
	runID, res, _, err := scoreRunCounted(ctx, s, cfg, prof, decider, loc, deadline)
	return runID, res, err
}

// scoreLockKey is the session advisory lock a score run holds for its length, so only one
// scorer runs at a time across processes (the score timer and the brief's rescore).
const scoreLockKey int64 = 7311041902

// errScoreBusy: another score run holds the lock.
var errScoreBusy = errors.New("another score run is in progress")

// withScoreLock runs fn holding the score lock on a connection acquired for it alone, and
// returns errScoreBusy without running fn when another process holds it. The lock is
// released on that connection; a connection that cannot release it is closed, which
// releases it too, and never goes back to the pool.
func withScoreLock(ctx context.Context, s *store.Store, fn func() error) (err error) {
	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("score lock: acquire: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, scoreLockKey).Scan(&got); err != nil {
		conn.Release()
		return fmt.Errorf("score lock: %w", err)
	}
	if !got {
		conn.Release()
		return errScoreBusy
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, uerr := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, scoreLockKey); uerr != nil {
			_ = conn.Conn().Close(uctx)
		}
		conn.Release()
	}()
	return fn()
}

// scoreNote is the score run's summary: the scorer's note and the jobs held back until
// their detail arrives.
func scoreNote(res scoring.Result, awaitingDetail int) string {
	return fmt.Sprintf("%s · awaiting detail %d", res.Note(), awaitingDetail)
}

// queueCounter is the scorer's store, keeping the count of jobs the queue held back until
// their detail arrives (store.ScoreQueue.AwaitingDetail).
type queueCounter struct {
	scoring.StoreAdapter
	awaitingDetail int
}

// ScoreQueue reads the queue and keeps its awaiting-detail count.
func (c *queueCounter) ScoreQueue(ctx context.Context) (store.ScoreQueue, error) {
	q, err := c.StoreAdapter.ScoreQueue(ctx)
	c.awaitingDetail = q.AwaitingDetail
	return q, err
}

// scoreRunCounted is scoreRun, also returning how many jobs were held back until their
// detail arrives (also in the run's note).
func scoreRunCounted(ctx context.Context, s *store.Store, cfg config.Config, prof candidate.Config, decider ai.Decider, loc *time.Location, deadline time.Time) (runID int64, res scoring.Result, awaiting int, err error) {
	err = withScoreLock(ctx, s, func() error {
		var rerr error
		runID, res, awaiting, rerr = scoreRunLocked(ctx, s, cfg, prof, decider, loc, deadline)
		return rerr
	})
	return runID, res, awaiting, err
}

// scoreRunLocked is scoreRunCounted with the score lock held.
func scoreRunLocked(ctx context.Context, s *store.Store, cfg config.Config, prof candidate.Config, decider ai.Decider, loc *time.Location, deadline time.Time) (int64, scoring.Result, int, error) {
	q := s.Q()
	remaining, err := remainingCalls(ctx, q, decider, cfg.JevDailyCallCap, loc)
	if err != nil {
		return 0, scoring.Result{}, 0, err
	}
	runID, err := q.StartRun(ctx, "score", "")
	if err != nil {
		return 0, scoring.Result{}, 0, err
	}
	qc := &queueCounter{StoreAdapter: scoring.StoreAdapter{S: s}}
	sc := &scoring.Scorer{
		Store:            qc,
		Decider:          &ai.Capped{Decider: decider, Store: q, DailyCalls: cfg.JevDailyCallCap, Loc: loc, Purpose: ai.PurposeJobs},
		Criteria:         prof.Candidate.Criteria.List(),
		EvidenceKeywords: prof.Candidate.EvidenceKeywords,
		Threshold:        prof.ConfidenceThreshold,
		TitleCheck:       prof.TitleCheck(),
		Batch:            scoring.BatchFor(remaining),
		Deadline:         deadline,
	}
	res, runErr := sc.Run(ctx)
	note := scoreNote(res, qc.awaitingDetail)
	if runErr != nil {
		note += scoreErrorSuffix
	}
	// Record the run even when Run failed; a cancelled context still gets its row closed.
	finishErr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{
		Requests: res.Calls, Failures: res.Failures, StopReason: res.StopReason, Note: note})
	if runErr != nil {
		return runID, res, qc.awaitingDetail, runErr
	}
	return runID, res, qc.awaitingDetail, finishErr
}

// remainingCalls is how many calls the daily cap still allows today, counted as ai.Capped
// counts them: per provider, jobs calls only, on the calendar day in loc. It only sizes the batches; the
// cap itself still refuses every call past it.
func remainingCalls(ctx context.Context, q store.Queries, d ai.Decider, daily int, loc *time.Location) (int, error) {
	provider := d.Name()
	if p, ok := d.(interface{ Provider() string }); ok {
		provider = p.Provider()
	}
	now := time.Now().In(loc)
	used, err := q.AICallsOn(ctx, provider, ai.PurposeJobs, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc))
	if err != nil {
		return 0, err
	}
	return max(daily-used, 0), nil
}

// skippedSummary lists the skip counts in precedence order, e.g. "language 6, criterion 1",
// or "none".
func skippedSummary(m map[string]int) string {
	var parts []string
	for _, k := range scoring.SkipReasons {
		if m[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// printProfileCounts prints counts only, never the profile's words: they are private.
func printProfileCounts(prof candidate.Config) {
	kw := 0
	for _, list := range prof.Candidate.EvidenceKeywords {
		kw += len(list)
	}
	_, _ = fmt.Fprintf(stdout, "profile ok: criteria %d · evidence keywords %d (for %d criteria) · search keywords %d · locations %d\n",
		len(prof.Candidate.Criteria.List()), kw, len(prof.Candidate.EvidenceKeywords), len(prof.LinkedIn.Keywords), len(prof.LinkedIn.Locations))
}
