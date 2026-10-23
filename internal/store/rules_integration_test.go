//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store"
)

func TestAddRuleValidates(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	bad := []struct{ name, kind, field, pattern, reason string }{
		{"invalid regex", "skip", "company", "acme(", "agency"},
		{"unknown kind", "drop", "company", "acme", "agency"},
		{"unknown field", "skip", "salary", "acme", "agency"},
		{"empty pattern", "skip", "company", " ", "agency"},
		{"no reason", "skip", "company", "acme", ""},
	}
	for _, tc := range bad {
		if _, err := q.AddRule(ctx, tc.kind, tc.field, tc.pattern, tc.reason, nil); !errors.Is(err, store.ErrInvalidRule) {
			t.Errorf("%s: AddRule = %v, want ErrInvalidRule", tc.name, err)
		}
	}
	rules, err := q.ListRules(ctx, true)
	if err != nil || len(rules) != 0 {
		t.Fatalf("after rejected adds: %d rules, %v; want none", len(rules), err)
	}

	jobID, _ := seedJob(t, q, "1000000001")
	id, err := q.AddRule(ctx, "skip", "company", `initech\b`, "staffing agency", &jobID)
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	rules, err = q.ActiveRules(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("ActiveRules = %d rules, %v; want 1", len(rules), err)
	}
	r := rules[0]
	if r.ID != id || r.Kind != "skip" || r.Field != "company" || r.Pattern != `initech\b` || r.Reason != "staffing agency" ||
		r.CreatedFromJobID == nil || *r.CreatedFromJobID != jobID || r.DisabledAt != nil || r.CreatedAt.IsZero() {
		t.Fatalf("rule = %+v", r)
	}
}

func TestDisableRule(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	keep, err := q.AddRule(ctx, "flag", "title", "contract", "contract roles", nil)
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	drop, err := q.AddRule(ctx, "skip", "company", "globex", "not this one", nil)
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	if err := q.DisableRule(ctx, drop, ""); !errors.Is(err, store.ErrInvalidRule) {
		t.Fatalf("DisableRule without a reason = %v, want ErrInvalidRule", err)
	}
	if err := q.DisableRule(ctx, drop, "changed my mind"); err != nil {
		t.Fatalf("DisableRule: %v", err)
	}
	if err := q.DisableRule(ctx, drop, "again"); !errors.Is(err, store.ErrRuleDisabled) {
		t.Fatalf("DisableRule twice = %v, want ErrRuleDisabled", err)
	}
	if err := q.DisableRule(ctx, 999, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DisableRule unknown = %v, want ErrNotFound", err)
	}

	active, err := q.ActiveRules(ctx)
	if err != nil || len(active) != 1 || active[0].ID != keep {
		t.Fatalf("ActiveRules = %+v, %v; want only rule %d", active, err, keep)
	}
	all, err := q.ListRules(ctx, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListRules(all) = %d, %v; want 2 (disabled kept, never deleted)", len(all), err)
	}
	d := all[1]
	if d.ID != drop || d.DisabledAt == nil || d.DisabledReason != "changed my mind" {
		t.Fatalf("disabled rule = %+v; the second disable must not overwrite the first", d)
	}
	if listed, _ := q.ListRules(ctx, false); len(listed) != 1 {
		t.Fatalf("ListRules(active) = %d, want 1", len(listed))
	}

	// Undo is adding it again: a new row, the old one stays disabled.
	again, err := q.AddRule(ctx, d.Kind, d.Field, d.Pattern, d.Reason, nil)
	if err != nil || again == drop {
		t.Fatalf("re-add = %d, %v; want a new id", again, err)
	}
}

func TestGateResults(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	jobID, fetchID := seedJob(t, q, "1000000001")
	if got, err := q.LatestGateResults(ctx, jobID); err != nil || len(got) != 0 {
		t.Fatalf("LatestGateResults with no observation = %v, %v; want none", got, err)
	}
	first, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test@1", Title: "A"})
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}
	if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: jobID, ObservationID: first, Gate: "language", Verdict: "pass", Reason: "posting is in English", DerivedBy: "gates@1"}); err != nil {
		t.Fatalf("InsertGateResult: %v", err)
	}
	second, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test@1", Title: "B"})
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}
	ruleID, err := q.AddRule(ctx, "skip", "company", "initech", "agency", nil)
	if err != nil {
		t.Fatalf("AddRule: %v", err)
	}
	for _, g := range []store.GateResultInsert{
		{JobID: jobID, ObservationID: second, Gate: "rule", Verdict: "fail", Reason: "agency (rule 1 on the company)", RuleID: &ruleID, DerivedBy: "gates@1"},
		{JobID: jobID, ObservationID: second, Gate: "language", Verdict: "pass", Reason: "posting is in English", DerivedBy: "gates@1"},
	} {
		if _, err := q.InsertGateResult(ctx, g); err != nil {
			t.Fatalf("InsertGateResult: %v", err)
		}
	}
	got, err := q.LatestGateResults(ctx, jobID)
	if err != nil {
		t.Fatalf("LatestGateResults: %v", err)
	}
	if len(got) != 2 || got[0].ObservationID != second || got[0].Gate != "rule" || got[0].RuleID == nil || *got[0].RuleID != ruleID ||
		got[1].Gate != "language" || got[1].RuleID != nil {
		t.Fatalf("LatestGateResults = %+v, want the second observation's two rows in order", got)
	}

	// The CHECKs hold: an unknown verdict or gate is refused.
	if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: jobID, ObservationID: second, Gate: "language", Verdict: "maybe", Reason: "x", DerivedBy: "gates@1"}); err == nil {
		t.Fatal("InsertGateResult with verdict maybe succeeded")
	}
	if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: jobID, ObservationID: second, Gate: "salary", Verdict: "pass", Reason: "x", DerivedBy: "gates@1"}); err == nil {
		t.Fatal("InsertGateResult with gate salary succeeded")
	}
}
