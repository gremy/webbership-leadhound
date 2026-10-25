//go:build scale

package leadstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadbrief"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The leads scale tests run the lead queries on the scale database with a year of lead
// finding and outreach seeded (leadstoretest.ScaleSeeded) as the app role, each against a
// time budget for a local M-series machine. They run with make test-scale.

// scaleLeads returns the app role's lead queries on the seeded scale database, and "now":
// ten in the morning of the seed's Day, never the wall clock.
func scaleLeads(t *testing.T) (leadstore.Queries, *store.Store, time.Time, *time.Location) {
	t.Helper()
	info := leadstoretest.ScaleSeeded(t)
	s, err := store.Open(context.Background(), storetest.ScaleDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping scale test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	return leadstore.NewQueries(s.Pool()), s, info.Day.Add(10 * time.Hour), info.Loc
}

// within runs fn, logs how long it took and fails when that is over budget.
func within(t *testing.T, name string, budget time.Duration, fn func() error) {
	t.Helper()
	start := time.Now()
	err := fn()
	took := time.Since(start)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	t.Logf("%s: %v (budget %v)", name, took.Round(time.Millisecond), budget)
	if took > budget {
		t.Errorf("%s took %v, over its budget of %v", name, took.Round(time.Millisecond), budget)
	}
}

// TestScaleLeadsFinding: the site finder's and the contact finder's selections.
func TestScaleLeadsFinding(t *testing.T) {
	q, _, now, loc := scaleLeads(t)
	ctx := context.Background()

	var due []leadstore.Site
	within(t, "DueSites(10000)", 2*time.Second, func() (err error) {
		due, err = q.DueSites(ctx, 10000, 30*24*time.Hour, now)
		return err
	})
	if len(due) != 10000 {
		t.Errorf("due sites = %d, want 10000", len(due))
	}

	y, m, d := now.In(loc).Date()
	yesterday := time.Date(y, m, d-1, 0, 0, 0, 0, loc)
	var sum leadstore.ProbeCounts
	within(t, "ProbeSummary(since yesterday)", time.Second, func() (err error) {
		sum, err = q.ProbeSummary(ctx, yesterday)
		return err
	})
	if sum.Total == 0 {
		t.Error("probe summary counted no probes since yesterday")
	}

	var probes []leadstore.ProbeForOrg
	within(t, "DrupalProbesWithoutOrg", 3*time.Second, func() (err error) {
		probes, err = q.DrupalProbesWithoutOrg(ctx)
		return err
	})
	if len(probes) == 0 {
		t.Error("no Drupal probe without an org")
	}

	var orgs []leadstore.Org
	within(t, "OrgsNeedingRoutes(300)", time.Second, func() (err error) {
		orgs, err = q.OrgsNeedingRoutes(ctx, 300, now)
		return err
	})
	if len(orgs) != 300 {
		t.Errorf("orgs needing routes = %d, want 300", len(orgs))
	}
}

// TestScaleLeadsQueue: the candidates a batch drafts from (no limit, as newBatch reads them)
// and the console's queue, under the seeded 30-rule profile.
func TestScaleLeadsQueue(t *testing.T) {
	q, _, now, loc := scaleLeads(t)
	ctx := context.Background()
	cfg, err := leadconfig.Load("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	// The seed lives on reserved names, so the scale test lets them through to measure the
	// query with its real rows; the reserved filter is the same per-row test either way.
	o := leadstore.CandidateOptions{KindsPriority: cfg.KindsPriority, Profile: cfg.PriorityProfile(), Today: now.In(loc), IncludeReserved: true}
	var cands []leadstore.Candidate
	within(t, "CandidateLeads(all)", 2*time.Second, func() (err error) {
		cands, err = q.CandidateLeads(ctx, o)
		return err
	})
	if len(cands) < 100 {
		t.Errorf("candidates = %d, want at least 100", len(cands))
	}
	var next []leadstore.Candidate
	within(t, "NextUp(50)", 2*time.Second, func() (err error) {
		next, err = q.NextUp(ctx, o, 50)
		return err
	})
	if len(next) != 50 {
		t.Errorf("next up = %d, want 50", len(next))
	}
}

// TestScaleLeadsSend: what one send run and the batch view read.
func TestScaleLeadsSend(t *testing.T) {
	q, s, now, loc := scaleLeads(t)
	ctx := context.Background()
	y, m, d := now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)

	var rows []leadstore.SendRow
	within(t, "SendRows", time.Second, func() (err error) {
		rows, err = q.SendRows(ctx, now, false)
		return err
	})
	if len(rows) == 0 {
		t.Error("no message due")
	}
	var sent int
	within(t, "SentSince(today)", time.Second, func() (err error) {
		sent, err = q.SentSince(ctx, today.AddDate(0, 0, -1))
		return err
	})
	if sent == 0 {
		t.Error("nothing sent since yesterday")
	}
	within(t, "SendingIDs", time.Second, func() error {
		ids, err := q.SendingIDs(ctx)
		if err == nil && len(ids) == 0 {
			t.Error("no message left sending")
		}
		return err
	})
	within(t, "SuppressionSet", time.Second, func() error {
		set, err := q.SuppressionSet(ctx)
		if err == nil && len(set.Addresses)+len(set.Domains) != 2000 {
			t.Errorf("suppressions = %d, want 2000", len(set.Addresses)+len(set.Domains))
		}
		return err
	})
	within(t, "SendPaused", time.Second, func() error {
		_, _, _, err := q.SendPaused(ctx)
		return err
	})
	within(t, "FirstLiveSend", time.Second, func() error {
		at, err := q.FirstLiveSend(ctx)
		if err == nil && at == nil {
			t.Error("no first live send")
		}
		return err
	})
	within(t, "WaitingEmails", time.Second, func() error {
		_, _, err := q.WaitingEmails(ctx)
		return err
	})
	within(t, "UnapprovedBatch", time.Second, func() error {
		_, _, err := q.UnapprovedBatch(ctx, false)
		return err
	})
	var last int64
	if err := s.Pool().QueryRow(ctx, `SELECT max(id) FROM outreach_batch`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	within(t, "BatchMessages", time.Second, func() error {
		ms, err := q.BatchMessages(ctx, last)
		if err == nil && len(ms) != 50 {
			t.Errorf("batch messages = %d, want 50", len(ms))
		}
		return err
	})
	within(t, "BatchContentSHA256", time.Second, func() error {
		_, err := q.BatchContentSHA256(ctx, last)
		return err
	})
	within(t, "leadbrief.Load", time.Second, func() error {
		_, err := leadbrief.Load(ctx, s.Pool(), now, loc)
		return err
	})
}
