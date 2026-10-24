//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// trackStore empties the test database and opens a Store as the app role.
func trackStore(t *testing.T) *store.Store {
	t.Helper()
	storetest.ResetDB(t)
	s, err := store.Open(context.Background(), storetest.AppDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping integration test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// seedStatuses creates one job per status in store.JobStatuses and gives each its status
// through a status event, except new, which gets none. It returns the job ids by status.
func seedStatuses(t *testing.T, s *store.Store) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	q := s.Q()
	companyID, err := q.UpsertCompany(ctx, "Contoso", "https://contoso.example", "")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for i, st := range store.JobStatuses {
		id, err := q.UpsertJob(ctx, store.JobUpsert{
			Source: "greenhouse", SourceJobID: fmt.Sprintf("contoso/%d", i),
			CompanyID: companyID, Title: "Go Engineer " + st, URL: fmt.Sprintf("https://boards.example/%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		if st != "new" {
			if _, err := q.InsertStatusEvent(ctx, id, st, "seed", "gremy", nil); err != nil {
				t.Fatal(err)
			}
		}
		ids[st] = id
	}
	return ids
}

func jobEvents(t *testing.T, jobID int64) int {
	t.Helper()
	return dbCount(t, fmt.Sprintf(`SELECT count(*) FROM status_event WHERE job_id = %d`, jobID))
}

// TestTrackCoreGrid: every starting status × every action, with the result and the rows
// written.
func TestTrackCoreGrid(t *testing.T) {
	type action struct {
		name, action string
		rule         bool
	}
	actions := []action{
		{"applied", "applied", false}, {"interviewing", "interviewing", false}, {"offer", "offer", false},
		{"rejected", "rejected", false}, {"withdrawn", "withdrawn", false}, {"maybe", "maybe", false},
		{"skip", "skip", false}, {"skip+rule", "skip", true},
	}
	ctx := context.Background()
	for _, a := range actions {
		t.Run(a.name, func(t *testing.T) {
			s := trackStore(t)
			ids := seedStatuses(t, s)
			for _, from := range store.JobStatuses {
				jobID := ids[from]
				seeded := jobEvents(t, jobID)
				req := trackRequest{JobID: jobID, Action: a.action, Reason: "grid"}
				if a.rule {
					req.RuleField, req.RulePattern = "company", "^contoso"
				}
				rulesBefore := dbCount(t, `SELECT count(*) FROM rule`)
				res, err := trackJob(ctx, s, req)
				written := jobEvents(t, jobID) - seeded
				rulesAdded := dbCount(t, `SELECT count(*) FROM rule`) - rulesBefore
				if res.Job.JobID != jobID || res.From != from {
					t.Errorf("%s on %s: Job %d From %q, want %d %q", a.name, from, res.Job.JobID, res.From, jobID, from)
				}

				to := a.action
				switch a.action {
				case "maybe":
					to = from
				case "skip":
					to = "skipped"
				}
				switch {
				case a.action == "skip" && slices.Contains(pastApplying, from):
					if !errors.Is(err, errPastApplying) || written != 0 || rulesAdded != 0 {
						t.Errorf("%s on %s: err %v, %d events, %d rules; want errPastApplying and nothing written", a.name, from, err, written, rulesAdded)
					}
				case a.action != "maybe" && to == from:
					if err != nil || !res.NoOp || written != 0 || rulesAdded != 0 {
						t.Errorf("%s on %s: err %v, NoOp %v, %d events, %d rules; want a no-op", a.name, from, err, res.NoOp, written, rulesAdded)
					}
				default:
					if err != nil || res.NoOp || res.To != to || written != 1 {
						t.Fatalf("%s on %s: err %v, NoOp %v, To %q, %d events; want one event to %s", a.name, from, err, res.NoOp, res.To, written, to)
					}
					st, reason, actor := currentStatus(t, jobID)
					wantReason := "grid"
					if a.action == "maybe" {
						wantReason = "maybe: grid"
					}
					if st != to || reason != wantReason || actor != "gremy" || res.Noted != (a.action == "maybe") {
						t.Errorf("%s on %s: wrote %s %q %s, Noted %v", a.name, from, st, reason, actor, res.Noted)
					}
					wantRules := 0
					if a.rule {
						wantRules = 1
					}
					if rulesAdded != wantRules || (res.RuleID > 0) != a.rule {
						t.Errorf("%s on %s: %d rules added, RuleID %d", a.name, from, rulesAdded, res.RuleID)
					}
					if a.rule {
						if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM status_event e JOIN rule r ON r.id = e.rule_id
							WHERE e.job_id = %d AND r.id = %d AND r.created_from_job_id = %d AND r.reason = 'grid'`, jobID, res.RuleID, jobID)); got != 1 {
							t.Errorf("%s on %s: event linked to its rule = %d, want 1", a.name, from, got)
						}
					}
				}
			}
		})
	}
}

// TestTrackCoreSurprises: the cells worth naming on their own.
func TestTrackCoreSurprises(t *testing.T) {
	ctx := context.Background()
	s := trackStore(t)
	ids := seedStatuses(t, s)

	// skip on skipped with a rule is a no-op and adds no rule.
	res, err := trackJob(ctx, s, trackRequest{JobID: ids["skipped"], Action: "skip", RuleField: "title", RulePattern: "go"})
	if err != nil || !res.NoOp || res.RuleID != 0 || dbCount(t, `SELECT count(*) FROM rule`) != 0 {
		t.Errorf("skip+rule on skipped: %+v, %v", res, err)
	}
	// maybe on skipped writes one row; the status stays skipped.
	before := jobEvents(t, ids["skipped"])
	res, err = trackJob(ctx, s, trackRequest{JobID: ids["skipped"], Action: "maybe", Reason: "  second look "})
	if err != nil || !res.Noted || res.To != "skipped" || jobEvents(t, ids["skipped"]) != before+1 {
		t.Errorf("maybe on skipped: %+v, %v", res, err)
	}
	if st, reason, _ := currentStatus(t, ids["skipped"]); st != "skipped" || reason != "maybe: second look" {
		t.Errorf("after maybe: %s %q", st, reason)
	}
	// maybe with no reason writes "maybe".
	if _, err := trackJob(ctx, s, trackRequest{JobID: ids["shortlisted"], Action: "maybe"}); err != nil {
		t.Fatal(err)
	}
	if st, reason, _ := currentStatus(t, ids["shortlisted"]); st != "shortlisted" || reason != "maybe" {
		t.Errorf("bare maybe: %s %q", st, reason)
	}
	// applied on skipped is allowed.
	res, err = trackJob(ctx, s, trackRequest{JobID: ids["skipped"], Action: "applied"})
	if err != nil || res.From != "skipped" || res.To != "applied" || res.NoOp {
		t.Errorf("applied on skipped: %+v, %v", res, err)
	}
	// An unknown job is ErrNotFound, with no summary.
	res, err = trackJob(ctx, s, trackRequest{JobID: 999, Action: "applied"})
	if !errors.Is(err, store.ErrNotFound) || res.Job.JobID != 0 {
		t.Errorf("unknown job: %+v, %v", res, err)
	}
	// A rule with any action but skip is refused before anything is read.
	if _, err := trackJob(ctx, s, trackRequest{JobID: ids["new"], Action: "applied", RuleField: "title", RulePattern: "go"}); !errors.Is(err, errUsage) {
		t.Errorf("rule with applied: %v", err)
	}
	if got := jobEvents(t, ids["new"]); got != 0 {
		t.Errorf("events on new = %d, want 0", got)
	}
}

// TestTrackCoreConcurrent: two marks of the same job at once write one event; the other is
// a no-op.
func TestTrackCoreConcurrent(t *testing.T) {
	ctx := context.Background()
	s := trackStore(t)
	jobID := seedStatuses(t, s)["shortlisted"]
	var wg sync.WaitGroup
	results := make([]trackResult, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = trackJob(ctx, s, trackRequest{JobID: jobID, Action: "applied"})
		}()
	}
	wg.Wait()
	noops := 0
	for i := range 2 {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if results[i].NoOp {
			noops++
		}
	}
	if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM status_event WHERE job_id = %d AND status = 'applied'`, jobID)); got != 1 || noops != 1 {
		t.Errorf("applied events = %d, no-ops = %d; want 1 and 1", got, noops)
	}
}

// TestTrackCoreParseRule: parseTrackRule's refusals, in the CLI's words.
func TestTrackCoreParseRule(t *testing.T) {
	if f, p, err := parseTrackRule(""); f != "" || p != "" || err != nil {
		t.Errorf("empty: %q %q %v", f, p, err)
	}
	if f, p, err := parseTrackRule("company=^tailspin"); f != "company" || p != "^tailspin" || err != nil {
		t.Errorf("valid: %q %q %v", f, p, err)
	}
	for in, want := range map[string]string{
		"salary=x":        "--rule must be field=PATTERN with field one of company, title, description, location",
		"title":           "--rule must be field=PATTERN",
		"title=  ":        "--rule needs a pattern after title=",
		"title=(unclosed": "--rule pattern does not compile",
	} {
		_, _, err := parseTrackRule(in)
		if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}
