//go:build scale

package leadstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestScaleDataModelSeed: the v6 seed's findings, stars, facts, relevance, settings and calls are
// there in the shape the seed promises, and their reads keep within budget.
func TestScaleDataModelSeed(t *testing.T) {
	q, s, now, _ := scaleLeads(t)
	ctx := context.Background()
	one := func(sql string) int {
		t.Helper()
		var n int
		if err := s.Pool().QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	for _, c := range []struct {
		what string
		sql  string
		want int
	}{
		{"leads without their finding", `SELECT count(*) FROM lead l LEFT JOIN finding f ON f.id = l.finding_id
			WHERE f.id IS NULL OR f.org_id <> l.org_id OR f.source_id <> coalesce(l.probe_id, l.agency_finding_id)`, 0},
		{"findings", `SELECT count(*) FROM finding`, 5000},
		{"orgs with facts", `SELECT count(DISTINCT org_id) FROM org_fact`, 4500},
		{"latest relevance rows", `SELECT count(DISTINCT lead_id) FROM lead_relevance`, 4000},
		{"relevance rows", `SELECT count(*) FROM lead_relevance`, 5500},
		{"settings rows", `SELECT count(*) FROM lead_setting`, 3},
		{"label rows", `SELECT count(*) FROM lead_label`, 55},
	} {
		if got := one(c.sql); got != c.want {
			t.Errorf("%s = %d, want %d", c.what, got, c.want)
		}
	}

	var order []leadstore.StarredLead
	within(t, "StarOrder", 200*time.Millisecond, func() (err error) {
		order, err = q.StarOrder(ctx)
		return err
	})
	if len(order) != 200 {
		t.Errorf("starred leads = %d, want 200", len(order))
	}
	within(t, "MoveDown", 200*time.Millisecond, func() error {
		_, err := q.MoveDown(ctx, order[len(order)-1].LeadID, leadstore.ActorSystem) // the last: a no-op, no row
		return err
	})

	ids := make([]int64, 0, 5000)
	for id := int64(1); id <= 5000; id++ {
		ids = append(ids, id)
	}
	var rel map[int64]leadstore.Relevance
	within(t, "LatestRelevance(5000)", 500*time.Millisecond, func() (err error) {
		rel, err = q.LatestRelevance(ctx, ids)
		return err
	})
	if len(rel) != 4000 {
		t.Errorf("rated leads = %d, want 4000", len(rel))
	}
	within(t, "LatestRelevanceEval", 100*time.Millisecond, func() error {
		_, _, err := q.LatestRelevanceEval(ctx, "relevance@1")
		return err
	})
	var labelled []leadstore.LeadLabel
	within(t, "LabelledLeads", 100*time.Millisecond, func() (err error) {
		labelled, err = q.LabelledLeads(ctx)
		return err
	})
	if len(labelled) != 35 {
		t.Errorf("labelled leads = %d, want 35 (40 called, 5 cleared since)", len(labelled))
	}
	within(t, "LabelCount", 100*time.Millisecond, func() error {
		_, err := q.LabelCount(ctx)
		return err
	})

	orgIDs := make([]int64, 0, 100)
	for id := int64(2); id <= 200; id += 2 {
		orgIDs = append(orgIDs, id)
	}
	var facts []leadstore.Fact
	within(t, "OrgFacts(100 orgs)", 200*time.Millisecond, func() (err error) {
		facts, err = q.OrgFacts(ctx, orgIDs)
		return err
	})
	if len(facts) < 200 {
		t.Errorf("facts of 100 enriched orgs = %d, want at least two each", len(facts))
	}
	var orgs []leadstore.Org
	within(t, "OrgsNeedingEnrich(300)", time.Second, func() (err error) {
		orgs, err = q.OrgsNeedingEnrich(ctx, 300, now)
		return err
	})
	if len(orgs) == 0 {
		t.Error("no org needs enrichment, want the unenriched half")
	}
	within(t, "EnrichBodies", 200*time.Millisecond, func() error {
		_, err := q.EnrichBodies(ctx, orgs[0].ID)
		return err
	})
	within(t, "FindingsFor", 100*time.Millisecond, func() error {
		_, err := q.FindingsFor(ctx, orgs[0].ID)
		return err
	})
	within(t, "FindingFor", 100*time.Millisecond, func() error {
		_, _, err := q.FindingFor(ctx, 1)
		return err
	})
	within(t, "LatestSettings", 100*time.Millisecond, func() error {
		raw, _, _, fromDB, err := q.LatestSettings(ctx)
		if err == nil && (!fromDB || raw == nil) {
			t.Errorf("latest settings: fromDB %v, raw %q, want the third save", fromDB, raw)
		}
		return err
	})
}
