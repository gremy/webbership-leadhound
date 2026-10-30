package main

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["switches"] = runLeadsSwitches }

const switchesUsage = "usage: leadhound leads switches list | set JOB on|off [--reason R] | convert-send-env --was VALUE (JOB is one of " +
	"batch, sync, probe, contacts, research, verify)"

// convertSendReason is the send pause the one-time conversion writes when the old server
// switch (LEADHOUND_LEADS_SEND) was not on.
const convertSendReason = "Sending was off on the server (LEADHOUND_LEADS_SEND) before the console switches"

// runLeadsSwitches is `leads switches <verb>`: the on/off switches the console's Controls
// page shows, from the command line. Sending is the send pause (leads send pause|resume);
// every other job has a job switch. convert-send-env is install.sh's one-time conversion of
// the old LEADHOUND_LEADS_SEND line into the send pause.
func runLeadsSwitches(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("%s", switchesUsage)
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "list":
		if len(rest) != 0 {
			return usageError("%s", switchesUsage)
		}
		return switchesList(ctx)
	case "set":
		return switchesSet(ctx, rest)
	case "convert-send-env":
		return switchesConvertSendEnv(ctx, rest)
	}
	return usageError("unknown switches verb %q; %s", verb, switchesUsage)
}

// switchesOpen opens the database only: no leads.yaml is read, so install.sh can run the
// conversion whatever state the data dir is in. dataDir is LEADHOUND_DATA_DIR ("" when unset).
func switchesOpen(ctx context.Context) (lq leadstore.Queries, loc *time.Location, dataDir string, done func(), err error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return leadstore.Queries{}, nil, "", nil, err
	}
	if loc, err = time.LoadLocation(cfg.TZ); err != nil {
		return leadstore.Queries{}, nil, "", nil, usageError("LEADHOUND_TZ: %v", err)
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return leadstore.Queries{}, nil, "", nil, err
	}
	return leadstore.NewQueries(s.Pool()), loc, cfg.DataDir, s.Close, nil
}

// requestOnSwitchOn writes the request file of the unit that runs job, when job has one
// (runRequestJobs), and reports whether it has. Research has no timer: its request watcher is
// its only trigger, so without this the searches a star queued while it was off would wait
// for the next star or Look again.
func requestOnSwitchOn(dataDir, job string, at time.Time) (bool, error) {
	for action, j := range runRequestJobs {
		if j == job {
			return true, writeRunRequest(dataDir, action, at)
		}
	}
	return false, nil
}

// requestedWords ends a switch-on line for a job with a request file: the queued work starts
// now, or (the file could not be written) with the job's next request.
func requestedWords(job string, err error) string {
	if err != nil {
		return fmt.Sprintf(" What was queued while it was off could not be started now: it starts with %s's next request.", strings.ToLower(jobName(job)))
	}
	return " What was queued while it was off starts now."
}

// switchLine is one switch in plain words: "<Name>: on|off · since <time> · <reason>".
func switchLine(name string, on bool, at time.Time, reason string, loc *time.Location) string {
	state := "off"
	if on {
		state = "on"
	}
	line := name + ": " + state
	if at.IsZero() {
		line += " · never switched"
	} else {
		line += " · since " + at.In(loc).Format("2006-01-02 15:04")
	}
	if reason != "" {
		line += " · " + reason
	}
	return line
}

func switchesList(ctx context.Context) error {
	lq, loc, _, done, err := switchesOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	paused, reason, at, err := lq.SendPaused(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, switchLine("Sending", !paused, at, reason, loc))
	sw, err := lq.JobSwitches(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return err
	}
	for _, s := range sw {
		_, _ = fmt.Fprintln(stdout, switchLine(jobName(s.Job), s.On, s.At, s.Reason, loc))
	}
	return nil
}

func switchesSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads switches set", flag.ContinueOnError)
	reason := fs.String("reason", "", "why the job is switched (kept with the switch)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || (pos[1] != "on" && pos[1] != "off") {
		return usageError("%s", switchesUsage)
	}
	job, on := pos[0], pos[1] == "on"
	if !slices.Contains(leadstore.Jobs, job) {
		return usageError("unknown job %q; %s", job, switchesUsage)
	}
	lq, _, dataDir, done, err := switchesOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	changed, err := lq.SetJobSwitch(ctx, leadstore.DefaultAccountID, job, on, strings.TrimSpace(*reason), leadstore.ActorGremy)
	if err != nil {
		return err
	}
	switch {
	case !changed:
		_, _ = fmt.Fprintf(stdout, "%s is already %s; nothing changed.\n", jobName(job), pos[1])
	case on:
		line := jobName(job) + " is switched on: its next run goes ahead."
		if has, err := requestOnSwitchOn(dataDir, job, leadsNow()); has {
			line += requestedWords(job, err)
		}
		_, _ = fmt.Fprintln(stdout, line)
	default:
		_, _ = fmt.Fprintf(stdout, "%s is switched off: its runs do nothing until you switch it on.\n", jobName(job))
	}
	return nil
}

// sendEnvValue is the old LEADHOUND_LEADS_SEND value as install.sh's env_value reads it:
// quotes left out and blanks trimmed. Only exactly "on" was on; empty (unset) was off.
func sendEnvValue(was string) string {
	return strings.TrimSpace(strings.NewReplacer(`"`, "", `'`, "").Replace(was))
}

// switchesConvertSendEnv is install.sh's one-time conversion: when the old server switch
// was not on, sending starts paused (as actor system), so the deploy that retires the env
// line never starts sending by itself. Already paused, or was on: nothing is written.
func switchesConvertSendEnv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads switches convert-send-env", flag.ContinueOnError)
	was := fs.String("was", "", "the LEADHOUND_LEADS_SEND value the server had (empty when unset)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == "was" })
	if len(pos) != 0 || !set {
		return usageError("%s", switchesUsage)
	}
	if sendEnvValue(*was) == "on" {
		_, _ = fmt.Fprintln(stdout, "Sending was on on the server: the send switch stays as it is.")
		return nil
	}
	lq, _, _, done, err := switchesOpen(ctx)
	if err != nil {
		return err
	}
	defer done()
	sw, err := setSendPause(ctx, lq, true, convertSendReason, leadstore.ActorSystem)
	if err != nil {
		return err
	}
	if !sw.Changed {
		_, _ = fmt.Fprintln(stdout, "Sending was off on the server and is already paused: nothing changed.")
		return nil
	}
	_, _ = fmt.Fprintln(stdout, "Sending was off on the server: sending is now paused; resume it on the Controls page.")
	return nil
}
