package linkedin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/pipeline"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// CommandRecheck is run.command for the recheck.
const CommandRecheck = "recheck"

// RecheckAfter is how old a listing's last check may be before the brief lists it again
// without a fresh look (LI4).
const RecheckAfter = 20 * time.Hour

// ClosedReason is the status_event reason a recheck writes when it withdraws a job.
const ClosedReason = "closed on LinkedIn"

// Rechecker re-fetches the detail page of jobs the brief is about to list, so a listing
// that closed since it was collected is not sent. Its client must draw on the detail
// budget: a recheck is a detail request.
//
// Per job: a page with the closed banner sets listing_status expired (and, while the job
// is new or shortlisted, writes status_event withdrawn/system); a readable open page sets
// it open and writes a new observation through pipeline.Observe; anything else (no answer,
// another status, a 404, the breaker open, the source paused today, the parser broke) sets
// it unreachable, so it is left out today and retried next time. Nothing but the banner
// ever marks a listing closed.
type Rechecker struct {
	Store     *store.Store
	NewClient func(rec fetch.Recorder) *fetch.Client // called once per run
	Now       func() time.Time                       // nil: time.Now
	Loc       *time.Location                         // LEADHOUND_TZ; nil: UTC
	Log       *slog.Logger                           // nil: slog.Default()
	Source    string                                 // as Collector.Source; "": Source
	// Gates is the candidate profile the gates read for every new observation.
	Gates gates.CandidateGates
	// After is RecheckAfter unless set.
	After time.Duration
}

// RecheckResult is what one Run did.
type RecheckResult struct {
	RunID       int64 // 0 when nothing was due
	Due         int   // jobs due a recheck among those asked about
	Paused      bool  // the source is paused today; nothing was requested
	Open        int
	Expired     int
	Unreachable int
	Withdrawn   int     // status events written for expired jobs
	Observed    []int64 // jobs with a new observation, for the scorer
	Requests    int
	Failures    int
	StopReason  string
	Note        string
}

// Run rechecks the jobs among jobIDs that are due (store.RecheckTargets): an unreachable
// listing always, an open one last checked more than After ago. With none due it records
// nothing. A recorded stop is a result with StopReason set and a nil error.
func (r *Rechecker) Run(ctx context.Context, jobIDs []int64) (RecheckResult, error) {
	col := Collector{Now: r.Now, Loc: r.Loc, Log: r.Log, Source: r.Source} // for its defaults
	q := r.Store.Q()
	after := r.After
	if after <= 0 {
		after = RecheckAfter
	}
	targets, err := q.RecheckTargets(ctx, col.source(), jobIDs, col.now().Add(-after))
	if err != nil {
		return RecheckResult{}, fmt.Errorf("linkedin: recheck: %w", err)
	}
	res := RecheckResult{Due: len(targets)}
	if len(targets) == 0 {
		return res, nil
	}

	reason, paused, err := q.PausedToday(ctx, col.source(), col.loc(), col.now())
	if err != nil {
		return res, fmt.Errorf("linkedin: recheck: %w", err)
	}
	if paused {
		id, note, err := RecordPause(ctx, q, CommandRecheck, col.source(), reason)
		if err != nil {
			return res, fmt.Errorf("linkedin: recheck: %w", err)
		}
		res.RunID, res.Paused, res.StopReason = id, true, reason
		for _, t := range targets {
			if err := q.SetListingStatus(ctx, t.JobID, store.ListingUnreachable); err != nil {
				return res, fmt.Errorf("linkedin: recheck: %w", err)
			}
			res.Unreachable++
		}
		res.Note = fmt.Sprintf("%s; %d left out until the next check", note, res.Unreachable)
		col.log().Info("recheck paused", "reason", reason)
		return res, nil
	}

	runID, err := q.StartRun(ctx, CommandRecheck, col.source())
	if err != nil {
		return res, fmt.Errorf("linkedin: recheck: %w", err)
	}
	res.RunID = runID
	rec := store.NewFetchRecorder(q, runID, col.source())
	client := r.NewClient(rec)

	var runErr error
	for _, t := range targets {
		if runErr = ctx.Err(); runErr != nil {
			break
		}
		if res.StopReason != "" {
			// Once fetching has stopped, the rest are unverified today.
			runErr = r.unreachable(ctx, q, t, &res)
		} else {
			runErr = r.check(ctx, q, client, rec, t, &res)
		}
		if runErr != nil {
			break
		}
	}
	res.Requests = client.Breaker.Requests()
	res.Failures = client.Breaker.Failures()
	res.Note = fmt.Sprintf("%d due, %d open, %d expired, %d unreachable", res.Due, res.Open, res.Expired, res.Unreachable)
	if runErr != nil {
		res.Note = "failed: " + runErr.Error()
	}
	finishErr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{
		Requests: res.Requests, Failures: res.Failures, StopReason: res.StopReason, Note: res.Note,
	})
	if runErr != nil {
		return res, errors.Join(fmt.Errorf("linkedin: recheck: %w", runErr), finishErr)
	}
	if finishErr != nil {
		return res, fmt.Errorf("linkedin: recheck: %w", finishErr)
	}
	return res, nil
}

// check fetches one job's detail page and records what it shows.
func (r *Rechecker) check(ctx context.Context, q store.Queries, client *fetch.Client, rec *store.FetchRecorder, t store.RecheckTarget, res *RecheckResult) error {
	u := DetailURL(t.SourceJobID)
	resp, err := client.Get(ctx, fetch.KindDetail, u)
	var open *fetch.OpenError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &open):
		res.StopReason = string(open.Reason)
		r.log().Warn("recheck stopped", "reason", open.Reason, "job", t.JobID)
		return r.unreachable(ctx, q, t, res)
	case err != nil, resp.Status != http.StatusOK:
		r.log().Warn("recheck got no page", "job", t.JobID, "status", resp.Status, "err", err)
		return r.unreachable(ctx, q, t, res)
	}
	d, err := ParseDetail(resp.Body, resp.FetchedAt)
	if err != nil {
		if errors.Is(err, ErrParserBroke) {
			res.StopReason = stopReasonParserBroke
			r.log().Error("parser broke", "kind", fetch.KindDetail, "url", u, "err", err)
		}
		return r.unreachable(ctx, q, t, res)
	}
	fetchID, found := rec.LastIDFor(u)
	if !found {
		return fmt.Errorf("recheck job %d: no fetch row recorded", t.JobID)
	}
	withdrawn := false
	_, err = pipeline.Observe(ctx, r.Store, observeInput(t.JobID, fetchID, t.Company, d, r.Gates), func(tq store.Queries, _ pipeline.ObserveResult) error {
		if !d.Closed {
			return tq.SetListingStatus(ctx, t.JobID, store.ListingOpen)
		}
		if err := tq.SetListingStatus(ctx, t.JobID, store.ListingExpired); err != nil {
			return err
		}
		// Only a job not yet acted on is withdrawn; applied and later keep their status.
		status, err := tq.CurrentStatus(ctx, t.JobID)
		if err != nil {
			return err
		}
		if status != "new" && status != "shortlisted" {
			return nil
		}
		withdrawn = true
		_, err = tq.InsertStatusEvent(ctx, t.JobID, "withdrawn", ClosedReason, "system", nil)
		return err
	})
	if err != nil {
		return fmt.Errorf("recheck job %d: %w", t.JobID, err)
	}
	if d.Closed {
		res.Expired++
		if withdrawn {
			res.Withdrawn++
		}
		return nil
	}
	res.Open++
	res.Observed = append(res.Observed, t.JobID)
	return nil
}

func (r *Rechecker) unreachable(ctx context.Context, q store.Queries, t store.RecheckTarget, res *RecheckResult) error {
	if err := q.SetListingStatus(ctx, t.JobID, store.ListingUnreachable); err != nil {
		return err
	}
	res.Unreachable++
	return nil
}

func (r *Rechecker) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}
