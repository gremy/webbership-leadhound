package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["track"] = runTrack }

const trackUsage = "usage: leadhound track <n|job:ID> applied|interviewing|offer|rejected|withdrawn|maybe|skip [--reason TEXT] [--rule company|title|description|location=PATTERN] [--day YYYY-MM-DD]\n" +
	"       leadhound track list [--status S|all]"

// trackActions are the actions track takes, each the status it writes except maybe and
// skip.
var trackActions = []string{"applied", "interviewing", "offer", "rejected", "withdrawn", "maybe", "skip"}

// pastApplying are the statuses a skip may no longer replace.
var pastApplying = []string{"applied", "interviewing", "offer"}

// runTrack is `leadhound track <n|job:ID> <action> …` and `leadhound track list`. A
// number resolves through the latest sent brief (or the brief of --day), and the job it
// names is printed before anything is written. A change that is refused, or a number the
// brief did not have, is a usage error (exit 2); a no-op says so and exits 0.
func runTrack(ctx context.Context, args []string) error {
	if len(args) > 0 && args[0] == "list" {
		return runTrackList(ctx, args[1:])
	}
	fs := flag.NewFlagSet("track", flag.ContinueOnError)
	reason := fs.String("reason", "", "why (stored with the status)")
	ruleFlag := fs.String("rule", "", "with skip: also add a skip rule, field=PATTERN")
	dayFlag := fs.String("day", "", "resolve the number through this day's brief instead of the latest sent one")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || !slices.Contains(trackActions, pos[1]) {
		return usageError("%s", trackUsage)
	}
	ref, action := pos[0], pos[1]
	if err := ruleGoesWithSkip(action, *ruleFlag); err != nil {
		return err
	}
	ruleField, rulePattern, err := parseTrackRule(*ruleFlag)
	if err != nil {
		return err
	}
	if *dayFlag != "" {
		if _, err := time.Parse(time.DateOnly, *dayFlag); err != nil {
			return usageError("--day must be YYYY-MM-DD, not %q", *dayFlag)
		}
	}

	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	s, _, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()

	jobID, label, err := resolveJobRef(ctx, q, ref, *dayFlag)
	if err != nil {
		return err
	}
	res, err := trackJob(ctx, s, trackRequest{JobID: jobID, Action: action, Reason: *reason, RuleField: ruleField, RulePattern: rulePattern})
	if res.Job.JobID != 0 {
		_, _ = fmt.Fprintf(stdout, "%s → %s · %s (job:%d)\n", label, res.Job.Title, res.Job.Company, jobID)
	}
	if err != nil {
		return trackRefusal(err, jobID, res)
	}
	_, _ = fmt.Fprintln(stdout, trackOutcomeLine(res))
	return nil
}

// resolveJobRef turns "job:ID" into the id, or a brief number into the job it named in
// the latest sent brief (or in day's brief, which must have been sent too). label is how
// the printed line names the reference.
func resolveJobRef(ctx context.Context, q store.Queries, ref, day string) (jobID int64, label string, err error) {
	if rest, ok := strings.CutPrefix(ref, "job:"); ok {
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id <= 0 {
			return 0, "", usageError("%q is not a job reference; use job:ID", ref)
		}
		return id, ref, nil
	}
	n, err := strconv.Atoi(ref)
	if err != nil || n <= 0 {
		return 0, "", usageError("%q is neither a brief number nor job:ID", ref)
	}
	var b store.BriefLog
	if day != "" {
		b, err = q.BriefByDay(ctx, day)
		if errors.Is(err, store.ErrNotFound) {
			return 0, "", usageError("there is no brief for %s", day)
		}
		if err == nil && b.SentAt == nil {
			return 0, "", usageError("the brief for %s was never sent, so its numbers mean nothing yet", day)
		}
	} else {
		b, err = q.LatestSentBrief(ctx)
		if errors.Is(err, store.ErrNotFound) {
			return 0, "", usageError("no brief has been sent yet; use job:ID")
		}
	}
	if err != nil {
		return 0, "", err
	}
	if n > len(b.JobIDs) {
		return 0, "", usageError("the brief of %s listed %d jobs; there is no number %d", b.Day, len(b.JobIDs), n)
	}
	return b.JobIDs[n-1], strconv.Itoa(n), nil
}

// runTrackList is `leadhound track list [--status S|all]`: one line per job with a
// status other than new (or only S, or all), latest change first.
func runTrackList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("track list", flag.ContinueOnError)
	status := fs.String("status", "", "only this status, or all (default: every status but new)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("%s", trackUsage)
	}
	if *status != "" && *status != "all" && !slices.Contains(store.JobStatuses, *status) {
		return usageError("--status must be all or one of %s", strings.Join(store.JobStatuses, ", "))
	}
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	rows, _, err := s.Q().TrackList(ctx, *status, 0)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(stdout, "nothing tracked")
		return nil
	}
	for _, r := range rows {
		line := fmt.Sprintf("job:%d\t%s · %s\t%s\tsince %s", r.JobID, r.Title, r.Company, r.Status, r.Since.In(loc).Format("2006-01-02 15:04"))
		if r.Reason != "" {
			line += "\t" + r.Reason
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	return nil
}
