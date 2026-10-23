package ats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/pipeline"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// CommandCollect is run.command for an ATS collect.
const CommandCollect = "collect"

// stopReasonParserBroke is run.stop_reason when every board of an ATS lost its expected shape.
const stopReasonParserBroke = "parser_broke"

// applyTypeATS is job_observation.apply_type for a posting applied to on the employer's own
// ATS page.
const applyTypeATS = "ats"

// atsOrder is the order the ATSs run in, whatever order the board list names them.
var atsOrder = []string{Greenhouse, Lever, Ashby}

// Collector runs one ATS collect: every board once, one run row per ATS.
type Collector struct {
	Store *store.Store
	// NewClient builds one ATS's client around that run's recorder. It is called once per ATS
	// per run, so each ATS gets its own breaker and budgets (one source's trouble never stops
	// another) and records into its own run row.
	NewClient func(atsName string, rec fetch.Recorder) *fetch.Client
	Loc       *time.Location // LEADHOUND_TZ: what "today" means for a pause; nil: UTC
	// Fixture writes every row under SourceFixture(ats) instead of the ATS's name, so a dry
	// run never reads as the ATS's data, never pauses it and never expires its jobs.
	Fixture bool
	// Gates is the candidate profile the gates read for every observation.
	Gates gates.CandidateGates
	Now   func() time.Time // nil: time.Now
	Log   *slog.Logger     // nil: slog.Default()
}

// Result is what one Run did: one entry per ATS that had boards, in run order.
type Result struct {
	ATS []ATSResult
}

// RunIDs are the run rows the collect wrote, one per ATS.
func (r Result) RunIDs() []int64 {
	ids := make([]int64, 0, len(r.ATS))
	for _, a := range r.ATS {
		ids = append(ids, a.RunID)
	}
	return ids
}

// Note is every ATS's note, one line each.
func (r Result) Note() string {
	lines := make([]string, 0, len(r.ATS))
	for _, a := range r.ATS {
		lines = append(lines, a.Note)
	}
	return strings.Join(lines, "\n")
}

// ATSResult is what one ATS's run did. It mirrors the run row it wrote.
type ATSResult struct {
	ATS          string
	Source       string // run.source: the ATS's name, or SourceFixture of it in a dry run
	RunID        int64
	Paused       bool // the ATS was already throttled or blocked today; nothing was requested
	Boards       int  // distinct boards in the list for this ATS
	BoardsOK     int  // fetched and parsed
	BoardsFailed int  // a failed request: an unexpected status, a network error or the breaker
	BoardsBroke  int  // answered, but the parser could not read it
	NotFound     int  // answered 404: the board does not exist
	Postings     int  // postings saved from the boards that parsed
	NewJobs      int  // postings with no observation yet
	Observations int  // observations written
	Expired      int64
	Reopened     int64
	Requests     int
	Failures     int
	StopReason   string
	Note         string

	broke    []string // boards the parser broke on, for the note
	notFound []string // boards that answered 404, for the note
	emptied  []string // boards that listed nothing while they had open jobs, one note line each
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
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

// source is what an ATS's rows are written under.
func (c *Collector) source(atsName string) string {
	if c.Fixture {
		return SourceFixture(atsName)
	}
	return atsName
}

// Run collects every board, grouped by ATS: each ATS present gets its own run row, client
// and breaker, and runs even when another ATS stopped or failed. A recorded stop is an
// ATSResult with StopReason set; an error means at least one ATS's run itself failed, and
// it is returned after every ATS has run, joined with the others.
func (c *Collector) Run(ctx context.Context, boards []Board) (Result, error) {
	groups := map[string][]Board{}
	seen := map[string]bool{}
	for _, b := range boards {
		if err := b.Validate(); err != nil {
			return Result{}, fmt.Errorf("ats: collect: %w", err)
		}
		if seen[b.String()] {
			continue // each board is fetched once per run
		}
		seen[b.String()] = true
		groups[b.ATS] = append(groups[b.ATS], b)
	}
	var res Result
	var errs []error
	for _, name := range atsOrder {
		if len(groups[name]) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("ats: collect %s: %w", name, err))
			break
		}
		r, err := c.runATS(ctx, name, groups[name])
		res.ATS = append(res.ATS, r)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// runATS is one ATS's run: its pause check, run row, client and boards.
func (c *Collector) runATS(ctx context.Context, name string, boards []Board) (ATSResult, error) {
	q := c.Store.Q()
	src := c.source(name)
	res := ATSResult{ATS: name, Source: src, Boards: len(boards)}

	// An ATS that throttled or blocked us today is left alone until tomorrow.
	reason, paused, err := q.PausedToday(ctx, src, c.loc(), c.now())
	if err != nil {
		return res, fmt.Errorf("ats: collect %s: %w", name, err)
	}
	if paused {
		id, err := q.StartRun(ctx, CommandCollect, src)
		if err != nil {
			return res, fmt.Errorf("ats: collect %s: %w", name, err)
		}
		res.RunID, res.Paused, res.StopReason = id, true, reason
		res.Note = name + ": paused today: " + reason
		if err := q.FinishRun(ctx, id, store.RunResult{StopReason: reason, Note: res.Note}); err != nil {
			return res, fmt.Errorf("ats: collect %s: %w", name, err)
		}
		c.log().Info("collect paused", "ats", name, "reason", reason)
		return res, nil
	}

	runID, err := q.StartRun(ctx, CommandCollect, src)
	if err != nil {
		return res, fmt.Errorf("ats: collect %s: %w", name, err)
	}
	res.RunID = runID
	rec := store.NewFetchRecorder(q, runID, src)
	r := &atsRun{c: c, q: q, rec: rec, client: c.NewClient(name, rec), src: src, res: &res}

	runErr := r.collect(ctx, boards)
	res.Requests = r.client.Breaker.Requests()
	res.Failures = r.client.Breaker.Failures()
	if res.StopReason == "" && res.BoardsBroke > 0 && res.BoardsBroke == res.Boards {
		res.StopReason = stopReasonParserBroke
	}
	res.Note = res.note()
	if runErr != nil {
		res.Note = name + ": failed: " + runErr.Error()
	}
	// Record the outcome even when the caller's context is done.
	finishErr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{
		Requests: res.Requests, Failures: res.Failures, StopReason: res.StopReason, Note: res.Note,
	})
	if runErr != nil {
		return res, errors.Join(fmt.Errorf("ats: collect %s: %w", name, runErr), finishErr)
	}
	if finishErr != nil {
		return res, fmt.Errorf("ats: collect %s: %w", name, finishErr)
	}
	return res, nil
}

// note is the run row's note: one line saying what the run did.
func (a *ATSResult) note() string {
	s := fmt.Sprintf("%s: %d boards (%d ok, %d failed, %d parser broke, %d not found), %d postings, %d new, %d observed, %d expired, %d reopened",
		a.ATS, a.Boards, a.BoardsOK, a.BoardsFailed, a.BoardsBroke, a.NotFound,
		a.Postings, a.NewJobs, a.Observations, a.Expired, a.Reopened)
	if a.StopReason != "" && a.StopReason != stopReasonParserBroke {
		s += "; stopped: " + a.StopReason
	}
	if len(a.notFound) > 0 {
		s += "; board not found: " + strings.Join(a.notFound, ", ")
	}
	if len(a.broke) > 0 {
		s += "; parser broke on " + strings.Join(a.broke, ", ")
	}
	for _, line := range a.emptied {
		s += "\n" + line
	}
	return s
}

// atsRun is the state of one ATS's run.
type atsRun struct {
	c      *Collector
	q      store.Queries
	rec    *store.FetchRecorder
	client *fetch.Client
	src    string
	res    *ATSResult
}

// collect fetches each board once. It sets res.StopReason when the breaker opens, which
// ends this ATS's run, and returns an error only when the run itself failed.
func (r *atsRun) collect(ctx context.Context, boards []Board) error {
	for i, b := range boards {
		stop, err := r.board(ctx, b)
		if err != nil {
			return err
		}
		if stop != "" {
			r.res.StopReason = stop
			r.res.BoardsFailed += len(boards) - i - 1 // never requested
			return nil
		}
	}
	return nil
}

// board fetches, parses and saves one board. stop is the breaker's reason when it opened.
func (r *atsRun) board(ctx context.Context, b Board) (stop string, _ error) {
	u := b.APIURL()
	resp, err := r.client.Get(ctx, fetch.KindBoard, u)
	if err != nil {
		r.res.BoardsFailed++
		return r.triage(b, resp, err)
	}
	if resp.Status == http.StatusNotFound {
		r.res.NotFound++
		r.res.notFound = append(r.res.notFound, b.String())
		r.c.log().Warn("board not found", "board", b.String())
		return "", nil
	}
	postings, err := Parse(b, resp.Body)
	if err != nil {
		// A board that cannot be read says nothing about which postings are gone.
		r.res.BoardsBroke++
		r.res.broke = append(r.res.broke, b.String())
		r.c.log().Error("parser broke", "board", b.String(), "err", err, "parser_broke", errors.Is(err, ErrParserBroke))
		return "", nil
	}
	fetchID, found := r.rec.LastIDFor(u)
	if !found {
		return "", fmt.Errorf("board %s: no fetch row recorded", b)
	}
	if err := r.save(ctx, b, fetchID, postings); err != nil {
		return "", fmt.Errorf("board %s: %w", b, err)
	}
	r.res.BoardsOK++
	return "", nil
}

// triage sorts a failed Get: the breaker's reason when it opened, an error when the run
// must fail, and neither when only this board is skipped (an unexpected status or a
// transport error, already recorded and counted by the breaker; tried again next run).
func (r *atsRun) triage(b Board, resp fetch.Response, err error) (stop string, _ error) {
	var open *fetch.OpenError
	switch {
	case errors.As(err, &open):
		r.c.log().Warn("fetching stopped", "ats", b.ATS, "reason", open.Reason, "board", b.String())
		return string(open.Reason), nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", err
	case errors.Is(err, fetch.ErrStatus), errors.Is(err, fetch.ErrBodyTooLarge),
		resp.Status == 0 && !errors.Is(err, fetch.ErrLiveOff):
		r.c.log().Warn("board skipped", "board", b.String(), "status", resp.Status, "err", err)
		return "", nil
	default:
		return "", err
	}
}

// save stores one parsed board: each posting's company and job, an observation for every
// job that has none, reopens the listed jobs that had expired, and expires this board's
// open jobs the board no longer lists.
func (r *atsRun) save(ctx context.Context, b Board, fetchID int64, postings []Posting) error {
	ids := make([]string, 0, len(postings))
	jobIDs := map[string]int64{}
	byID := map[string]Posting{}
	for _, p := range postings {
		sid := SourceJobID(b, p)
		if _, dup := jobIDs[sid]; dup {
			continue
		}
		company := firstNonEmpty(p.Company, b.Name, b.Board)
		companyID, err := r.q.UpsertCompany(ctx, company, "", "")
		if errors.Is(err, store.ErrEmptyName) {
			r.c.log().Warn("posting skipped: company name has no letters", "board", b.String(), "job", sid)
			continue
		}
		if err != nil {
			return err
		}
		jobID, err := r.q.UpsertJob(ctx, store.JobUpsert{
			Source: r.src, SourceJobID: sid, CompanyID: companyID, Title: p.Title, URL: p.URL,
		})
		if err != nil {
			return err
		}
		p.Company = company
		jobIDs[sid], byID[sid] = jobID, p
		ids = append(ids, sid)
	}
	r.res.Postings += len(ids)

	fresh, err := r.q.JobsWithoutObservation(ctx, r.src, ids)
	if err != nil {
		return err
	}
	r.res.NewJobs += len(fresh)
	for _, sid := range fresh {
		if err := r.c.writeObservation(ctx, b.ATS, jobIDs[sid], fetchID, byID[sid]); err != nil {
			return err
		}
		r.res.Observations++
	}

	reopened, err := r.q.Reopen(ctx, r.src, ids)
	if err != nil {
		return err
	}
	r.res.Reopened += reopened
	// Every job the board still lists was checked just now.
	if _, err := r.q.MarkChecked(ctx, r.src, ids); err != nil {
		return err
	}
	expired, err := r.q.ExpireMissing(ctx, r.src, boardPrefix(b), ids)
	if err != nil {
		return err
	}
	r.res.Expired += expired
	if len(postings) == 0 && expired > 0 {
		// A board that went from open jobs to none may be a renamed slug: say so.
		r.res.emptied = append(r.res.emptied, fmt.Sprintf("%s: listed 0 postings, %d expired", b, expired))
	}
	return nil
}

// writeObservation stores what one board fetch showed about a new job through
// pipeline.Observe (the sanitised observation and its gate results), with its `new` status
// event written in Observe's own transaction.
func (c *Collector) writeObservation(ctx context.Context, atsName string, jobID, fetchID int64, p Posting) error {
	_, err := pipeline.Observe(ctx, c.Store, pipeline.Input{
		JobID:           jobID,
		FetchID:         fetchID,
		DerivedBy:       DerivedBy(atsName),
		Title:           p.Title,
		Company:         p.Company,
		Location:        p.Location,
		DescriptionHTML: p.DescriptionHTML,
		EmploymentType:  p.EmploymentType,
		PostedAtEst:     p.PostedAt,
		ApplyType:       applyTypeATS,
		ApplyURL:        p.ApplyURL,
		Gates:           c.Gates,
	}, func(q store.Queries, _ pipeline.ObserveResult) error {
		_, err := q.InsertStatusEvent(ctx, jobID, "new", "", "system", nil)
		return err
	})
	return err
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// boardPrefix is the part of SourceJobID before the slash, so the expiry reaches exactly
// the ids this board's postings are stored under.
func boardPrefix(b Board) string { return strings.TrimSuffix(SourceJobID(b, Posting{}), "/") }
