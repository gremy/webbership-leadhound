package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/eval"
)

func init() { commands["eval"] = runEval }

// runEval is `leadhound eval --golden DIR [--runs 3] [--decider fake|jev] [--config DIR]`.
// The profile comes from --config, else LEADHOUND_DATA_DIR, else the golden directory
// (examples/ holds the demo profile beside the demo set). It reads no database and writes
// nothing. The decider is the fake unless --decider jev is given; a jev eval is uncapped,
// so it prints how many calls it will make before the first one, and --runs stops at
// eval.MaxRuns.
func runEval(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	golden := fs.String("golden", "", "golden directory with jobs.jsonl (or golden.demo.jsonl) and its raw pages")
	runs := fs.Int("runs", 3, "how many times to run the set")
	deciderName := fs.String("decider", "", "fake or jev (default fake; jev is live and paid)")
	configDir := fs.String("config", "", "directory holding leadhound.yaml (default LEADHOUND_DATA_DIR)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 || *golden == "" {
		return usageError("usage: leadhound eval --golden DIR [--runs 3] [--decider fake|jev] [--config DIR]")
	}
	if *runs < 1 || *runs > eval.MaxRuns {
		return usageError("--runs must be 1 to %d", eval.MaxRuns)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	dir := *configDir
	if dir == "" {
		dir = cfg.DataDir
	}
	if dir == "" {
		dir = *golden
	}
	prof, err := loadProfileDir(dir)
	if err != nil {
		return err
	}
	jobs, file, err := eval.Load(*golden)
	if errors.Is(err, eval.ErrNoGolden) {
		return usageError("%v", err)
	}
	if err != nil {
		return err
	}
	decider, err := pickDecider(cfg, *deciderName, "eval")
	if err != nil {
		return err
	}
	announceEval(decider, len(jobs), *runs)
	rep, err := eval.Run(ctx, *golden, file, jobs, eval.Options{Runs: *runs, Decider: decider, Profile: prof, Now: time.Now()})
	if err != nil {
		return err
	}
	return rep.Write(stdout)
}

// announceEval prints, before a live eval's first call, how many calls it will make.
func announceEval(d ai.Decider, jobs, runs int) {
	if _, ok := d.(*ai.Jev); !ok {
		return
	}
	_, _ = fmt.Fprintf(notes, "eval: %s will make %d calls (%d jobs × %d runs, plus one retry for each request too long), uncapped\n",
		d.Name(), eval.PlannedCalls(jobs, runs), jobs, runs)
}
