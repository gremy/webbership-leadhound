package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
)

func init() { commands["canary"] = runCanary }

// The canary's default query, until Task 4.2 reads it from leadhound.yaml.
const (
	defaultCanaryKeywords = "platform engineer"
	defaultCanaryLocation = "European Union"
)

// runCanary is `leadhound canary linkedin [--dry-run --fixtures DIR] [--keywords K
// --location L]`: one search request a day. A pass, a fail, a pause and a skip all exit 0.
func runCanary(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("canary", flag.ContinueOnError)
	var ff fetchFlags
	ff.register(fs)
	keywords := fs.String("keywords", defaultCanaryKeywords, "search keywords")
	location := fs.String("location", defaultCanaryLocation, "search location")
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
	res, err := c.Run(ctx, *keywords, *location)
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
