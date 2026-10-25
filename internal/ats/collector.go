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
	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// CommandCollect is run.command for an ATS collect.
const CommandCollect = "collect"

// stopReasonParserBroke is run.stop_reason when every board of an ATS lost its expected shape.
const stopReasonParserBroke = "parser_broke"

// StopDiskBudget is run.stop_reason when the storage check stopped the run: the database
// past its size budget or the disk under its free floor.
const StopDiskBudget = "disk_budget"

// StorageCheckEvery is how many boards an ATS's run reads between two storage checks; it
// checks before the first.
const StorageCheckEvery = 50

// applyTypeATS is job_observation.apply_type for a posting applied to on the employer's own
// ATS page.
const applyTypeATS = "ats"

// atsOrder is the order the ATSs run in, whatever order the board list names them.
var atsOrder = Names

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
	// Storage reports whether storing must stop (the database past its size budget or the
	// disk under its free floor) and why, in a plain note. Each ATS's run asks it before its
	// first board and every StorageCheckEvery boards; an error fails that run. nil never stops.
	Storage func(ctx context.Context) (over bool, why string, err error)
	Now     func() time.Time // nil: time.Now
	Log     *slog.Logger     // nil: slog.Default()
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
	Capped       int  // boards with more than MaxBoardPages pages: saved as read, nothing expired
	Postings     int  // postings saved from the boards that parsed
	ScreenedOut  int  // postings the title screen dropped, never stored
	NewJobs      int  // postings with no observation yet
	Observations int  // observations written: one per new job, one per later detail
	// The detail step (SmartRecruiters): a description fetched, one that failed (no answer,
	// an unexpected status, a 404 or a body the parser could not read), and one left for a
	// later run because the run's detail budget was spent or the breaker opened.
	Details, DetailsFailed, DetailsDeferred int
	// DetailsNotNeeded counts the postings never asked for a detail because a gate already
	// failed on their list data (see listGatesFail): new ones, and stored ones still
	// without their detail.
	DetailsNotNeeded int
	Expired          int64
	Reopened         int64
	Requests         int
	Failures         int
	StopReason       string
	StorageNote      string // why the storage check stopped the run (StopReason disk_budget)
	Note             string
	// ScreenedByBoard is ScreenedOut per board ("<ats>/<board>"), for boards that dropped any.
	ScreenedByBoard map[string]int
	// StateChanges counts the registry boards whose state moved, by "old→new".
	StateChanges map[string]int
	// Flagged are manual boards that missed on enough days to be retired, which manual
	// boards never are.
	Flagged []string
	// Shifted counts the paged boards whose listing changed while they were read (a later
	// page gave another total than the first), and Repeated those where a page added no
	// posting not already read (an ATS ignoring the offset, or a page repeated): both are
	// saved as read, and nothing of them expires that run.
	Shifted, Repeated int

	broke    []string // boards the parser broke on, for the note
	notFound []string // boards that answered 404, for the note
	capped   []string // boards read only up to MaxBoardPages, for the note
	shifted  []string // boards whose listing changed while read, for the note
	repeated []string // boards where a page added no new posting, for the note
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
		k := b.SameBoardKey()
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
	// A detail answer is kept like a board answer: its size and hash, the body only when
	// the parser broke on it.
	rec := store.NewFetchRecorder(q, runID, src).SizeOnly(fetch.KindDetail)
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
	if a.Details+a.DetailsFailed+a.DetailsDeferred+a.DetailsNotNeeded > 0 {
		s += fmt.Sprintf("; details: %d fetched, %d failed, %d left for a later run, %d not needed (gates failed)",
			a.Details, a.DetailsFailed, a.DetailsDeferred, a.DetailsNotNeeded)
	}
	if a.StopReason != "" && a.StopReason != stopReasonParserBroke {
		s += "; stopped: " + a.StopReason
		if a.StorageNote != "" {
			s += " (" + a.StorageNote + ")"
		}
	}
	if len(a.notFound) > 0 {
		s += "; board not found: " + capList(a.notFound)
	}
	if len(a.broke) > 0 {
		s += "; parser broke on " + capList(a.broke)
	}
	if len(a.capped) > 0 {
		s += fmt.Sprintf("; more than %d pages, read in part, nothing expired: %s", MaxBoardPages, capList(a.capped))
	}
	if len(a.shifted) > 0 {
		s += "; listing changed while reading, nothing expired: " + capList(a.shifted)
	}
	if len(a.repeated) > 0 {
		s += "; a page added no new posting, read in part, nothing expired: " + capList(a.repeated)
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

// RegistryKey is the board's key in the board registry: its ATS, slug and region, the slug
// spelled as given (an ATS API may be case-sensitive); the registry matches it ignoring case.
func (b Board) RegistryKey() store.BoardKey {
	return store.BoardKey{ATS: b.ATS, Slug: b.Board, Region: b.Region}
}

// SameBoardKey is RegistryKey with the slug lower-cased: two boards with the same key are
// the same board, whatever their spelling.
func (b Board) SameBoardKey() store.BoardKey {
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
	// detailStop is set once no more detail may be requested this run: the detail budget
	// is spent, or the breaker opened (then it is the stop reason, which ends the run after
	// the board in hand is saved).
	detailStop      string
	detailSpentOnly bool // detailStop is the detail budget only: the run goes on
	// listRules are the active stored rules the list-data gates read (listGatesFail),
	// loaded once per run; rulesLoaded says they are.
	listRules   []gates.Rule
	rulesLoaded bool
}

// listedPosting is one posting a board listed, with the fetch row it was read from.
type listedPosting struct {
	Posting
	fetchID int64
}

// collect fetches each board once. It sets res.StopReason when the breaker opens or the
// storage check says stop, either of which ends this ATS's run, and returns an error only when the run itself failed.
func (r *atsRun) collect(ctx context.Context, boards []Board) error {
	for i, b := range boards {
		if r.c.Storage != nil && i%StorageCheckEvery == 0 {
			over, why, err := r.c.Storage(ctx)
			if err != nil {
				return fmt.Errorf("storage check: %w", err)
			}
			if over {
				// Nothing more is stored; the boards not read wait for the next run.
				r.res.StopReason, r.res.StorageNote = StopDiskBudget, why
				r.c.log().Warn("collect stopped: storage budget", "ats", r.res.ATS, "why", why)
				return nil
			}
		}
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
//
// A paged board (SmartRecruiters, Teamtailor) is read page after page, at most
// MaxBoardPages; every page is a request like any other, paced, budgeted and counted by the
// breaker. A Personio feed that lists no described position in English is asked once more
// without the language, and that answer is the board's. A board is saved only when every
// page it was asked for parsed; any page that failed, broke or answered 404 is the board's
// outcome, and nothing of it is saved or expired. A board cut at MaxBoardPages is saved as
// read, but expires nothing, and so does one whose listing changed while it was read (a
// SmartRecruiters page whose totalFound differs from the first page's: postings may have
// shifted across a page edge unread; the rest of its pages are still read) and one where a
// later page added no posting not already read (an ATS that ignores the offset, or a page
// repeated: the read stops there).
func (r *atsRun) board(ctx context.Context, b Board) (stop string, _ error) {
	var items []listedPosting
	complete := true
	offset, retried := 0, false
	firstTotal, haveTotal, shifted := 0, false, false
	seenIDs := map[string]bool{}
	u := b.pageURL(0)
	for page := 0; ; page++ {
		if page == MaxBoardPages {
			complete = false
			r.res.Capped++
			r.res.capped = append(r.res.capped, b.String())
			r.c.log().Warn("board has more pages than read", "board", b.String(), "pages", MaxBoardPages)
			break
		}
		body, fetchID, done, stop, err := r.get(ctx, b, u)
		if done || err != nil {
			return stop, err
		}
		if b.ATS == Personio && !retried && personioEmptyWithLanguage(body) {
			// No position described in English: the feed without a language is the board's.
			retried, u = true, personioFeedURL(b, "")
			continue
		}
		ps, next, last, err := parsePage(b, offset, body)
		if err != nil {
			return "", r.broke(ctx, b, u, body, err)
		}
		if total, ok := listTotal(b, body); ok {
			switch {
			case !haveTotal:
				firstTotal, haveTotal = total, true
			case total != firstTotal && !shifted:
				shifted, complete = true, false
				r.res.Shifted++
				r.res.shifted = append(r.res.shifted, b.String())
				r.c.log().Warn("board listing changed while reading; nothing expired", "board", b.String(), "first_total", firstTotal, "total", total)
			}
		}
		added := false
		for _, p := range ps {
			if !seenIDs[p.ID] {
				seenIDs[p.ID], added = true, true
			}
			items = append(items, listedPosting{Posting: p, fetchID: fetchID})
		}
		if page > 0 && len(ps) > 0 && !added {
			complete = false
			r.res.Repeated++
			r.res.repeated = append(r.res.repeated, b.String())
			r.c.log().Warn("a page added no new posting; read stopped, nothing expired", "board", b.String(), "offset", offset)
			break
		}
		if last {
			break
		}
		offset, u = next, b.pageURL(next)
	}
	if err := r.save(ctx, b, items, complete); err != nil {
		return "", fmt.Errorf("board %s: %w", b, err)
	}
	r.res.BoardsOK++
	if err := r.record(ctx, b, store.CheckOK, len(items)); err != nil {
		return "", err
	}
	if !r.detailSpentOnly {
		return r.detailStop, nil // a breaker that opened during the detail step ends the run
	}
	return "", nil
}

// get requests one list URL of b. On a 2xx it returns the body and its fetch row. Anything
// else is the board's outcome, counted and written to the registry here, and done is true:
// stop is the breaker's reason when it opened, err is set when the run itself must end.
func (r *atsRun) get(ctx context.Context, b Board, u string) (body []byte, fetchID int64, done bool, stop string, _ error) {
	resp, err := r.client.Get(ctx, fetch.KindBoard, u)
	if err != nil {
		r.res.BoardsFailed++
		outcome, stop, terr := FailedFetch(ctx, resp, err)
		switch {
		case stop != "":
			r.c.log().Warn("fetching stopped", "ats", b.ATS, "reason", stop, "board", b.String())
			return nil, 0, true, stop, nil
		case terr != nil:
			return nil, 0, true, "", terr
		}
		// Skipped after a real attempt, already recorded and counted by the breaker; tried
		// again next run. Only a bad answer counts against the board.
		r.c.log().Warn("board skipped", "board", b.String(), "status", resp.Status, "outcome", outcome, "err", err)
		return nil, 0, true, "", r.record(ctx, b, outcome, 0)
	}
	if resp.Status == http.StatusNotFound {
		r.res.NotFound++
		r.res.notFound = append(r.res.notFound, b.String())
		r.c.log().Warn("board not found", "board", b.String())
		return nil, 0, true, "", r.record(ctx, b, store.CheckNotFound, 0)
	}
	id, found := r.rec.LastIDFor(u)
	if !found {
		return nil, 0, true, "", fmt.Errorf("board %s: no fetch row recorded", b)
	}
	return resp.Body, id, false, "", nil
}

// broke counts a board page the parser could not read and keeps its body. A board that
// cannot be read says nothing about which postings are gone.
func (r *atsRun) broke(ctx context.Context, b Board, u string, body []byte, perr error) error {
	r.res.BoardsBroke++
	r.res.broke = append(r.res.broke, b.String())
	r.c.log().Error("parser broke", "board", b.String(), "err", perr, "parser_broke", errors.Is(perr, ErrParserBroke))
	// Board fetch rows keep no body; one the parser broke on is kept, to read why.
	if err := r.keepBody(ctx, u, body); err != nil {
		return fmt.Errorf("board %s: %w", b, err)
	}
	return r.record(ctx, b, store.CheckError, 0)
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
// open jobs the board no longer lists, unless complete is false (a board read only in
// part). A posting the title screen drops is not stored, but it still counts as listed, so
// it never expires or leaves unchecked a job stored earlier.
//
// For an ATS whose list carries no description (SmartRecruiters) the detail step runs on
// the postings that passed the title screen, before anything is written: see details.
func (r *atsRun) save(ctx context.Context, b Board, postings []listedPosting, complete bool) error {
	ids := make([]string, 0, len(postings))    // kept by the title screen
	listed := make([]string, 0, len(postings)) // every id the board lists
	seen := map[string]bool{}
	byID := map[string]listedPosting{}
	for _, p := range postings {
		sid := SourceJobID(b, p.Posting)
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
		p.Company = firstNonEmpty(p.Company, b.Name, b.Board)
		byID[sid] = p
		ids = append(ids, sid)
	}

	// New: no observation yet, a job row or not.
	fresh, err := r.q.JobsWithoutObservation(ctx, r.src, ids)
	if err != nil {
		return err
	}
	var later []string // stored before, now with their detail: one more observation each
	if b.ATS == SmartRecruiters {
		if later, err = r.details(ctx, b, ids, fresh, byID); err != nil {
			return err
		}
	}

	jobIDs := map[string]int64{}
	stored := make([]string, 0, len(ids))
	for _, sid := range ids {
		p := byID[sid]
		companyID, err := r.q.UpsertCompany(ctx, p.Company, "", "")
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
		jobIDs[sid] = jobID
		stored = append(stored, sid)
	}
	r.res.Postings += len(stored)

	for _, sid := range fresh {
		jobID, ok := jobIDs[sid]
		if !ok {
			continue // skipped above
		}
		p := byID[sid]
		if err := r.c.writeObservation(ctx, b.ATS, jobID, p.fetchID, p.Posting, true); err != nil {
			return err
		}
		r.res.NewJobs++
		r.res.Observations++
	}
	for _, sid := range later {
		jobID, ok := jobIDs[sid]
		if !ok {
			continue
		}
		p := byID[sid]
		if err := r.c.writeObservation(ctx, b.ATS, jobID, p.fetchID, p.Posting, false); err != nil {
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
	if !complete {
		return nil
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

// details runs the detail step for one board whose list carries no description: one
// request per posting, for the new ones (fresh) first and then the stored ones still
// waiting for theirs, all among ids (the postings the title screen kept) and only those
// whose list data fails no gate (listGatesFail): one that fails there is stored as any
// other, with its gate rows, but its detail could not save it, so it is never asked for
// one and counts as not needed. A stored job is
// waiting when its latest observation was read from a board answer (store.JobsAwaitingDetail):
// a posting whose detail failed or was over budget is stored with an empty description
// (the gates flag the missing text, as they do for any posting without one), and the next
// run that lists it asks again. A detail that answered updates the posting in byID: its
// description, apply URL, public URL when the detail gives one, and the fetch row it cites.
// later is the waiting stored jobs whose detail came, each owed a new observation.
//
// Every request is paced by the run's limiter and counted by its breaker. A spent detail
// budget (fetch.KindDetail) stops only the detail step; a breaker that opens stops it and,
// once this board is saved, the run.
func (r *atsRun) details(ctx context.Context, b Board, ids, fresh []string, byID map[string]listedPosting) (later []string, _ error) {
	isFresh := map[string]bool{}
	for _, sid := range fresh {
		isFresh[sid] = true
	}
	var stored []string
	for _, sid := range ids {
		if !isFresh[sid] {
			stored = append(stored, sid)
		}
	}
	waiting, err := r.q.JobsAwaitingDetail(ctx, r.src, stored)
	if err != nil {
		return nil, err
	}
	isWaiting := map[string]bool{}
	for _, sid := range waiting {
		isWaiting[sid] = true
	}
	var eligible []string
	for _, sid := range append(append([]string{}, fresh...), waiting...) {
		failed, err := r.listGatesFail(ctx, byID[sid].Posting)
		if err != nil {
			return nil, err
		}
		if failed {
			r.res.DetailsNotNeeded++
			continue
		}
		eligible = append(eligible, sid)
	}
	for _, sid := range eligible {
		if r.detailStop != "" {
			r.res.DetailsDeferred++
			continue
		}
		p := byID[sid]
		ok, err := r.detail(ctx, b, &p)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		byID[sid] = p
		if isWaiting[sid] {
			later = append(later, sid)
		}
	}
	return later, nil
}

// listGatesFail reports whether a posting fails a gate on its list data alone, which no
// detail can change: the gates (gates.Evaluate, with the collector's candidate gates and the
// active stored rules) read its title, company and location with its workplace label, each
// cleaned as pipeline.Observe cleans it, and no description. Every fail counts except
// those that rest only on the description being empty, so that posting still gets its
// detail:
//   - the language gate reads the title when the description is empty; the description
//     decides the language, so it is not judged here (the observation carries a candidate
//     language, which also lets the other gates run on a title in another language);
//   - a location field with no workplace label is read as an office's address only while
//     nothing says remote, which the description may: ListedRemote turns that one reading
//     off, as it does for a source that lists remote jobs only;
//   - a rule on the description field reads the empty text, so it is left out.
//
// The posting's stored list observation still gets every gate as Observe evaluates it.
func (r *atsRun) listGatesFail(ctx context.Context, p Posting) (bool, error) {
	if !r.rulesLoaded {
		stored, err := r.q.ActiveRules(ctx)
		if err != nil {
			return false, err
		}
		for _, rl := range stored {
			if rl.Field == "description" {
				continue
			}
			r.listRules = append(r.listRules, gates.Rule{ID: rl.ID, Kind: rl.Kind, Field: rl.Field, Pattern: rl.Pattern, Reason: rl.Reason})
		}
		r.rulesLoaded = true
	}
	lang := "en"
	if len(r.c.Gates.Languages) > 0 {
		lang = r.c.Gates.Languages[0]
	}
	obs := gates.Observation{
		Title:          postingguard.CleanField(p.Title),
		Company:        postingguard.CleanField(p.Company),
		Location:       postingguard.CleanField(p.Location),
		Lang:           lang,
		LangConfidence: 1,
		ListedRemote:   true,
	}
	for _, g := range gates.Evaluate(obs, r.c.Gates, r.listRules) {
		if g.Verdict == gates.Fail && g.Gate != gates.GateLanguage {
			return true, nil
		}
	}
	return false, nil
}

// detail requests one posting's detail and fills p from it. ok is false when the detail
// did not come (counted in the result); err is set only when the run itself must end.
func (r *atsRun) detail(ctx context.Context, b Board, p *listedPosting) (ok bool, _ error) {
	u := b.DetailURL(p.ID)
	resp, err := r.client.Get(ctx, fetch.KindDetail, u)
	if err != nil {
		var open *fetch.OpenError
		if errors.As(err, &open) && open.Reason == fetch.StopBudget && open.Kind == fetch.KindDetail && open.Err == nil {
			r.detailStop, r.detailSpentOnly = string(fetch.StopBudget), true
			r.res.DetailsDeferred++
			r.c.log().Info("detail budget spent; the rest wait for a later run", "board", b.String())
			return false, nil
		}
		_, stop, terr := FailedFetch(ctx, resp, err)
		switch {
		case stop != "":
			r.detailStop = stop
			r.res.DetailsDeferred++
			r.c.log().Warn("fetching stopped", "ats", b.ATS, "reason", stop, "board", b.String())
			return false, nil
		case terr != nil:
			return false, terr
		}
		r.res.DetailsFailed++
		r.c.log().Warn("detail skipped", "board", b.String(), "job", p.ID, "status", resp.Status, "err", err)
		return false, nil
	}
	if resp.Status == http.StatusNotFound {
		r.res.DetailsFailed++
		r.c.log().Warn("detail not found", "board", b.String(), "job", p.ID)
		return false, nil
	}
	desc, apply, public, err := parseSmartRecruitersDetail(resp.Body)
	if err != nil {
		r.res.DetailsFailed++
		r.c.log().Error("parser broke on a detail", "board", b.String(), "job", p.ID, "err", err)
		return false, r.keepBody(ctx, u, resp.Body)
	}
	fetchID, found := r.rec.LastIDFor(u)
	if !found {
		return false, fmt.Errorf("detail %s %s: no fetch row recorded", b, p.ID)
	}
	p.DescriptionHTML, p.ApplyURL, p.fetchID = desc, apply, fetchID
	if public != "" {
		p.URL = public
	}
	r.res.Details++
	return true, nil
}

// writeObservation stores what one fetch showed about a job through pipeline.Observe (the
// sanitised observation and its gate results). For a new job (isNew) its `new` status event
// is written in Observe's own transaction; a later observation (a detail that came on a
// later run) writes none.
func (c *Collector) writeObservation(ctx context.Context, atsName string, jobID, fetchID int64, p Posting, isNew bool) error {
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
		if !isNew {
			return nil
		}
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
