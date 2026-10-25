package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["trim"] = runTrim }

// The trim's defaults: a posting rejected a week ago or expired a month ago loses its text,
// 5,000 observations a batch, and the nightly unit's 45-minute budget.
const (
	defaultTrimRejectedAfter = 7 * 24 * time.Hour
	defaultTrimExpiredAfter  = 30 * 24 * time.Hour
	defaultTrimBatch         = 5000
	defaultTrimMaxDuration   = 45 * time.Minute
)

// trimPause is how long the trim waits between batches, so it yields to the server's other
// work between its short transactions.
const trimPause = 200 * time.Millisecond

// errTrimBudget is the cause the trim's context ends with when --max-duration passes, which
// tells the budget apart from a cancelled run.
var errTrimBudget = errors.New("trim: time budget reached")

const trimUsage = "usage: leadhound trim [--rejected-after D] [--expired-after D] [--batch N] [--max-duration D]"

// runTrim is `leadhound trim [--rejected-after 168h] [--expired-after 720h] [--batch 5000]
// [--max-duration 45m]`: as the app role, it blanks the description text of old rejected and
// old expired postings through the owner's function (store.TrimDescriptions), never a
// protected job's, and records a trim run. Reaching --max-duration is a clean stop (stop
// reason budget, exit 0): what was trimmed stays trimmed and the next run carries on with
// the rest. Any other error fails the run, with the error in its note.
func runTrim(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("trim", flag.ContinueOnError)
	rejectedAfter := fs.Duration("rejected-after", defaultTrimRejectedAfter, "trim a rejected posting observed longer ago than this")
	expiredAfter := fs.Duration("expired-after", defaultTrimExpiredAfter, "trim an expired posting last checked longer ago than this")
	batch := fs.Int("batch", defaultTrimBatch, "observations scanned per batch")
	maxDuration := fs.Duration("max-duration", defaultTrimMaxDuration, "stop once this long has passed (0: no limit)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) > 0:
		return usageError(trimUsage)
	case *rejectedAfter <= 0:
		return usageError("--rejected-after must be positive, not %v", *rejectedAfter)
	case *expiredAfter <= 0:
		return usageError("--expired-after must be positive, not %v", *expiredAfter)
	case *batch <= 0:
		return usageError("--batch must be positive, not %d", *batch)
	case *maxDuration < 0:
		return usageError("--max-duration must not be negative, not %v", *maxDuration)
	}

	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()
	runID, err := q.StartRun(ctx, "trim", "")
	if err != nil {
		return err
	}

	start := time.Now()
	tctx := ctx
	if *maxDuration > 0 {
		var cancel context.CancelFunc
		tctx, cancel = context.WithTimeoutCause(ctx, *maxDuration, errTrimBudget)
		defer cancel()
	}
	pause := func() {
		t := time.NewTimer(trimPause)
		defer t.Stop()
		select {
		case <-t.C:
		case <-tctx.Done():
		}
	}
	opt := store.TrimOptions{RejectedAfter: *rejectedAfter, ExpiredAfter: *expiredAfter, Batch: *batch}
	res, err := q.TrimDescriptions(tctx, opt, pause)
	took := time.Since(start).Round(time.Millisecond)
	done := fmt.Sprintf("rejected %d · expired %d · batches %d", res.Rejected, res.Expired, res.Batches)

	// The run row is finished even when ctx was cancelled, so a stopped run never stays open.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer fcancel()
	budget := err != nil && ctx.Err() == nil && errors.Is(context.Cause(tctx), errTrimBudget)
	switch {
	case budget:
		note := fmt.Sprintf("stopped at the %v budget after %s; the next run carries on", *maxDuration, done)
		if ferr := q.FinishRun(fctx, runID, store.RunResult{StopReason: "budget", Note: note}); ferr != nil {
			return ferr
		}
		_, _ = fmt.Fprintf(stdout, "trim: %s · %v\n", done, took)
		_, _ = fmt.Fprintf(stdout, "trim: stopped at the %v budget; the next run carries on\n", *maxDuration)
		return nil
	case err != nil:
		_ = q.FinishRun(fctx, runID, store.RunResult{Note: fmt.Sprintf("failed after %s: %v", done, err)})
		return err
	}
	if err := q.FinishRun(fctx, runID, store.RunResult{Note: done}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "trim: %s · %v\n", done, took)
	return nil
}
