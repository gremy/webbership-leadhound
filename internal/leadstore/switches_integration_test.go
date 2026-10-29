//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestJobSwitch: cold start every job on with a zero time, in Jobs order; off and on again
// are rows; a write that would not change the value is skipped; an unknown job or actor is
// refused before any write.
func TestJobSwitch(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	rows := func() int {
		t.Helper()
		var n int
		if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT count(*) FROM job_switch`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	all, err := q.JobSwitches(ctx, leadstore.DefaultAccountID)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for _, s := range all {
		jobs = append(jobs, s.Job)
		if !s.On || !s.At.IsZero() || s.Reason != "" || s.Actor != "" {
			t.Errorf("cold start %s: %+v, want on and never set", s.Job, s)
		}
	}
	if !slices.Equal(jobs, leadstore.Jobs) || len(jobs) != 6 {
		t.Errorf("jobs %v, want %v", jobs, leadstore.Jobs)
	}

	changed, err := q.SetJobSwitch(ctx, 0, leadstore.JobResearch, false, "credits low", leadstore.ActorGremy)
	if err != nil || !changed {
		t.Fatalf("off: changed %v, %v", changed, err)
	}
	s, err := q.JobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobResearch)
	if err != nil || s.On || s.Reason != "credits low" || s.Actor != leadstore.ActorGremy || s.At.IsZero() {
		t.Fatalf("after off: %+v %v", s, err)
	}
	if other, _ := q.JobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobBatch); !other.On {
		t.Error("switching research off switched batch off")
	}

	// Unchanged: off again (even with another reason) and on when already on write nothing.
	before := rows()
	if changed, err = q.SetJobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobResearch, false, "other", leadstore.ActorGremy); err != nil || changed {
		t.Errorf("off again: changed %v, %v", changed, err)
	}
	if changed, err = q.SetJobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobSync, true, "", leadstore.ActorGremy); err != nil || changed {
		t.Errorf("on when never set: changed %v, %v", changed, err)
	}
	if n := rows(); n != before {
		t.Errorf("unchanged writes added rows: %d → %d", before, n)
	}

	if changed, err = q.SetJobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobResearch, true, "", leadstore.ActorGremy); err != nil || !changed {
		t.Fatalf("on: changed %v, %v", changed, err)
	}
	if s, _ = q.JobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobResearch); !s.On || s.Reason != "" {
		t.Errorf("after on: %+v", s)
	}
	if n := rows(); n != 2 {
		t.Errorf("rows %d, want 2 (off, on)", n)
	}

	if _, err = q.SetJobSwitch(ctx, leadstore.DefaultAccountID, "send", false, "", leadstore.ActorGremy); !errors.Is(err, leadstore.ErrUnknownJob) {
		t.Errorf("unknown job: %v", err)
	}
	if _, err = q.JobSwitch(ctx, leadstore.DefaultAccountID, "nope"); !errors.Is(err, leadstore.ErrUnknownJob) {
		t.Errorf("unknown job read: %v", err)
	}
	if _, err = q.SetJobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobBatch, false, "", "someone"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
	if n := rows(); n != 2 {
		t.Errorf("refusals wrote rows: %d", n)
	}
}

// TestJobSwitchAppendOnly: the app role never rewrites or deletes a switch row.
func TestJobSwitchAppendOnly(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	if _, err := leadstoretest.Q(t).SetJobSwitch(ctx, 0, leadstore.JobBatch, false, "x", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	for _, q := range []string{"UPDATE job_switch SET is_on = true", "DELETE FROM job_switch", "TRUNCATE job_switch"} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "42501" {
			t.Errorf("%s as app: err = %v, want 42501", q, err)
		}
	}
}

// TestSendPauseActor: the send pause records its actor; an unknown actor is refused.
func TestSendPauseActor(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	if err := q.SetSendPause(ctx, true, "converted", leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	var actor string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT actor FROM send_pause ORDER BY id DESC LIMIT 1`).Scan(&actor); err != nil || actor != leadstore.ActorSystem {
		t.Errorf("actor %q (%v), want system", actor, err)
	}
	if err := q.SetSendPause(ctx, false, "", "someone"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
}
