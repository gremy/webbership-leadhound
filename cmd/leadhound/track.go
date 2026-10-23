package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"regexp"
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
	var ruleField, rulePattern string
	if *ruleFlag != "" {
		if action != "skip" {
			return usageError("--rule goes with skip only")
		}
		var ok bool
		ruleField, rulePattern, ok = strings.Cut(*ruleFlag, "=")
		switch {
		case !ok || !slices.Contains(store.RuleFields, ruleField):
			return usageError("--rule must be field=PATTERN with field one of %s", strings.Join(store.RuleFields, ", "))
		case strings.TrimSpace(rulePattern) == "":
			return usageError("--rule needs a pattern after %s=", ruleField)
		}
		if _, err := regexp.Compile("(?i)" + rulePattern); err != nil {
			return usageError("--rule pattern does not compile: %v", err)
		}
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
	sum, err := q.JobSummaryByID(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) {
		return usageError("job:%d does not exist", jobID)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s → %s · %s (job:%d)\n", label, sum.Title, sum.Company, jobID)

	status := action
	switch action {
	case "maybe":
		status = sum.Status // the status stays; only the reason is written
	case "skip":
		status = "skipped"
		if slices.Contains(pastApplying, sum.Status) {
			return usageError("already %s; use withdrawn", sum.Status)
		}
	}
	if action != "maybe" && status == sum.Status {
		_, _ = fmt.Fprintf(stdout, "already %s; nothing changed\n", status)
		return nil
	}
	why := strings.TrimSpace(*reason)
	if action == "maybe" {
		why = strings.TrimSpace("maybe: " + why)
		why = strings.TrimSuffix(why, ":")
	}

	var ruleID int64
	err = s.InTx(ctx, func(tq store.Queries) error {
		var rid *int64
		if ruleField != "" {
			ruleReason := why
			if ruleReason == "" {
				ruleReason = fmt.Sprintf("skipped job %d", jobID)
			}
			id, err := tq.AddRule(ctx, "skip", ruleField, rulePattern, ruleReason, &jobID)
			if err != nil {
				return err
			}
			ruleID, rid = id, &id
		}
		_, err := tq.InsertStatusEvent(ctx, jobID, status, why, "gremy", rid)
		return err
	})
	if errors.Is(err, store.ErrInvalidRule) {
		return usageError("%v", err)
	}
	if err != nil {
		return err
	}
	switch {
	case action == "maybe":
		_, _ = fmt.Fprintf(stdout, "noted; status stays %s\n", status)
	case ruleID > 0:
		_, _ = fmt.Fprintf(stdout, "%s → %s; rule %d added\n", sum.Status, status, ruleID)
	default:
		_, _ = fmt.Fprintf(stdout, "%s → %s\n", sum.Status, status)
	}
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
	rows, err := s.Q().TrackList(ctx, *status)
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
