//go:build integration

package main

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// everyTableCount is the row count of every base table of the database, read as the owner.
func everyTableCount(t *testing.T) map[string]int {
	t.Helper()
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	rows, err := conn.Query(ctx, `SELECT quote_ident(table_schema) || '.' || quote_ident(table_name) FROM information_schema.tables
		WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if len(tables) < 20 {
		t.Fatalf("only %d tables found", len(tables))
	}
	out := map[string]int{}
	for _, tbl := range tables {
		var n int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+tbl).Scan(&n); err != nil { // tbl is quoted by the database
			t.Fatal(err)
		}
		out[tbl] = n
	}
	return out
}

// TestStepsNotSetUp: without leads set up the counts say so, and the bar answers 204.
func TestStepsNotSetUp(t *testing.T) {
	leadstoretest.Reset(t)
	b := testBackend(t)
	if _, err := b.MorningCounts(context.Background()); !errors.Is(err, console.ErrLeadsNotSetUp) {
		t.Errorf("MorningCounts: %v, want ErrLeadsNotSetUp", err)
	}
	if w := newConsoleClient(t, b, nil).get("/leads/steps?at=/leads/today", true); w.Code != http.StatusNoContent {
		t.Errorf("/leads/steps: %d, want 204", w.Code)
	}
}

// TestStepsColdStart: a brand-new database with leads set up counts nothing at all.
func TestStepsColdStart(t *testing.T) {
	dir := leadsDB(t)
	b := testBackend(t)
	b.cfg.DataDir = dir
	c, err := b.MorningCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c != (console.MorningCounts{}) {
		t.Errorf("cold start: %+v, want all zeros", c)
	}
}

// TestStepsMatchToday: on one world with a call due now, a call due by the clock that waits on
// an earlier step, a reply and new leads, the bar's By hand, Calls and Replies are Today's own
// lists, Review is Total - Reviewed of CampaignProgress summed over the campaigns, and the
// step fragment writes nothing.
func TestStepsMatchToday(t *testing.T) {
	w := newCP5World(t)
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	// The IE call is due by the clock, but its earlier steps are unsent: Today keeps it waiting.
	if _, err := owner.Exec(ctx, `UPDATE outreach_message SET due_at = $2 WHERE id = $1`, w.ieCall, w.now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx,
		`UPDATE lead SET state = 'replied', state_at = now() WHERE id = (SELECT min(id) FROM lead WHERE state = 'new' AND kind <> 'selftest')`); err != nil {
		t.Fatal(err)
	}

	today, err := w.b.LeadsToday(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var waits bool
	for _, it := range today.CallsWaiting {
		waits = waits || it.MessageID == w.ieCall
	}
	if due := len(today.CallsNow) + len(today.CallsDue); !waits || due == 0 || len(today.Replies) == 0 {
		t.Fatalf("the world lacks a case: IE call waiting %v, calls due %d, replies %d", waits, due, len(today.Replies))
	}
	c, err := w.b.MorningCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(today.CallsNow) + len(today.CallsDue) + len(today.CallsOverdue); c.Calls != want {
		t.Errorf("Calls %d, Today %d", c.Calls, want)
	}
	if c.Replies != len(today.Replies) || c.ByHand != len(today.Manual) {
		t.Errorf("Replies %d By hand %d, Today %d and %d", c.Replies, c.ByHand, len(today.Replies), len(today.Manual))
	}

	lq := leadstore.NewQueries(w.b.s.Pool())
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		t.Fatal(err)
	}
	review := 0
	for _, cp := range cs {
		if cp.RetiredAt != nil {
			continue
		}
		reviewed, total, err := lq.CampaignProgress(ctx, cp.ID)
		if err != nil {
			t.Fatal(err)
		}
		review += total - reviewed
	}
	if c.ToReview != review || review == 0 {
		t.Errorf("Review %d, CampaignProgress %d", c.ToReview, review)
	}
	if c.BatchID != 0 || c.BatchItems != 0 {
		t.Errorf("the only batch is approved, yet batch %d with %d leads", c.BatchID, c.BatchItems)
	}

	before := everyTableCount(t)
	cl := newConsoleClient(t, w.b, nil)
	if r := cl.get("/leads/steps?at=/leads/today", true); r.Code != http.StatusOK {
		t.Errorf("/leads/steps: %d", r.Code)
	}
	if after := everyTableCount(t); !maps.Equal(before, after) {
		for tbl, n := range after {
			if before[tbl] != n {
				t.Errorf("/leads/steps wrote %s: %d rows, then %d", tbl, before[tbl], n)
			}
		}
	}
}

// TestStepsCallOnlyBatch: a waiting batch that holds calls alone still has work to approve,
// so its id is the bar's batch and its leads are counted.
func TestStepsCallOnlyBatch(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	if err := os.CopyFS(filepath.Join(dir, campaign.Dir), os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}
	batchFixture(t)
	orgPhone(t, "tailspin.example", cp5GBNumber)
	mustRun(t, "leads", "campaigns", "register")
	mustRun(t, "leads", "build")
	mustRun(t, "leads", "batch", "new")
	owner := storetest.OwnerConn(t)
	if _, err := owner.Exec(ctx, `DELETE FROM outreach_message WHERE channel <> 'phone'`); err != nil {
		t.Fatal(err)
	}
	var batch int64
	var leads int
	if err := owner.QueryRow(ctx,
		`SELECT max(batch_id), count(DISTINCT lead_id)::int FROM outreach_message WHERE channel = 'phone'`).Scan(&batch, &leads); err != nil || leads == 0 {
		t.Fatalf("no call-only batch: %v", err)
	}
	b := testBackend(t)
	b.cfg.DataDir = dir
	c, err := b.MorningCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.BatchID != batch || c.BatchItems != leads {
		t.Errorf("batch %d with %d leads, want batch %d with %d", c.BatchID, c.BatchItems, batch, leads)
	}
}
