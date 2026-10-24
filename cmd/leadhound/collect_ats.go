package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// atsSource is the positional argument that selects the ATS collect: `collect ats`.
const atsSource = "ats"

// atsMaxBody is the largest board answer read: one request returns every posting with its
// full description, which can run past fetch's default.
const atsMaxBody = 20 << 20

// atsTimeout is how long one board request may take: a large board's full answer can run
// well past fetch's default. fetch has no option for it, so callers set Client.HTTP.Timeout,
// as boards harvest does.
const atsTimeout = 60 * time.Second

// atsProfile asks the ATS APIs for JSON and treats no response as blocked: they are public
// APIs, so a 403 is an error for one board, not a reason to stop.
var atsProfile = fetch.Profile{Name: "ats-json", Header: http.Header{"Accept": {"application/json"}}}

// atsFeedProfile is atsProfile for the ATSs that answer with a feed: Teamtailor's RSS and
// Personio's XML.
var atsFeedProfile = fetch.Profile{Name: "ats-feed", Header: http.Header{"Accept": {"application/rss+xml, application/xml;q=0.9, text/xml;q=0.8"}}}

// atsProfileFor is the profile an ATS's client sends.
func atsProfileFor(atsName string) fetch.Profile {
	if atsName == ats.Teamtailor || atsName == ats.Personio {
		return atsFeedProfile
	}
	return atsProfile
}

// atsFlags are the flags the ATS commands share.
type atsFlags struct {
	fetchFlags
	boards string
}

func (f *atsFlags) register(fs *flag.FlagSet) {
	f.fetchFlags.register(fs)
	fs.StringVar(&f.boards, "boards", "", "board list (default: LEADHOUND_DATA_DIR/"+ats.BoardsFile+", else the demo boards)")
}

// boardList is the boards a command works on: --boards FILE, else ats-boards.yaml in
// LEADHOUND_DATA_DIR, else, in a dry run only, the fictional demo boards with one note on
// notes. A live command with no list is a usage error: the demo slugs are fictional, so
// asking a real ATS for them would only waste requests. A file that exists but cannot be
// read, or is not a valid board list, is a usage error naming it.
func boardList(cfg config.Config, file, command string, dryRun bool) ([]ats.Board, error) {
	if file != "" {
		b, err := ats.LoadBoardsFile(file)
		if err != nil {
			return nil, usageError("%v", err)
		}
		return b, nil
	}
	if cfg.DataDir != "" {
		b, err := ats.LoadBoards(cfg.DataDir)
		switch {
		case err == nil:
			return b, nil
		case !errors.Is(err, fs.ErrNotExist):
			return nil, usageError("%v", err)
		case !dryRun:
			return nil, errNoBoardList()
		}
		_, _ = fmt.Fprintf(notes, "%s: no %s in LEADHOUND_DATA_DIR; using the demo boards\n", command, ats.BoardsFile)
		return ats.DemoBoards(), nil
	}
	if !dryRun {
		return nil, errNoBoardList()
	}
	_, _ = fmt.Fprintf(notes, "%s: LEADHOUND_DATA_DIR is not set; using the demo boards\n", command)
	return ats.DemoBoards(), nil
}

// fileBoards is the board file collect ats merges with the registry: --boards FILE, else
// ats-boards.yaml in LEADHOUND_DATA_DIR, else none. A file that exists but cannot be read,
// or is not a valid board list, is a usage error naming it.
func fileBoards(cfg config.Config, file string) ([]ats.Board, error) {
	if file != "" {
		b, err := ats.LoadBoardsFile(file)
		if err != nil {
			return nil, usageError("%v", err)
		}
		return b, nil
	}
	if cfg.DataDir == "" {
		return nil, nil
	}
	b, err := ats.LoadBoards(cfg.DataDir)
	switch {
	case err == nil:
		return b, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	default:
		return nil, usageError("%v", err)
	}
}

// registryBoards turns registry rows into boards. A row that is not a valid board (the
// table's checks are looser than ats.Board.Validate) is skipped with a log line and a note,
// so one bad row never stops the collect; skipped is how many.
func registryBoards(command string, rows []store.Board) (boards []ats.Board, skipped int) {
	out := make([]ats.Board, 0, len(rows))
	for _, r := range rows {
		b := ats.Board{ATS: r.ATS, Board: r.Slug, Region: r.Region, Name: r.Name}
		if err := b.Validate(); err != nil {
			slog.Warn("registry board skipped", "id", r.ID, "err", err)
			_, _ = fmt.Fprintf(notes, "%s: registry board %d skipped: %v\n", command, r.ID, err)
			skipped++
			continue
		}
		out = append(out, b)
	}
	return out, skipped
}

// mergeBoards is the registry's boards, then each file board the registry does not hold,
// one per ats, slug (ignoring case) and region. The registry's spelling of a slug wins over
// the file's (it is what the last check used); a registry board with no name takes the
// file's.
func mergeBoards(registry, file []ats.Board) []ats.Board {
	at := map[store.BoardKey]int{}
	out := make([]ats.Board, 0, len(registry)+len(file))
	for _, list := range [][]ats.Board{registry, file} {
		for _, b := range list {
			k := b.SameBoardKey()
			if i, dup := at[k]; dup {
				if strings.TrimSpace(out[i].Name) == "" {
					out[i].Name = b.Name
				}
				continue
			}
			at[k] = len(out)
			out = append(out, b)
		}
	}
	return out
}

// errNoBoardList is the usage error for a live command with no board list.
func errNoBoardList() error {
	return usageError("no board list: pass --boards FILE, or put %s in LEADHOUND_DATA_DIR (the demo boards are for --dry-run only)", ats.BoardsFile)
}

// errNoCollectBoards is collect ats's usage error when neither the registry nor a file
// names a board.
func errNoCollectBoards() error {
	return usageError("no boards: the board registry has none to collect (add them with leadhound boards import FILE --origin O), and there is no board list: pass --boards FILE, or put %s in LEADHOUND_DATA_DIR (the demo boards are for --dry-run only)", ats.BoardsFile)
}

// boardHost is the host a board's API is requested from.
func boardHost(b ats.Board) string {
	u, err := url.Parse(b.APIURL())
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// liveAllowsBoards reports whether LEADHOUND_LIVE allows every board's API host.
func liveAllowsBoards(live config.Live, boards []ats.Board) bool {
	for _, b := range boards {
		if !live.Allows(boardHost(b)) {
			return false
		}
	}
	return true
}

// boardsPerATS counts the distinct boards of each ATS.
func boardsPerATS(boards []ats.Board) map[string]int {
	seen := map[string]bool{}
	n := map[string]int{}
	for _, b := range boards {
		if !seen[b.String()] {
			seen[b.String()] = true
			n[b.ATS]++
		}
	}
	return n
}

// atsClients returns the constructor for each ATS's client. With --dry-run the client reads
// fixtures and is never paced or gated; otherwise it is live-gated, paced at
// LEADHOUND_ATS_INTERVAL and
// asks for JSON. Either way each ATS gets its own breaker, whose board budget is twice the
// list requests that ATS's boards may take (ats.MaxBoardRequests: one, Personio's retry,
// or the page cap; twice for each one's 429 retry), and whose detail budget is
// LEADHOUND_ATS_DETAIL_BUDGET. errLiveOff means LEADHOUND_LIVE does not allow every board's
// API host, so the command must not run. Bodies up to atsMaxBody are read.
func atsClients(cfg config.Config, f fetchFlags, boards []ats.Board) (func(string, fetch.Recorder) *fetch.Client, error) {
	perATS := boardsPerATS(boards)
	budgets := func(atsName string) fetch.Budgets {
		return fetch.Budgets{
			fetch.KindBoard:  2 * perATS[atsName] * ats.MaxBoardRequests(atsName),
			fetch.KindDetail: cfg.ATSDetailBudget,
		}
	}
	if f.dryRun {
		ft, err := fetch.NewFixtureTransport(f.fixtures)
		if err != nil {
			return nil, usageError("%v", err)
		}
		return func(atsName string, rec fetch.Recorder) *fetch.Client {
			return fetch.New(
				fetch.WithFixtures(ft),
				fetch.WithRecorder(rec),
				fetch.WithProfile(atsProfileFor(atsName)),
				fetch.WithMaxBody(atsMaxBody),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
				fetch.WithJitter(0),
				fetch.WithBreaker(fetch.NewBreaker(budgets(atsName))),
			)
		}, nil
	}
	if !liveAllowsBoards(cfg.Live, boards) {
		return nil, errLiveOff
	}
	return func(atsName string, rec fetch.Recorder) *fetch.Client {
		c := fetch.New(
			fetch.WithLiveGate(cfg.Live),
			fetch.WithRecorder(rec),
			fetch.WithProfile(atsProfileFor(atsName)),
			fetch.WithMaxBody(atsMaxBody),
			fetch.WithLimiter(rate.NewLimiter(rate.Every(cfg.ATSInterval), 1)),
			fetch.WithBreaker(fetch.NewBreaker(budgets(atsName))),
		)
		c.HTTP.Timeout = atsTimeout
		return c
	}, nil
}

// runCollectATS is `leadhound collect ats [--dry-run --fixtures DIR] [--boards FILE]`: every
// board once, one run row per ATS, the ATSs side by side. Live, the boards are the
// registry's (state ok, and manual boards in any state but retired; a row that is not a
// valid board is skipped with a note) merged with --boards FILE or the data directory's
// ats-boards.yaml, whose boards are collected even when the registry does not hold them;
// with neither it is a usage error. A dry run never reads the registry: --boards, the
// data-directory list, else the demo boards. Each live fetch updates its registry row as boards check does. With a profile its
// title screen drops postings before they are stored. Any recorded outcome, a stop
// included, exits 0.
func runCollectATS(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("collect ats", flag.ContinueOnError)
	var af atsFlags
	af.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: leadhound collect ats [--dry-run --fixtures DIR] [--boards FILE]")
	}
	if err := af.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	prof, haveProf, err := candidateProfileOr(cfg, "collect ats", "neutral gates")
	if err != nil {
		return err
	}
	cg := defaultCandidateGates
	var screen func(string) bool
	if haveProf {
		cg, screen = prof.Gates(), prof.TitleScreen()
	}
	var boards, fromFile []ats.Board
	if af.dryRun {
		// A dry run never reads the registry: fixtures answer only the boards a file or the
		// demo names.
		if boards, err = boardList(cfg, af.boards, "collect ats", true); err != nil {
			return err
		}
	} else {
		if fromFile, err = fileBoards(cfg, af.boards); err != nil {
			return err
		}
		// Any registry board may be on any ATS host, so live needs them all; checked before
		// the database is opened, so a live-off machine never touches it.
		if !liveAllowsBoards(cfg.Live, append(allATSHosts(), fromFile...)) {
			_, _ = fmt.Fprintf(stdout, "collect ats: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
			return nil
		}
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	if !af.dryRun {
		rows, err := s.Q().CollectBoards(ctx)
		if err != nil {
			return err
		}
		fromRegistry, _ := registryBoards("collect ats", rows)
		boards = mergeBoards(fromRegistry, fromFile)
		if len(boards) == 0 {
			return errNoCollectBoards()
		}
	}
	newClient, err := atsClients(cfg, af.fetchFlags, boards)
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "collect ats: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
		return nil
	}
	if err != nil {
		return err
	}

	c := &ats.Collector{Store: s, NewClient: newClient, Loc: loc, Fixture: af.dryRun, Gates: cg, TitleScreen: screen}
	res, runErr := c.Run(ctx, boards)
	for _, a := range res.ATS {
		if a.Paused {
			_, _ = fmt.Fprintf(stdout, "collect ats: %s run %d · %s; nothing requested\n", a.ATS, a.RunID, a.Note)
			continue
		}
		stop := a.StopReason
		if stop == "" {
			stop = "none"
		}
		_, _ = fmt.Fprintf(stdout, "collect ats: %s run %d · %d requests (%d failed) · stop %s\n  %s\n",
			a.ATS, a.RunID, a.Requests, a.Failures, stop, strings.ReplaceAll(a.Note, "\n", "\n  "))
	}
	return runErr
}
