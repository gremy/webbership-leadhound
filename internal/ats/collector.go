package ats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
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

// Collector runs one ATS collect: every board once, one run row per ATS, the ATSs side by
// side.
type Collector struct {
	Store *store.Store
	// NewClient builds one ATS's client around that run's recorder. It is called once per ATS
	// per run, so each ATS gets its own breaker and budgets (one source's trouble never stops
	// another) and records into its own run row.
	NewClient func(atsName string, rec fetch.Recorder) *fetch.Client
	Loc       *time.Location // LEADHOUND_TZ: what "today" means for a pause; nil: UTC
	// Fixture writes every row under SourceFixture(ats) instead of the ATS's name, so a dry
	// run never reads as the ATS's data, never pauses it and never expires its jobs. A fixture
	// run also leaves the board registry alone: a fixture answer says nothing about a board.
	Fixture bool
	// TitleScreen keeps a posting when it returns true for its title; one it drops is never
	// stored, so the next fetch screens it again. nil keeps every posting.
	TitleScreen func(title string) bool
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
	ScreenedOut  int  // postings the title screen dropped, never stored
	NewJobs      int  // postings with no observation yet
	Observations int  // observations written
	Expired      int64
	Reopened     int64
	Requests     int
	Failures     int
	StopReason   string
	Note         string
	// ScreenedByBoard is ScreenedOut per board ("<ats>/<board>"), for boards that dropped any.
	ScreenedByBoard map[string]int
	// StateChanges counts the registry boards whose state moved, by "old→new".
	StateChanges map[string]int
	// Flagged are manual boards that missed on enough days to be retired, which manual
	// boards never are.
	Flagged []string

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

// Run collects every board, grouped by ATS: each ATS present gets its own goroutine, run
// row, client and breaker, so the ATSs run side by side and one runs even when another
// stopped or failed; within one ATS the boards go one after another at its client's pace.
// A recorded stop is an ATSResult with StopReason set; an error means at least one ATS's run
// itself failed, and it is returned after every ATS has run, joined with the others. Results
// and errors come back in the fixed ATS order, whatever order the runs finished in.
func (c *Collector) Run(ctx context.Context, boards []Board) (Result, error) {
	groups := map[string][]Board{}
	seen := map[store.BoardKey]bool{}
	for _, b := range boards {
		if err := b.Validate(); err != nil {
			return Result{}, fmt.Errorf("ats: collect: %w", err)
		}
		k := b.RegistryKey()
		if seen[k] {
			continue // each board is fetched once per run
		}
		seen[k] = true
		groups[b.ATS] = append(groups[b.ATS], b)
	}
	results := make([]*ATSResult, len(atsOrder))
	errs := make([]error, len(atsOrder))
	var wg sync.WaitGroup
	for i, name := range atsOrder {
		if len(groups[name]) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ctx.Err(); err != nil {
				errs[i] = fmt.Errorf("ats: collect %s: %w", name, err)
				return
			}
			r, err := c.runATS(ctx, name, groups[name])
			results[i], errs[i] = &r, err
		}()
	}
	wg.Wait()
	var res Result
	for _, r := range results {
		if r != nil {
			res.ATS = append(res.ATS, *r)
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
	s := fmt.Sprintf("%s: %d boards (%d ok, %d failed, %d parser broke, %d not found), %d postings, %d screened out, %d new, %d observed, %d expired, %d reopened",
		a.ATS, a.Boards, a.BoardsOK, a.BoardsFailed, a.BoardsBroke, a.NotFound,
		a.Postings, a.ScreenedOut, a.NewJobs, a.Observations, a.Expired, a.Reopened)
	if a.StopReason != "" && a.StopReason != stopReasonParserBroke {
		s += "; stopped: " + a.StopReason
	}
	if len(a.notFound) > 0 {
		s += "; board not found: " + capList(a.notFound)
	}
	if len(a.broke) > 0 {
		s += "; parser broke on " + capList(a.broke)
	}
	if len(a.StateChanges) > 0 {
		s += "; board states: " + StateChangeSummary(a.StateChanges)
	}
	if len(a.Flagged) > 0 {
		s += "; manual boards missing " + fmt.Sprint(store.RetireAfterMisses) + "+ days (kept): " + capList(a.Flagged)
	}
	for _, line := range a.emptied {
		s += "\n" + line
	}
	return s
}

// RegistryKey is the board's key in the board registry: its ATS, lower-case slug and
// region. Two boards with the same key are the same board.
func (b Board) RegistryKey() store.BoardKey {
	return store.BoardKey{ATS: b.ATS, Slug: strings.ToLower(b.Board), Region: b.Region}
}

// noteListCap is how many board names a note lists before it says how many more.
const noteListCap = 10

// capList joins names, listing at most noteListCap and then how many more.
func capList(names []string) string {
	if len(names) <= noteListCap {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:noteListCap], ", ") + fmt.Sprintf(" and %d more", len(names)-noteListCap)
}

// StateChangeSummary is "2 ok→not_found, 1 error→retired": each change with its count,
// in a stable order.
func StateChangeSummary(changes map[string]int) string {
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", changes[k], k))
	}
	return strings.Join(parts, ", ")
}

// StateChangeKey is the key a board's state change is counted under: "old→new".
func StateChangeKey(from, to string) string { return from + "→" + to }

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
		outcome, stop, terr := FailedFetch(ctx, resp, err)
		switch {
		case stop != "":
			r.c.log().Warn("fetching stopped", "ats", b.ATS, "reason", stop, "board", b.String())
			return stop, nil
		case terr != nil:
			return "", terr
		}
		// Skipped after a real attempt, already recorded and counted by the breaker; tried
		// again next run. Only a bad answer counts against the board.
		r.c.log().Warn("board skipped", "board", b.String(), "status", resp.Status, "outcome", outcome, "err", err)
		return "", r.record(ctx, b, outcome, 0)
	}
	if resp.Status == http.StatusNotFound {
		r.res.NotFound++
		r.res.notFound = append(r.res.notFound, b.String())
		r.c.log().Warn("board not found", "board", b.String())
		return "", r.record(ctx, b, store.CheckNotFound, 0)
	}
	postings, err := Parse(b, resp.Body)
	if err != nil {
		// A board that cannot be read says nothing about which postings are gone.
		r.res.BoardsBroke++
		r.res.broke = append(r.res.broke, b.String())
		r.c.log().Error("parser broke", "board", b.String(), "err", err, "parser_broke", errors.Is(err, ErrParserBroke))
		// Board fetch rows keep no body; one the parser broke on is kept, to read why.
		if err := r.keepBody(ctx, u, resp.Body); err != nil {
			return "", fmt.Errorf("board %s: %w", b, err)
		}
		return "", r.record(ctx, b, store.CheckError, 0)
	}
	fetchID, found := r.rec.LastIDFor(u)
	if !found {
		return "", fmt.Errorf("board %s: no fetch row recorded", b)
	}
	if err := r.save(ctx, b, fetchID, postings); err != nil {
		return "", fmt.Errorf("board %s: %w", b, err)
	}
	r.res.BoardsOK++
	return "", r.record(ctx, b, store.CheckOK, len(postings))
}

// keepBody attaches body to the fetch row recorded for u. A row whose stored hash does not
// match the body is left as it is, with a warning.
func (r *atsRun) keepBody(ctx context.Context, u string, body []byte) error {
	fetchID, found := r.rec.LastIDFor(u)
	if !found {
		return fmt.Errorf("no fetch row recorded for %s", u)
	}
	err := r.q.AttachFetchBody(ctx, fetchID, body)
	if errors.Is(err, store.ErrFetchBodyMismatch) {
		r.c.log().Warn("parser-broke body not kept: hash mismatch", "fetch_id", fetchID)
		return nil
	}
	return err
}

// record writes one fetch's outcome to the board registry, as boards check does, and
// counts the state change. A fixture run and a board missing from the registry write
// nothing: collect never adds a board.
func (r *atsRun) record(ctx context.Context, b Board, outcome string, postings int) error {
	if r.c.Fixture {
		return nil
	}
	ch, err := r.q.RecordBoardCheck(ctx, b.RegistryKey(), outcome, postings, r.c.loc(), r.c.now())
	if err != nil {
		return fmt.Errorf("board %s: %w", b, err)
	}
	r.res.noteChange(b, ch)
	return nil
}

// noteChange counts one registry write in the result.
func (a *ATSResult) noteChange(b Board, ch store.BoardChange) {
	if ch.Changed() {
		if a.StateChanges == nil {
			a.StateChanges = map[string]int{}
		}
		a.StateChanges[StateChangeKey(ch.Old, ch.New)]++
	}
	if ch.Flagged {
		a.Flagged = append(a.Flagged, b.String())
	}
}

// FailedFetch sorts a failed board Get, for collect and boards check alike:
//   - stop is the breaker's reason when it opened (the board was not requested, or this
//     request opened it): the ATS's run ends;
//   - fatal is set when the command itself must stop: the caller's context is done, or the
//     live gate refused the host;
//   - otherwise outcome is the board's registry outcome: store.CheckError for an answer
//     with an unexpected status (a 5xx, a 403), which counts as a miss, and
//     store.CheckNoVerdict when no answer said anything about the board (a body over the
//     size limit, a timeout, a network error), which only stamps checked_at, so a large or
//     slow board is never retired for it.
func FailedFetch(ctx context.Context, resp fetch.Response, err error) (outcome, stop string, fatal error) {
	var open *fetch.OpenError
	var nerr net.Error
	switch {
	case errors.As(err, &open):
		return "", string(open.Reason), nil
	case ctx.Err() != nil:
		return "", "", err
	case errors.Is(err, fetch.ErrLiveOff):
		return "", "", err
	case errors.Is(err, fetch.ErrStatus):
		return store.CheckError, "", nil
	case errors.Is(err, fetch.ErrBodyTooLarge), errors.Is(err, context.DeadlineExceeded),
		errors.As(err, &nerr) && nerr.Timeout(), resp.Status == 0:
		return store.CheckNoVerdict, "", nil
	default:
		return "", "", err
	}
}

// save stores one parsed board: each posting's company and job, an observation for every
// job that has none, reopens the listed jobs that had expired, and expires this board's
// open jobs the board no longer lists. A posting the title screen drops is not stored, but
// it still counts as listed, so it never expires or leaves unchecked a job stored earlier.
func (r *atsRun) save(ctx context.Context, b Board, fetchID int64, postings []Posting) error {
	ids := make([]string, 0, len(postings))    // stored this time
	listed := make([]string, 0, len(postings)) // every id the board lists
	seen := map[string]bool{}
	jobIDs := map[string]int64{}
	byID := map[string]Posting{}
	for _, p := range postings {
		sid := SourceJobID(b, p)
		if seen[sid] {
			continue
		}
		seen[sid] = true
		listed = append(listed, sid)
		if r.c.TitleScreen != nil && !r.c.TitleScreen(p.Title) {
			r.res.ScreenedOut++
			if r.res.ScreenedByBoard == nil {
				r.res.ScreenedByBoard = map[string]int{}
			}
			r.res.ScreenedByBoard[b.String()]++
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

	reopened, err := r.q.Reopen(ctx, r.src, listed)
	if err != nil {
		return err
	}
	r.res.Reopened += reopened
	// Every job the board still lists was checked just now.
	if _, err := r.q.MarkChecked(ctx, r.src, listed); err != nil {
		return err
	}
	expired, err := r.q.ExpireMissing(ctx, r.src, boardPrefix(b), listed)
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
