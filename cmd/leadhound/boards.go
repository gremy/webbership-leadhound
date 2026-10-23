package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/fetch"
)

func init() { commands["boards"] = runBoards }

// verifyRecorder records nothing. boards verify is read-only and opens no database, but
// fetch.Client refuses to fetch without a Recorder, so it gets this one.
type verifyRecorder struct{}

func (verifyRecorder) Record(context.Context, fetch.Record) error { return nil }

// errBoardsNotOK makes boards verify exit 1 when any board is not ok, so a script can tell.
var errBoardsNotOK = errors.New("boards verify: not every board is ok")

// runBoards is `leadhound boards verify [--boards FILE] [--dry-run --fixtures DIR]`: one GET
// per board, read-only, one line per board. It opens no database and writes nothing. Every
// board ok exits 0; any other exits 1; live fetching off exits 0 with nothing requested.
func runBoards(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("boards", flag.ContinueOnError)
	var af atsFlags
	af.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != "verify" {
		return usageError("usage: leadhound boards verify [--boards FILE] [--dry-run --fixtures DIR]")
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
			return fetch.New(fetch.WithLiveGate(cfg.Live), fetch.WithRecorder(verifyRecorder{}), fetch.WithProfile(atsProfile), fetch.WithMaxBody(atsMaxBody),
				fetch.WithLimiter(rate.NewLimiter(rate.Every(atsInterval), 1)))
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
