package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
)

func init() { commands["canary"] = runCanary }

// The canary's query when LEADHOUND_DATA_DIR is not set.
const (
	defaultCanaryKeywords = "platform engineer"
	defaultCanaryLocation = "European Union"
)

// canaryQuery is the profile's canary query, or the default without one; each flag given
// replaces its part.
func canaryQuery(prof *candidate.Config, keywords, location string) (string, string) {
	k, l := defaultCanaryKeywords, defaultCanaryLocation
	if prof != nil {
		k, l = prof.CanaryQuery()
	}
	if keywords != "" {
		k = keywords
	}
	if location != "" {
		l = location
	}
	return k, l
}

// runCanary is `leadhound canary linkedin [--dry-run --fixtures DIR] [--keywords K
// --location L]`: one search request a day. A pass, a fail, a pause and a skip all exit 0.
func runCanary(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	keywords := fs.String("keywords", "", "search keywords (default: the profile's canary, else "+defaultCanaryKeywords+")")
	location := fs.String("location", "", "search location (default: the profile's canary, else "+defaultCanaryLocation+")")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || pos[0] != linkedin.Source {
		return usageError("usage: leadhound canary linkedin [--dry-run --fixtures DIR] [--keywords K --location L]")
	}
	if err := ff.check(); err != nil {
		return err
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	prof, haveProf, err := candidateProfile(cfg, "canary linkedin")
	if err != nil {
		return err
	}
	var profp *candidate.Config
	if haveProf {
		profp = &prof
	}
	kw, where := canaryQuery(profp, *keywords, *location)
	// Two: one request and, after a 429, its one retry.
	newClient, source, err := linkedinClients(cfg, ff, fetch.Budgets{fetch.KindCanary: 2})
	if errors.Is(err, errLiveOff) {
		_, _ = fmt.Fprintf(stdout, "canary linkedin: %s (LEADHOUND_LIVE is %s); nothing requested\n", liveOffMessage, cfg.Live)
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

	c := &linkedin.Canary{Store: s, NewClient: newClient, Loc: loc, Source: source}
	res, err := c.Run(ctx, kw, where)
	if err != nil {
		return err
	}
	switch {
	case res.Paused:
		_, _ = fmt.Fprintf(stdout, "canary linkedin: run %d · %s; nothing requested\n", res.RunID, res.Note)
	case res.Skipped:
		_, _ = fmt.Fprintln(stdout, "canary linkedin: already ran today; nothing requested")
	case res.Passed:
		_, _ = fmt.Fprintf(stdout, "canary linkedin: run %d · passed · %s\n", res.RunID, res.Note)
	default:
		_, _ = fmt.Fprintf(stdout, "canary linkedin: run %d · failed · stop %s · %s\n", res.RunID, res.StopReason, res.Note)
	}
	return nil
}
