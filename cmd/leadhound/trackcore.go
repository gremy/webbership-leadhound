package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/store"
)

// trackRequest is one status change, as `leadhound track` takes it.
type trackRequest struct {
	JobID       int64
	Action      string // one of trackActions
	Reason      string
	RuleField   string // with skip only; "" for none
	RulePattern string
}

// trackResult is what a status change did. Job is set whenever the job exists, refusals
// included.
type trackResult struct {
	Job      store.JobSummary
	From, To string // status before and after ("maybe" keeps To == From)
	RuleID   int64  // > 0 when a skip rule was added
	NoOp     bool   // already in that status; nothing written
	Noted    bool   // maybe: the reason was written, the status stays
}

// errPastApplying refuses a skip of a job already applied to, interviewing or offered.
var errPastApplying = errors.New("track: already past applying")

// ruleGoesWithSkip refuses a rule given with any action but skip.
func ruleGoesWithSkip(action, rule string) error {
	if rule != "" && action != "skip" {
		return usageError("--rule goes with skip only")
	}
	return nil
}

// parseTrackRule splits --rule's field=PATTERN and checks both; "" is no rule.
func parseTrackRule(flag string) (field, pattern string, err error) {
	if flag == "" {
		return "", "", nil
	}
	field, pattern, ok := strings.Cut(flag, "=")
	switch {
	case !ok || !slices.Contains(store.RuleFields, field):
		return "", "", usageError("--rule must be field=PATTERN with field one of %s", strings.Join(store.RuleFields, ", "))
	case strings.TrimSpace(pattern) == "":
		return "", "", usageError("--rule needs a pattern after %s=", field)
	}
	if _, err := regexp.Compile("(?i)" + pattern); err != nil {
		return "", "", usageError("--rule pattern does not compile: %v", err)
	}
	return field, pattern, nil
}

// trackJob writes req's status change in one transaction that first locks the job, so two
// changes of the same job at once run one after the other and the second sees the first.
// A skip past applying is errPastApplying and an unknown job store.ErrNotFound; neither
// writes anything, and neither does a change to the status the job already has (NoOp).
func trackJob(ctx context.Context, s *store.Store, req trackRequest) (trackResult, error) {
	var res trackResult
	if err := ruleGoesWithSkip(req.Action, req.RuleField); err != nil {
		return res, err
	}
	jobID := req.JobID
	err := s.InTx(ctx, func(tq store.Queries) error {
		if err := tq.LockJob(ctx, jobID); err != nil {
			return err
		}
		sum, err := tq.JobSummaryByID(ctx, jobID)
		if err != nil {
			return err
		}
		res.Job, res.From = sum, sum.Status

		status := req.Action
		switch req.Action {
		case "maybe":
			status = sum.Status // the status stays; only the reason is written
		case "skip":
			status = "skipped"
			if slices.Contains(pastApplying, sum.Status) {
				return fmt.Errorf("%w: already %s; use withdrawn", errPastApplying, sum.Status)
			}
		}
		res.To = status
		if req.Action != "maybe" && status == sum.Status {
			res.NoOp = true
			return nil
		}
		why := strings.TrimSpace(req.Reason)
		if req.Action == "maybe" {
			why = strings.TrimSpace("maybe: " + why)
			why = strings.TrimSuffix(why, ":")
		}

		var rid *int64
		if req.RuleField != "" {
			ruleReason := why
			if ruleReason == "" {
				ruleReason = fmt.Sprintf("skipped job %d", jobID)
			}
			id, err := tq.AddRule(ctx, "skip", req.RuleField, req.RulePattern, ruleReason, &jobID)
			if err != nil {
				return err
			}
			res.RuleID, rid = id, &id
		}
		if _, err := tq.InsertStatusEvent(ctx, jobID, status, why, "gremy", rid); err != nil {
			return err
		}
		res.Noted = req.Action == "maybe"
		return nil
	})
	if err != nil {
		res.RuleID = 0 // rolled back
	}
	return res, err
}

// trackRefusal is the usage error for a refusal from trackJob, in the words both track
// and the console show; an error that is not a refusal comes back as it is.
func trackRefusal(err error, jobID int64, res trackResult) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return usageError("job:%d does not exist", jobID)
	case errors.Is(err, errPastApplying):
		return usageError("already %s; use withdrawn", res.From)
	case errors.Is(err, store.ErrInvalidRule):
		return usageError("%v", err)
	}
	return err
}

// trackOutcomeLine is the line track prints, and the console shows, for what a mark did.
func trackOutcomeLine(res trackResult) string {
	switch {
	case res.NoOp:
		return fmt.Sprintf("already %s; nothing changed", res.To)
	case res.Noted:
		return fmt.Sprintf("noted; status stays %s", res.To)
	case res.RuleID > 0:
		return fmt.Sprintf("%s → %s; rule %d added", res.From, res.To, res.RuleID)
	}
	return fmt.Sprintf("%s → %s", res.From, res.To)
}
