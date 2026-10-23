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

// Names the collector writes into the database.
const (
	Source         = "linkedin"          // run.source, http_fetch.source, job.source
	SourceFixture  = "linkedin-fixture"  // the same, for a dry run served from fixtures
	CommandCollect = "collect"           // run.command
	DerivedBy      = "linkedin-parser@1" // job_observation.derived_by
	pageSize       = 10                  // cards on a full search page
	tprCold        = 604800              // f_TPR on a cold start: 7 days
	tprSteady      = 86400               // f_TPR afterwards: 24 hours
)

// stopReasonParserBroke is run.stop_reason when a page lost its expected markup.
const stopReasonParserBroke = "parser_broke"

// Collector runs one LinkedIn collect: search pages, then details for the new jobs.
type Collector struct {
	Store *store.Store
	// NewClient builds the run's client around the run's recorder. It is called once per
	// run, so every run gets a fresh breaker and budgets and records into its own run row.
	NewClient func(rec fetch.Recorder) *fetch.Client
	Now       func() time.Time // nil: time.Now
	Loc       *time.Location   // LEADHOUND_TZ: what "today" means; nil: UTC
	Log       *slog.Logger     // nil: slog.Default()
	// Source is what the run's rows are written under: Source (the default) for LinkedIn
	// itself, SourceFixture for a dry run, so fixture data never reads as LinkedIn's and
	// never counts toward LinkedIn's cold start, daily canary or pause.
	Source string
	// Gates is the candidate profile the gates read for every observation.
	Gates gates.CandidateGates
}

// Result is what one Run did. It mirrors the run row it wrote.
type Result struct {
	RunID         int64
	Paused        bool // today was already stopped by throttled or blocked; nothing was requested
	ColdStart     bool // no warm collect had run before (store.HasWarmRun): 7-day window, MaxPages pages
	Searches      int  // search pages parsed
	Cards         int  // cards saved, counted per page
	NewIDs        int  // distinct jobs with no observation yet
	Observations  int  // observations written
	DetailMissing int  // new jobs whose detail page was 404
	Requests      int
	Failures      int
	StopReason    string
	Note          string
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) source() string {
	if c.Source != "" {
		return c.Source
	}
	return Source
}

func (c *Collector) loc() *time.Location {
	if c.Loc != nil {
		return c.Loc
	}
	return time.UTC
}

func (c *Collector) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Run collects m. A recorded stop (throttled, blocked, failure_rate, budget, parser_broke)
// is a Result with StopReason set and a nil error; an error means the run itself failed.
func (c *Collector) Run(ctx context.Context, m Matrix) (Result, error) {
	q := c.Store.Q()

	// Stop for the day once LinkedIn has throttled or blocked any command today (LI1).
	reason, paused, err := q.PausedToday(ctx, c.source(), c.loc(), c.now())
	if err != nil {
		return Result{}, fmt.Errorf("linkedin: collect: %w", err)
	}
	if paused {
		id, note, err := RecordPause(ctx, q, CommandCollect, c.source(), reason)
		if err != nil {
			return Result{}, fmt.Errorf("linkedin: collect: %w", err)
		}
		c.log().Info("collect paused", "reason", reason)
		return Result{RunID: id, Paused: true, StopReason: reason, Note: note}, nil
	}

	warm, err := q.HasWarmRun(ctx, CommandCollect, c.source())
	if err != nil {
		return Result{}, fmt.Errorf("linkedin: collect: %w", err)
	}
	runID, err := q.StartRun(ctx, CommandCollect, c.source())
	if err != nil {
		return Result{}, fmt.Errorf("linkedin: collect: %w", err)
	}
	rec := store.NewFetchRecorder(q, runID, c.source())
	r := &collectRun{
		c:       c,
		q:       q,
		rec:     rec,
		client:  c.NewClient(rec),
		res:     Result{RunID: runID, ColdStart: !warm},
		jobIDs:  map[string]int64{},
		company: map[string]string{},
		pending: map[string]bool{},
	}

	runErr := r.collect(ctx, m)
	res := r.res
	res.Requests = r.client.Breaker.Requests()
	res.Failures = r.client.Breaker.Failures()
	res.Note = r.note()
	if runErr != nil {
		res.Note = "failed: " + runErr.Error()
	}
	// Record the outcome even when the caller's context is done.
	finishErr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{
		Requests: res.Requests, Failures: res.Failures, StopReason: res.StopReason, Note: res.Note,
	})
	if runErr != nil {
		return res, errors.Join(fmt.Errorf("linkedin: collect: %w", runErr), finishErr)
	}
	if finishErr != nil {
		return res, fmt.Errorf("linkedin: collect: %w", finishErr)
	}
	return res, nil
}

// RecordPause records a run of command that made no request because source is paused
// today. It carries the pausing stop reason, so it never counts as a warm run, and its note
// says why.
func RecordPause(ctx context.Context, q store.Queries, command, source, reason string) (runID int64, note string, err error) {
	id, err := q.StartRun(ctx, command, source)
	if err != nil {
		return 0, "", err
	}
	note = "paused today: " + reason
	if err := q.FinishRun(ctx, id, store.RunResult{StopReason: reason, Note: note}); err != nil {
		return 0, "", err
	}
	return id, note, nil
}

// collectRun is the state of one run.
type collectRun struct {
	c       *Collector
	q       store.Queries
	rec     *store.FetchRecorder
	client  *fetch.Client
	res     Result
	jobIDs  map[string]int64  // LinkedIn job ID → job.id, for every card saved this run
	company map[string]string // LinkedIn job ID → the card's company name, for company rules
	pending map[string]bool   // new IDs already queued for a detail fetch
	order   []string          // the queue, in the order first seen
	broke   string            // the page the parser broke on, for the note
}

// collect runs the search phase, then the detail phase. It sets res.StopReason for a
// recorded stop and returns an error only when the run itself failed.
func (r *collectRun) collect(ctx context.Context, m Matrix) error {
	stopAll, err := r.searchAll(ctx, m)
	if err != nil || stopAll {
		return err
	}
	return r.details(ctx)
}

// searchAll walks the matrix. It reports stopAll when the run must end before details.
func (r *collectRun) searchAll(ctx context.Context, m Matrix) (stopAll bool, err error) {
	tpr, maxPages := tprSteady, min(max(m.MaxPages, 1), 2)
	if r.res.ColdStart {
		tpr, maxPages = tprCold, max(m.MaxPages, 1)
	}
	for _, kw := range m.Keywords {
		for _, loc := range m.Locations {
			for page := 1; page <= maxPages; page++ {
				more, stop, err := r.searchPage(ctx, SearchURL(kw, loc, tpr, page), page)
				if err != nil {
					return true, err
				}
				switch stop {
				case "":
				case string(fetch.StopBudget):
					// The search budget is spent: keep the details that budget left room for.
					r.res.StopReason = stop
					return false, nil
				default:
					r.res.StopReason = stop
					return true, nil
				}
				if !more {
					break
				}
			}
		}
	}
	return false, nil
}

// searchPage fetches and saves one search page. more says whether the next page of the
// same query is worth fetching; stop is a recorded stop reason.
func (r *collectRun) searchPage(ctx context.Context, u string, page int) (more bool, stop string, err error) {
	resp, err := r.client.Get(ctx, fetch.KindSearch, u)
	ok, stop, err := r.triage(fetch.KindSearch, u, resp, err)
	if !ok {
		return false, stop, err
	}
	if resp.Status == http.StatusNotFound {
		r.c.log().Warn("search page missing", "url", u)
		return false, "", nil
	}
	cards, err := ParseSearch(resp.Body)
	if errors.Is(err, ErrParserBroke) {
		r.broke = "search page " + u
		r.c.log().Error("parser broke", "kind", fetch.KindSearch, "url", u, "err", err)
		return false, stopReasonParserBroke, nil
	}
	if err != nil {
		return false, "", err
	}
	r.res.Searches++
	unseen, err := r.saveCards(ctx, cards)
	if err != nil {
		return false, "", err
	}
	switch {
	case len(cards) < pageSize:
		return false, "", nil
	case r.res.ColdStart:
		return true, "", nil
	default:
		// Steady state: page 2 only when page 1 was full of jobs never seen before.
		return page == 1 && unseen == len(cards), "", nil
	}
}

// triage sorts the result of one Get. ok means the response can be read (a 2xx or a
// 404); otherwise stop is the breaker's reason when it opened, err is set when the run
// must fail, and neither means this one request is skipped (an unexpected status or a
// transport error, already recorded and counted by the breaker; retried next run).
func (r *collectRun) triage(kind fetch.Kind, u string, resp fetch.Response, err error) (ok bool, stop string, _ error) {
	if err == nil {
		return true, "", nil
	}
	var open *fetch.OpenError
	switch {
	case errors.As(err, &open):
		r.c.log().Warn("fetching stopped", "kind", kind, "reason", open.Reason, "url", u)
		return false, string(open.Reason), nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false, "", err
	case errors.Is(err, fetch.ErrStatus), errors.Is(err, fetch.ErrBodyTooLarge),
		resp.Status == 0 && !errors.Is(err, fetch.ErrLiveOff):
		r.c.log().Warn("request skipped", "kind", kind, "status", resp.Status, "url", u, "err", err)
		return false, "", nil
	default:
		return false, "", err
	}
}

// saveCards upserts each card's company and job, queues the new IDs (no observation yet)
// for a detail fetch once each, and returns how many cards were jobs never seen before.
func (r *collectRun) saveCards(ctx context.Context, cards []Card) (unseen int, err error) {
	ids := make([]string, 0, len(cards))
	for _, card := range cards {
		if _, err := r.q.JobBySourceID(ctx, r.c.source(), card.ID); errors.Is(err, store.ErrNotFound) {
			unseen++
		} else if err != nil {
			return 0, err
		}
		companyID, err := r.q.UpsertCompany(ctx, card.Company, card.CompanyURL, card.CompanySlug)
		if errors.Is(err, store.ErrEmptyName) {
			r.c.log().Warn("card skipped: company name has no letters", "job", card.ID)
			continue
		}
		if err != nil {
			return 0, err
		}
		jobID, err := r.q.UpsertJob(ctx, store.JobUpsert{
			Source: r.c.source(), SourceJobID: card.ID, CompanyID: companyID, Title: card.Title, URL: card.URL,
		})
		if err != nil {
			return 0, err
		}
		r.jobIDs[card.ID] = jobID
		r.company[card.ID] = card.Company
		ids = append(ids, card.ID)
		r.res.Cards++
	}
	fresh, err := r.q.JobsWithoutObservation(ctx, r.c.source(), ids)
	if err != nil {
		return 0, err
	}
	for _, id := range fresh {
		if !r.pending[id] {
			r.pending[id] = true
			r.order = append(r.order, id)
		}
	}
	return unseen, nil
}

// details fetches the detail page of every queued job and writes its observation.
func (r *collectRun) details(ctx context.Context) error {
	r.res.NewIDs = len(r.order)
	for _, id := range r.order {
		u := DetailURL(id)
		resp, err := r.client.Get(ctx, fetch.KindDetail, u)
		ok, stop, err := r.triage(fetch.KindDetail, u, resp, err)
		if err != nil {
			return err
		}
		if stop != "" {
			r.res.StopReason = stop
			return nil
		}
		if !ok {
			continue
		}
		if resp.Status == http.StatusNotFound {
			r.res.DetailMissing++
			r.c.log().Info("detail missing", "job", id)
			continue
		}
		d, err := ParseDetail(resp.Body, resp.FetchedAt)
		if errors.Is(err, ErrParserBroke) {
			r.broke = "detail page " + u
			r.res.StopReason = stopReasonParserBroke
			r.c.log().Error("parser broke", "kind", fetch.KindDetail, "url", u, "err", err)
			return nil
		}
		if err != nil {
			return err
		}
		fetchID, found := r.rec.LastIDFor(u)
		if !found {
			return fmt.Errorf("detail %s: no fetch row recorded", id)
		}
		if err := r.c.writeObservation(ctx, r.jobIDs[id], fetchID, r.company[id], d); err != nil {
			return err
		}
		r.res.Observations++
	}
	return nil
}

// note is the run row's note: what the run did, plus where the parser broke.
func (r *collectRun) note() string {
	s := fmt.Sprintf("%d searches, %d cards, %d new, %d observed, %d detail missing",
		r.res.Searches, r.res.Cards, r.res.NewIDs, r.res.Observations, r.res.DetailMissing)
	if r.broke != "" {
		s += "; parser broke on " + r.broke
	}
	return s
}

// writeObservation stores what one detail fetch showed about a new job through
// pipeline.Observe (the sanitised observation and its gate results), with its `new` status
// event written in Observe's own transaction. company is the card's company name, used
// when the detail page shows none.
func (c *Collector) writeObservation(ctx context.Context, jobID, fetchID int64, company string, d Detail) error {
	if d.Company != "" {
		company = d.Company
	}
	_, err := pipeline.Observe(ctx, c.Store, pipeline.Input{
		JobID:           jobID,
		FetchID:         fetchID,
		DerivedBy:       DerivedBy,
		Title:           d.Title,
		Company:         company,
		Location:        d.Location,
		DescriptionHTML: d.Description,
		Seniority:       d.Seniority,
		EmploymentType:  d.EmploymentType,
		Industries:      d.Industries,
		ApplicantCount:  d.ApplicantCount,
		ApplicantText:   d.ApplicantText,
		PostedText:      d.PostedText,
		PostedAtEst:     d.PostedAtEst,
		ApplyType:       d.ApplyType,
		Closed:          d.Closed,
		Gates:           c.Gates,
	}, func(q store.Queries, _ pipeline.ObserveResult) error {
		_, err := q.InsertStatusEvent(ctx, jobID, "new", "", "system", nil)
		return err
	})
	return err
}
