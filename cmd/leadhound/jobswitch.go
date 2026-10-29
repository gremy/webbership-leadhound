package main

import (
	"context"
	"fmt"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The job switches (job_switch, 00023) as the scheduled jobs honour them: a job switched off
// in the console runs nothing, prints one line, records its run as switched off and exits 0,
// so its timer or its request watcher never shows a failure for it. The account is the one
// the jobs already work on, leadstore.DefaultAccountID.

// runNoteSwitchedOff is the run note (the run's result) of a job that was switched off.
const runNoteSwitchedOff = "switched off"

// jobNames are the jobs' plain names, as the console and the journal line say them.
var jobNames = map[string]string{
	leadstore.JobBatch:    "Morning batch",
	leadstore.JobSync:     "Inbox sync",
	leadstore.JobProbe:    "Site probe",
	leadstore.JobContacts: "Contacts",
	leadstore.JobResearch: "Research",
	leadstore.JobVerify:   "Email check",
}

// jobName is job's plain name (the job itself when it has none).
func jobName(job string) string {
	if n, ok := jobNames[job]; ok {
		return n
	}
	return job
}

// jobOff reports whether job is switched off for the default account, with the line the job
// prints when it is ("" when it is on). A switch that cannot be read is an error, never "on":
// the job then fails rather than running against the operator's switch.
func jobOff(ctx context.Context, lq leadstore.Queries, job string) (bool, string, error) {
	sw, err := lq.JobSwitch(ctx, leadstore.DefaultAccountID, job)
	if err != nil || sw.On {
		return false, "", err
	}
	loc, lerr := leadsZone()
	if lerr != nil {
		loc = time.UTC
	}
	return true, fmt.Sprintf("%s is switched off in the console (since %s): nothing ran.",
		jobName(job), sw.At.In(loc).Format("2006-01-02 15:04")), nil
}

// switchedOff is the gate at the top of a switched job's command: when job is off it prints
// the line, records a finished run of command and source with the note "switched off" (as a
// paused send records its run) and returns true, and the command returns nil (exit 0).
func switchedOff(ctx context.Context, s *store.Store, job, command, source string) (bool, error) {
	off, line, err := jobOff(ctx, leadstore.NewQueries(s.Pool()), job)
	if err != nil || !off {
		return false, err
	}
	q := s.Q()
	runID, err := q.StartRun(ctx, command, source)
	if err != nil {
		return false, err
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, store.RunResult{Note: runNoteSwitchedOff}); err != nil {
		return false, err
	}
	_, _ = fmt.Fprintln(stdout, line)
	return true, nil
}
