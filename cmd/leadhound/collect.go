package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["collect"] = runCollect }

// stdout is where commands print their outcome; tests replace it.
var stdout io.Writer = os.Stdout

// notes is where commands print a one-line notice that is not their outcome; tests
// replace it.
var notes io.Writer = os.Stderr

// liveOffMessage is printed, with exit 0 and no run row, when a command would need the
// network and LEADHOUND_LIVE does not allow LinkedIn.
const liveOffMessage = "live fetching is off here"

const linkedinHost = "www.linkedin.com"

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// fetchFlags are the flags every LinkedIn command shares.
type fetchFlags struct {
	dryRun   bool
	fixtures string
}

func (f *fetchFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.dryRun, "dry-run", false, "serve every request from --fixtures instead of the network")
	fs.StringVar(&f.fixtures, "fixtures", "", "fixture directory with a manifest.json (with --dry-run)")
}

func (f *fetchFlags) check() error {
	switch {
	case f.dryRun && f.fixtures == "":
		return usageError("--dry-run needs --fixtures DIR")
	case !f.dryRun && f.fixtures != "":
		return usageError("--fixtures is only read with --dry-run")
	}
	return nil
}

// parseArgs parses flags that may come before, between or after the positional
// arguments, and returns the positionals in order.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError("%v", err)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// linkedinClients returns a constructor for the run's LinkedIn client and the source its
// rows are written under. With --dry-run the client reads fixtures, is never paced or
// gated, and writes under linkedin.SourceFixture, so a dry run never reads as LinkedIn's
// data; otherwise it is live-gated, paced at LEADHOUND_LINKEDIN_INTERVAL, capped by the
// budgets and writes under linkedin.Source. errLiveOff means LEADHOUND_LIVE does not allow
// LinkedIn, so the command must not run.
func linkedinClients(cfg config.Config, f fetchFlags, budgets fetch.Budgets) (func(fetch.Recorder) *fetch.Client, string, error) {
	if f.dryRun {
		ft, err := fetch.NewFixtureTransport(f.fixtures)
		if err != nil {
			return nil, "", usageError("%v", err)
		}
		return func(rec fetch.Recorder) *fetch.Client {
			return fetch.New(
				fetch.WithFixtures(ft),
				fetch.WithRecorder(rec),
				fetch.WithProfile(fetch.LinkedInJobs),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
				fetch.WithJitter(0),
				fetch.WithBreaker(fetch.NewBreaker(budgets)),
			)
		}, linkedin.SourceFixture, nil
	}
	if !cfg.Live.Allows(linkedinHost) {
		return nil, "", errLiveOff
	}
	return func(rec fetch.Recorder) *fetch.Client {
		return fetch.New(
			fetch.WithLiveGate(cfg.Live),
			fetch.WithRecorder(rec),
			fetch.WithProfile(fetch.LinkedInJobs),
			fetch.WithLimiter(rate.NewLimiter(rate.Every(cfg.LinkedInInterval), 1)),
			fetch.WithBreaker(fetch.NewBreaker(budgets)),
		)
	}, linkedin.Source, nil
}

var errLiveOff = errors.New(liveOffMessage)

// defaultCandidateGates is the neutral profile the gates read when LEADHOUND_DATA_DIR is
// not set: English only, no country limit, any workplace.
var defaultCandidateGates = gates.CandidateGates{Languages: []string{"en"}}

// candidateProfile loads leadhound.yaml from LEADHOUND_DATA_DIR; a broken file is a usage
// error naming the problem. With no data directory it prints one note and reports ok
// false, and the command runs on the built-in demo search and neutral gates.
func candidateProfile(cfg config.Config, command string) (prof candidate.Config, ok bool, err error) {
	return candidateProfileOr(cfg, command, "the demo search and neutral gates")
}

// candidateProfileOr is candidateProfile whose note says the command uses fallback, for a
// command with no search to fall back on.
func candidateProfileOr(cfg config.Config, command, fallback string) (prof candidate.Config, ok bool, err error) {
	if cfg.DataDir == "" {
		_, _ = fmt.Fprintf(notes, "%s: LEADHOUND_DATA_DIR is not set; using %s\n", command, fallback)
		return candidate.Config{}, false, nil
	}
	prof, err = candidate.Load(cfg.DataDir)
	if err != nil {
		return candidate.Config{}, false, usageError("%v", err)
	}
	return prof, true, nil
}

// collectMatrix is the profile's matrix, or the demo's without one; each flag given
// replaces its part.
func collectMatrix(prof *candidate.Config, keywords, locations []string, maxPages int) linkedin.Matrix {
	m := linkedin.DemoMatrix()
	if prof != nil {
		m = prof.Matrix()
	}
	if len(keywords) > 0 {
		m.Keywords = keywords
	}
	if len(locations) > 0 {
		m.Locations = locations
	}
	if maxPages > 0 {
		m.MaxPages = maxPages
	}
	return m
}

// openStoreAndZone opens the app database and the LEADHOUND_TZ zone.
func openStoreAndZone(ctx context.Context, cfg config.Config) (*store.Store, *time.Location, error) {
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return nil, nil, usageError("LEADHOUND_TZ: %v", err)
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return nil, nil, err
	}
	return s, loc, nil
}

// runCollect is `leadhound collect linkedin [--dry-run --fixtures DIR] [--keyword K]...
// [--location L]... [--max-pages N]`, or `leadhound collect ats ...` (runCollectATS, when
// ats is the first argument). Any recorded outcome, a stop included, exits 0.
func runCollect(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == atsSource {
		return runCollectATS(ctx, args[1:])
	}
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	var keywords, locations stringList
	fs.Var(&keywords, "keyword", "search keywords (repeatable)")
	fs.Var(&locations, "location", "search location (repeatable)")
	maxPages := fs.Int("max-pages", 0, "pages per query on a cold start")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != linkedin.Source {
		return usageError("usage: leadhound collect linkedin [--dry-run --fixtures DIR] [--keyword K]... [--location L]... [--max-pages N] | leadhound collect ats [--dry-run --fixtures DIR] [--boards FILE]")
	}
	if err := ff.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	prof, haveProf, err := candidateProfile(cfg, "collect linkedin")
	if err != nil {
		return err
	}
	cg := defaultCandidateGates
	var profp *candidate.Config
	if haveProf {
		cg, profp = prof.Gates(), &prof
	}
	m := collectMatrix(profp, keywords, locations, *maxPages)
	newClient, source, err := linkedinClients(cfg, ff, fetch.Budgets{
		fetch.KindSearch: cfg.LinkedInSearchBudget,
		fetch.KindDetail: cfg.LinkedInDetailBudget,
	})
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "collect linkedin: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
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

	c := &linkedin.Collector{Store: s, NewClient: newClient, Loc: loc, Source: source, Gates: cg}
	res, err := c.Run(ctx, m)
	if err != nil {
		return err
	}
	stop := res.StopReason
	if stop == "" {
		stop = "none"
	}
	if res.Paused {
		_, _ = fmt.Fprintf(stdout, "collect linkedin: run %d · %s; nothing requested\n", res.RunID, res.Note)
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "collect linkedin: run %d · cold start %t · %d requests (%d failed) · stop %s\n  %s\n",
		res.RunID, res.ColdStart, res.Requests, res.Failures, stop, res.Note)
	return nil
}
