package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/boardlist"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["boards"] = runBoards }

// verifyRecorder records nothing. boards verify is read-only and opens no database, but
// fetch.Client refuses to fetch without a Recorder, so it gets this one.
type verifyRecorder struct{}

func (verifyRecorder) Record(context.Context, fetch.Record) error { return nil }

// errBoardsNotOK makes boards verify exit 1 when any board is not ok, so a script can tell.
var errBoardsNotOK = errors.New("boards verify: not every board is ok")

// boardsUsage lists the boards subcommands.
const boardsUsage = "usage: leadhound boards verify [--boards FILE] [--dry-run --fixtures DIR]" +
	" | boards import FILE --origin O [--ats A]" +
	" | boards list [--state S] [--ats A] [--rows]" +
	" | boards check [--limit N] [--state S] [--ats A] [--order oldest|random]" +
	" | boards harvest ..."

// runBoards dispatches `leadhound boards <subcommand>`: import, list, check and harvest by
// name, and verify otherwise (which refuses anything but verify).
func runBoards(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "import":
			return runBoardsImport(ctx, args[1:])
		case "list":
			return runBoardsList(ctx, args[1:])
		case "check":
			return runBoardsCheck(ctx, args[1:])
		case "harvest":
			return runBoardsHarvest(ctx, args[1:])
		}
	}
	return runBoardsVerify(ctx, args)
}

// runBoardsVerify is `leadhound boards verify [--boards FILE] [--dry-run --fixtures DIR]`:
// one GET per board, read-only, one line per board. It opens no database and writes nothing.
// Every board ok exits 0; any other exits 1; live fetching off exits 0 with nothing requested.
func runBoardsVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("boards", flag.ContinueOnError)
	var af atsFlags
	af.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "verify" {
		return usageError("%s", boardsUsage)
	}
	if err := af.check(); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	boards, err := boardList(cfg, af.boards, "boards verify", af.dryRun)
	if err != nil {
		return err
	}
	var newClient func() *fetch.Client
	if af.dryRun {
		ft, err := fetch.NewFixtureTransport(af.fixtures)
		if err != nil {
			return usageError("%v", err)
		}
		newClient = func() *fetch.Client {
			return fetch.New(fetch.WithFixtures(ft), fetch.WithRecorder(verifyRecorder{}), fetch.WithProfile(atsProfile), fetch.WithMaxBody(atsMaxBody),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)), fetch.WithJitter(0))
		}
	} else {
		if !liveAllowsBoards(cfg.Live, boards) {
			_, _ = fmt.Fprintf(stdout, "boards verify: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
			return nil
		}
		newClient = func() *fetch.Client {
			c := fetch.New(fetch.WithLiveGate(cfg.Live), fetch.WithRecorder(verifyRecorder{}), fetch.WithProfile(atsProfile), fetch.WithMaxBody(atsMaxBody),
				fetch.WithLimiter(rate.NewLimiter(rate.Every(cfg.ATSInterval), 1)))
			c.HTTP.Timeout = atsTimeout
			return c
		}
	}

	clients := map[string]*fetch.Client{} // one per ATS: each API is paced on its own
	seen := map[string]bool{}
	bad := 0
	for _, b := range boards {
		if seen[b.String()] {
			continue
		}
		seen[b.String()] = true
		c := clients[b.ATS]
		if c == nil {
			c = newClient()
			clients[b.ATS] = c
		}
		line, ok, err := verifyBoard(ctx, c, b)
		if err != nil {
			return err
		}
		if !ok {
			bad++
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	if bad > 0 {
		return fmt.Errorf("%w: %d of %d", errBoardsNotOK, bad, len(seen))
	}
	return nil
}

// verifyBoard requests one board and returns its line: ok with the posting count, 404 not
// found, or error with a short reason. err is set only when the caller's context is done.
func verifyBoard(ctx context.Context, c *fetch.Client, b ats.Board) (line string, ok bool, err error) {
	resp, err := c.Get(ctx, fetch.KindBoard, b.APIURL())
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", false, err
	case err != nil:
		slog.Warn("board request failed", "board", b.String(), "err", err)
		return fmt.Sprintf("error %s  %s", b, shortReason(resp, err)), false, nil
	case resp.Status == http.StatusNotFound:
		return fmt.Sprintf("404 %s  not found", b), false, nil
	}
	postings, err := ats.Parse(b, resp.Body)
	if err != nil {
		slog.Warn("board unreadable", "board", b.String(), "err", err)
		return fmt.Sprintf("error %s  parser broke", b), false, nil
	}
	if len(postings) == 0 {
		return fmt.Sprintf("ok  %s  0 postings (check the slug)", b), true, nil
	}
	return fmt.Sprintf("ok  %s  %d postings", b, len(postings)), true, nil
}

// shortReason says in a few words why a request failed.
func shortReason(resp fetch.Response, err error) string {
	var open *fetch.OpenError
	switch {
	case errors.As(err, &open):
		return "stopped: " + string(open.Reason)
	case errors.Is(err, fetch.ErrStatus):
		return fmt.Sprintf("status %d", resp.Status)
	case errors.Is(err, fetch.ErrBodyTooLarge):
		return "body over the size limit"
	case errors.Is(err, fetch.ErrLiveOff):
		return "live fetching is off for this host"
	default:
		return "network error"
	}
}

// importRefusedCap is how many refused rows boards import names before it says how many more.
const importRefusedCap = 20

// importRefusal is one row boards import did not take: where it was and why.
type importRefusal struct {
	where  string // "line 12", or the board for a list without line numbers
	reason string
}

// runBoardsImport is `leadhound boards import FILE --origin O [--ats A]`: it adds the boards
// in FILE to the registry and writes nothing else. A .yaml or .yml FILE is a board list in
// the ats-boards.yaml shape; any other is a name,slug,url CSV whose ATS and region come from
// each URL. With --ats only that ATS's boards are taken; the others are refused. The import
// is idempotent: a board already there keeps its state, and its origin only moves up
// (manual > found > seed-list = commoncrawl).
func runBoardsImport(ctx context.Context, args []string) error {
	const usage = "usage: leadhound boards import FILE --origin manual|found|seed-list|commoncrawl [--ats greenhouse|lever|ashby]"
	fs := flag.NewFlagSet("boards import", flag.ContinueOnError)
	origin := fs.String("origin", "", "how these boards were found: "+strings.Join(store.Origins, ", "))
	atsName := fs.String("ats", "", "take only this ATS's boards")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) != 1:
		return usageError("%s", usage)
	case !slices.Contains(store.Origins, *origin):
		return usageError("--origin must be one of %s; %s", strings.Join(store.Origins, ", "), usage)
	case *atsName != "" && !slices.Contains(atsNames, *atsName):
		return usageError("--ats must be one of %s", strings.Join(atsNames, ", "))
	}
	boards, refused, err := readBoardFile(pos[0])
	if err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	var rows []store.BoardImport
	for _, b := range boards {
		if *atsName != "" && b.ATS != *atsName {
			refused = append(refused, importRefusal{where: b.String(), reason: "not a " + *atsName + " board (--ats)"})
			continue
		}
		rows = append(rows, store.BoardImport{BoardKey: b.RegistryKey(), Name: b.Name, Origin: *origin})
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer s.Close()
	tally, err := s.Q().ImportBoardsTally(ctx, rows)
	if err != nil {
		return err
	}
	revived := ""
	if tally.Revived > 0 {
		revived = fmt.Sprintf(" · revived %d", tally.Revived)
	}
	_, _ = fmt.Fprintf(stdout, "boards import: added %d · already known %d · refused %d%s\n", tally.Added, tally.Known, len(refused), revived)
	for i, r := range refused {
		if i == importRefusedCap {
			_, _ = fmt.Fprintf(stdout, "  and %d more\n", len(refused)-importRefusedCap)
			break
		}
		_, _ = fmt.Fprintf(stdout, "  %s: %s\n", r.where, r.reason)
	}
	return nil
}

// readBoardFile reads the boards in path: a .yaml/.yml board list (all or nothing: one bad
// line refuses the file), or a name,slug,url CSV whose unreadable rows are refused one by
// one. A file that cannot be read or parsed at all is a usage error.
func readBoardFile(path string) ([]ats.Board, []importRefusal, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		b, err := ats.LoadBoardsFile(path)
		if err != nil {
			return nil, nil, usageError("%v", err)
		}
		return b, nil, nil
	}
	f, err := os.Open(path) // #nosec G304 -- the operator's own board list
	if err != nil {
		return nil, nil, usageError("%v", err)
	}
	defer func() { _ = f.Close() }()
	entries, bad, err := boardlist.ParseCSV(f)
	if err != nil {
		return nil, nil, usageError("%s: %v", filepath.Base(path), err)
	}
	boards := make([]ats.Board, 0, len(entries))
	for _, e := range entries {
		b := e.Board
		if b.Name == "" {
			b.Name = e.Name
		}
		boards = append(boards, b)
	}
	refused := make([]importRefusal, 0, len(bad))
	for _, r := range bad {
		refused = append(refused, importRefusal{where: fmt.Sprintf("line %d", r.Line), reason: r.Reason})
	}
	return boards, refused, nil
}

// atsNames are the ATSs the registry holds, in run order.
var atsNames = []string{ats.Greenhouse, ats.Lever, ats.Ashby}

// boardFilterFlags are the --state and --ats flags boards list and check share.
type boardFilterFlags struct {
	state, ats string
}

func (f *boardFilterFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.state, "state", "", "only boards in this state: "+strings.Join(store.BoardStates, ", "))
	fs.StringVar(&f.ats, "ats", "", "only this ATS's boards: "+strings.Join(atsNames, ", "))
}

func (f *boardFilterFlags) check() error {
	switch {
	case f.state != "" && !slices.Contains(store.BoardStates, f.state):
		return usageError("--state must be one of %s", strings.Join(store.BoardStates, ", "))
	case f.ats != "" && !slices.Contains(atsNames, f.ats):
		return usageError("--ats must be one of %s", strings.Join(atsNames, ", "))
	}
	return nil
}

// runBoardsList is `leadhound boards list [--state S] [--ats A] [--rows]`: the registry's
// board counts by ATS and state, then with --rows one line per board.
func runBoardsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("boards list", flag.ContinueOnError)
	var ff boardFilterFlags
	ff.register(fs)
	rowsFlag := fs.Bool("rows", false, "also print one line per board")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: leadhound boards list [--state S] [--ats A] [--rows]")
	}
	if err := ff.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer s.Close()
	filter := store.BoardFilter{State: ff.state, ATS: ff.ats}
	counts, err := s.Q().CountBoards(ctx, filter)
	if err != nil {
		return err
	}
	writeBoardCounts(stdout, counts)
	if !*rowsFlag {
		return nil
	}
	rows, err := s.Q().ListBoards(ctx, filter)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "\nats\tslug\tregion\torigin\tstate\tmisses\tpostings\tchecked")
	for _, b := range rows {
		postings, checked := "-", "never"
		if b.PostingsLast != nil {
			postings = fmt.Sprint(*b.PostingsLast)
		}
		if b.CheckedAt != nil {
			checked = b.CheckedAt.UTC().Format(time.RFC3339)
		}
		region := b.Region
		if region == "" {
			region = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", b.ATS, b.Slug, region, b.Origin, b.State, b.Misses, postings, checked)
	}
	return tw.Flush()
}

// writeBoardCounts prints the ATS × state table, with a total column and a total row.
func writeBoardCounts(w io.Writer, counts []store.BoardCount) {
	n := map[string]map[string]int{}
	for _, c := range counts {
		if n[c.ATS] == nil {
			n[c.ATS] = map[string]int{}
		}
		n[c.ATS][c.State] = c.N
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "ats\t"+strings.Join(store.BoardStates, "\t")+"\ttotal\t")
	totals := map[string]int{}
	all := 0
	for _, a := range atsNames {
		line, sum := a, 0
		for _, st := range store.BoardStates {
			line += fmt.Sprintf("\t%d", n[a][st])
			sum += n[a][st]
			totals[st] += n[a][st]
		}
		all += sum
		_, _ = fmt.Fprintf(tw, "%s\t%d\t\n", line, sum)
	}
	line := "total"
	for _, st := range store.BoardStates {
		line += fmt.Sprintf("\t%d", totals[st])
	}
	_, _ = fmt.Fprintf(tw, "%s\t%d\t\n", line, all)
	_ = tw.Flush()
}

// commandCheck is run.command for boards check. The brief reads collect runs only, so a
// check never shows as a collect; its throttled or blocked stop still pauses the ATS for
// the day (PausedToday reads every command).
const commandCheck = "check"

// allATSHosts is every API host a board in the registry can be requested from.
func allATSHosts() []ats.Board {
	return []ats.Board{
		{ATS: ats.Greenhouse, Board: "x"},
		{ATS: ats.Lever, Board: "x"},
		{ATS: ats.Lever, Board: "x", Region: ats.RegionEU},
		{ATS: ats.Ashby, Board: "x"},
	}
}

// checkTally is what boards check did for one ATS.
type checkTally struct {
	ats                                     string
	runID                                   int64
	paused                                  bool
	stop                                    string
	boards, ok, notFound, failed, unchecked int
	noVerdict                               int // no answer said anything about the board
	requests, failures                      int
	retired                                 int
	changes                                 map[string]int
	flagged                                 []string
	err                                     error
}

// runBoardsCheck is `leadhound boards check [--limit N] [--state S] [--ats A] [--order
// oldest|random]`: one GET per registry board, live only, each ATS side by side on its own
// paced client, breaker and run row; each answer updates the board's row (state, checked_at,
// last_ok_at, misses, postings_last). A board that is not manual and misses on 3 separate
// days is retired; a manual one is only flagged. Without --state every board but the
// retired ones is a candidate; --order oldest (the default) checks the least recently
// checked first. An empty registry is a usage error.
func runBoardsCheck(ctx context.Context, args []string) error {
	const usage = "usage: leadhound boards check [--limit N] [--state S] [--ats A] [--order oldest|random]"
	fs := flag.NewFlagSet("boards check", flag.ContinueOnError)
	var ff boardFilterFlags
	ff.register(fs)
	limit := fs.Int("limit", 0, "check at most this many boards (0: all)")
	order := fs.String("order", store.BoardOrderOldest, "oldest (least recently checked first) or random")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	switch {
	case len(pos) != 0:
		return usageError("%s", usage)
	case *limit < 0:
		return usageError("--limit must be 0 or more")
	case *order != store.BoardOrderOldest && *order != store.BoardOrderRandom:
		return usageError("--order must be oldest or random")
	}
	if err := ff.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	if !liveAllowsBoards(cfg.Live, allATSHosts()) {
		_, _ = fmt.Fprintf(stdout, "boards check: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()
	counts, err := q.CountBoards(ctx, store.BoardFilter{})
	if err != nil {
		return err
	}
	if len(counts) == 0 {
		return usageError("the board registry is empty: add boards with leadhound boards import FILE --origin O first")
	}
	rows, err := q.ListBoards(ctx, store.BoardFilter{State: ff.state, ATS: ff.ats, NotRetired: true, Limit: *limit, Order: *order})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(stdout, "boards check: no boards match; nothing requested")
		return nil
	}
	groups := map[string][]ats.Board{}
	for _, r := range rows {
		groups[r.ATS] = append(groups[r.ATS], ats.Board{ATS: r.ATS, Board: r.Slug, Region: r.Region, Name: r.Name})
	}

	start := time.Now()
	tallies := make([]*checkTally, len(atsNames))
	var wg sync.WaitGroup
	for i, name := range atsNames {
		if len(groups[name]) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tallies[i] = checkATS(ctx, cfg, q, loc, name, groups[name])
		}()
	}
	wg.Wait()

	var errs []error
	total := checkTally{changes: map[string]int{}}
	for _, t := range tallies {
		if t == nil {
			continue
		}
		if t.err != nil {
			errs = append(errs, t.err)
		}
		writeCheckTally(stdout, t)
		total.boards += t.boards
		total.ok += t.ok
		total.notFound += t.notFound
		total.failed += t.failed
		total.noVerdict += t.noVerdict
		total.retired += t.retired
		total.unchecked += t.unchecked
	}
	_, _ = fmt.Fprintf(stdout, "boards check: %d boards · %d ok · %d not_found · %d error · %d no verdict · %d retired · %d not checked · %s\n",
		total.boards, total.ok, total.notFound, total.failed, total.noVerdict, total.retired, total.unchecked, time.Since(start).Round(time.Millisecond))
	return errors.Join(errs...)
}

// writeCheckTally prints one ATS's check outcome.
func writeCheckTally(w io.Writer, t *checkTally) {
	if t.paused {
		_, _ = fmt.Fprintf(w, "boards check: %s run %d · paused today: %s; nothing requested\n", t.ats, t.runID, t.stop)
		return
	}
	stop := t.stop
	if stop == "" {
		stop = "none"
	}
	_, _ = fmt.Fprintf(w, "boards check: %s run %d · %d boards: %d ok, %d not_found, %d error, %d no verdict, %d retired, %d not checked · %d requests (%d failed) · stop %s\n",
		t.ats, t.runID, t.boards, t.ok, t.notFound, t.failed, t.noVerdict, t.retired, t.unchecked, t.requests, t.failures, stop)
	if len(t.changes) > 0 {
		_, _ = fmt.Fprintf(w, "  board states: %s\n", ats.StateChangeSummary(t.changes))
	}
	for _, f := range t.flagged {
		_, _ = fmt.Fprintf(w, "  manual board %s has missed on %d+ days; kept (manual boards are never retired)\n", f, store.RetireAfterMisses)
	}
	if t.err != nil {
		_, _ = fmt.Fprintf(w, "  failed: %v\n", t.err)
	}
}

// checkATS checks one ATS's boards in order on its own client and run row.
func checkATS(ctx context.Context, cfg config.Config, q store.Queries, loc *time.Location, name string, boards []ats.Board) *checkTally {
	t := &checkTally{ats: name, boards: len(boards), changes: map[string]int{}}
	reason, paused, err := q.PausedToday(ctx, name, loc, time.Now())
	if err != nil {
		t.err = fmt.Errorf("boards check %s: %w", name, err)
		return t
	}
	runID, err := q.StartRun(ctx, commandCheck, name)
	if err != nil {
		t.err = fmt.Errorf("boards check %s: %w", name, err)
		return t
	}
	t.runID = runID
	if paused {
		t.paused, t.stop, t.unchecked = true, reason, len(boards)
		t.err = q.FinishRun(ctx, runID, store.RunResult{StopReason: reason, Note: name + ": paused today: " + reason})
		return t
	}
	c := fetch.New(
		fetch.WithLiveGate(cfg.Live),
		fetch.WithRecorder(verifyRecorder{}),
		fetch.WithProfile(atsProfile),
		fetch.WithMaxBody(atsMaxBody),
		fetch.WithLimiter(rate.NewLimiter(rate.Every(cfg.ATSInterval), 1)),
		fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{fetch.KindBoard: 2 * len(boards)})),
	)
	c.HTTP.Timeout = atsTimeout
	for i, b := range boards {
		outcome, postings, stop, err := checkBoard(ctx, c, b)
		if err != nil {
			t.err = fmt.Errorf("boards check %s: %w", b, err)
			t.unchecked += len(boards) - i
			break
		}
		if stop != "" {
			t.stop = stop
			t.unchecked += len(boards) - i
			break
		}
		ch, err := q.RecordBoardCheck(ctx, b.RegistryKey(), outcome, postings, loc, time.Now())
		if err != nil {
			t.err = fmt.Errorf("boards check %s: %w", b, err)
			t.unchecked += len(boards) - i - 1
			break
		}
		switch {
		case ch.New == store.BoardRetired:
			t.retired++
		case outcome == store.CheckOK:
			t.ok++
		case outcome == store.CheckNotFound:
			t.notFound++
		case outcome == store.CheckNoVerdict:
			t.noVerdict++
		default:
			t.failed++
		}
		if ch.Changed() {
			t.changes[ats.StateChangeKey(ch.Old, ch.New)]++
		}
		if ch.Flagged {
			t.flagged = append(t.flagged, b.String())
		}
	}
	t.requests, t.failures = c.Breaker.Requests(), c.Breaker.Failures()
	note := fmt.Sprintf("%s: %d boards checked: %d ok, %d not_found, %d error, %d no verdict, %d retired, %d not checked",
		name, t.boards-t.unchecked, t.ok, t.notFound, t.failed, t.noVerdict, t.retired, t.unchecked)
	finishErr := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{
		Requests: t.requests, Failures: t.failures, StopReason: t.stop, Note: note,
	})
	t.err = errors.Join(t.err, finishErr)
	return t
}

// checkBoard requests one board at its light check URL and sorts the answer into a
// registry outcome: ok with the posting count, not_found for a 404, error for an unexpected
// status or a body the parser cannot read, no_verdict when nothing answered (a timeout, a
// network error, a body over the size limit). stop is the breaker's reason when it opened
// (the board was not checked); err is set only when the command must stop (its context is
// done, or the live gate refused the host). See ats.FailedFetch.
func checkBoard(ctx context.Context, c *fetch.Client, b ats.Board) (outcome string, postings int, stop string, err error) {
	resp, err := c.Get(ctx, fetch.KindBoard, b.CheckURL())
	switch {
	case err != nil:
		outcome, stop, fatal := ats.FailedFetch(ctx, resp, err)
		if outcome != "" {
			slog.Warn("board request failed", "board", b.String(), "outcome", outcome, "err", err)
		}
		return outcome, 0, stop, fatal
	case resp.Status == http.StatusNotFound:
		return store.CheckNotFound, 0, "", nil
	}
	ps, err := ats.Parse(b, resp.Body)
	if err != nil {
		slog.Warn("board unreadable", "board", b.String(), "err", err)
		return store.CheckError, 0, "", nil
	}
	return store.CheckOK, len(ps), "", nil
}
