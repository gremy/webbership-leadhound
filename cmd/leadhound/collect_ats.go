package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
)

// atsSource is the positional argument that selects the ATS collect: `collect ats`.
const atsSource = "ats"

// atsInterval is the pause between two requests to one ATS's API.
const atsInterval = 2 * time.Second

// atsMaxBody is the largest board answer read: one request returns every posting with its
// full description, which can run past fetch's default.
const atsMaxBody = 20 << 20

// atsProfile asks the ATS APIs for JSON and treats no response as blocked: they are public
// APIs, so a 403 is an error for one board, not a reason to stop.
var atsProfile = fetch.Profile{Name: "ats-json", Header: http.Header{"Accept": {"application/json"}}}

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

// errNoBoardList is the usage error for a live command with no board list.
func errNoBoardList() error {
	return usageError("no board list: pass --boards FILE, or put %s in LEADHOUND_DATA_DIR (the demo boards are for --dry-run only)", ats.BoardsFile)
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
// fixtures and is never paced or gated; otherwise it is live-gated, paced at atsInterval and
// asks for JSON. Either way each ATS gets its own breaker, whose board budget is twice that
// ATS's boards (room for each one's 429 retry). errLiveOff means LEADHOUND_LIVE does not allow
// every board's API host, so the command must not run. Bodies up to atsMaxBody are read.
func atsClients(cfg config.Config, f fetchFlags, boards []ats.Board) (func(string, fetch.Recorder) *fetch.Client, error) {
	perATS := boardsPerATS(boards)
	budgets := func(atsName string) fetch.Budgets { return fetch.Budgets{fetch.KindBoard: 2 * perATS[atsName]} }
	if f.dryRun {
		ft, err := fetch.NewFixtureTransport(f.fixtures)
		if err != nil {
			return nil, usageError("%v", err)
		}
		return func(atsName string, rec fetch.Recorder) *fetch.Client {
			return fetch.New(
				fetch.WithFixtures(ft),
				fetch.WithRecorder(rec),
				fetch.WithProfile(atsProfile),
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
		return fetch.New(
			fetch.WithLiveGate(cfg.Live),
			fetch.WithRecorder(rec),
			fetch.WithProfile(atsProfile),
			fetch.WithMaxBody(atsMaxBody),
			fetch.WithLimiter(rate.NewLimiter(rate.Every(atsInterval), 1)),
			fetch.WithBreaker(fetch.NewBreaker(budgets(atsName))),
		)
	}, nil
}

// runCollectATS is `leadhound collect ats [--dry-run --fixtures DIR] [--boards FILE]`: every
// board once, one run row per ATS. Any recorded outcome, a stop included, exits 0.
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
	boards, err := boardList(cfg, af.boards, "collect ats", af.dryRun)
	if err != nil {
		return err
	}
	prof, haveProf, err := candidateProfileOr(cfg, "collect ats", "neutral gates")
	if err != nil {
		return err
	}
	cg := defaultCandidateGates
	if haveProf {
		cg = prof.Gates()
	}
	newClient, err := atsClients(cfg, af.fetchFlags, boards)
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "collect ats: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
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

	c := &ats.Collector{Store: s, NewClient: newClient, Loc: loc, Fixture: af.dryRun, Gates: cg}
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
