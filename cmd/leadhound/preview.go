package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/gremy/webbership-leadhound/internal/preview"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["preview"] = runPreview }

const previewUsage = "usage: leadhound preview [--after-fetch] [--allow-fake]"

// runPreview is `leadhound preview [--after-fetch] [--allow-fake]`: the jobs part of today's
// brief, composed now exactly as the brief composes it (the same live recheck and leave
// out of what is still unchecked), written to LEADHOUND_DATA_DIR/preview.json for the
// console's results page instead of being mailed. It never writes brief_log, never sends,
// never shortlists and never writes the outbox; it is its own command so that only
// leadhound-brief.service ever runs `brief`.
//
// --after-fetch is set by leadhound-fetch.service only: the run row's source is then
// "fetch", so the console tells the fetch's own preview from a separate Show results now.
func runPreview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	afterFetch := fs.Bool("after-fetch", false, "mark the run as the fetch unit's preview (set by leadhound-fetch.service)")
	allowFake := fs.Bool("allow-fake", false, "rank with the fake decider's verdicts (development only)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", previewUsage)
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL", "LEADHOUND_DATA_DIR")
	if err != nil {
		return err
	}
	prof, err := loadProfileDir(cfg.DataDir)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return usageError("LEADHOUND_TZ: %v", err)
	}

	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()
	source := ""
	if *afterFetch {
		source = previewFetchSource
	}
	runID, err := q.StartRun(ctx, "preview", source)
	if err != nil {
		return err
	}
	note := "did not finish"
	defer func() {
		_ = q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{Note: note})
	}()

	in, err := composeNow(ctx, s, cfg, prof, loc, time.Now().In(loc), *allowFake, true)
	if err != nil {
		return err
	}
	if err := preview.Write(cfg.DataDir, preview.FromInput(in, time.Now())); err != nil {
		return err
	}
	listed := len(in.Apply) + len(in.YourCall)
	counts := fmt.Sprintf("%d listed · %d apply · %d your call", listed, len(in.Apply), len(in.YourCall))
	note = counts
	_, _ = fmt.Fprintf(stdout, "preview: %s · written to %s\n", counts, filepath.Join(cfg.DataDir, preview.FileName))
	return nil
}
