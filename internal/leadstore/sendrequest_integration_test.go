//go:build integration

package leadstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00027: outreach_batch.manual, send_request and send_override.

const sendOverrideVersion = 27 // 00027_send_override.sql

// sendNow is the fixed "now" of these tests.
var sendNow = time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)

// sendNowBatch creates a live batch for leads and marks it manual.
func (f outreachFixture) sendNowBatch(t *testing.T, leadIDs ...int64) (int64, string) {
	t.Helper()
	id, hash := f.batch(t, false, leadIDs...)
	if err := f.q.MarkBatchManual(context.Background(), id); err != nil {
		t.Fatalf("mark batch %d manual: %v", id, err)
	}
	return id, hash
}

// TestMigrationSendOverrideDownUp: down drops both tables and the column, up puts them back;
// the app role may insert both, UPDATE only send_request.used_at and outreach_batch.manual, and
// never UPDATE send_override.
func TestMigrationSendOverrideDownUp(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := f.owner

	for _, c := range []string{"send_request_codes_check", "send_override_codes_check"} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, c); n != 1 {
			t.Errorf("constraint %s: %d, want 1", c, n)
		}
	}
	if appHas(t, owner, "send_override", "UPDATE") || !appHas(t, owner, "send_override", "INSERT") {
		t.Error("send_override: want INSERT and no UPDATE for the app")
	}
	if appHas(t, owner, "send_request", "UPDATE") || !appHas(t, owner, "send_request", "INSERT") {
		t.Error("send_request: want INSERT and no table-wide UPDATE for the app")
	}
	if !appHasColumn(t, owner, "send_request", "used_at", "UPDATE") {
		t.Error("send_request: the app lacks UPDATE on used_at")
	}
	for _, col := range []string{"batch_id", "codes", "hash_prefix", "operator", "created_at"} {
		if appHasColumn(t, owner, "send_request", col, "UPDATE") {
			t.Errorf("send_request: the app has UPDATE on %s, want none", col)
		}
	}
	if !appHasColumn(t, owner, "outreach_batch", "manual", "UPDATE") {
		t.Error("outreach_batch: the app lacks UPDATE on manual")
	}
	for _, table := range []string{"send_request", "send_override"} {
		if appHas(t, owner, table, "DELETE") || appHas(t, owner, table, "TRUNCATE") {
			t.Errorf("%s: the app may DELETE or TRUNCATE", table)
		}
	}
	lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	batch, _ := f.batch(t, false, lead)
	if _, err := f.q.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: batch, HashPrefix: "abc", Operator: "op"}); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, `UPDATE send_request SET codes = '{ramp}'`); sqlState(err) != "42501" {
		t.Errorf("app rewrites a request's codes: %v, want 42501", err)
	}

	p := leadsProvider(t)
	t.Cleanup(func() {
		migrateAll(t)
	})
	if _, err := p.DownTo(ctx, sendOverrideVersion-1); err != nil {
		t.Fatalf("down to %d: %v", sendOverrideVersion-1, err)
	}
	for _, table := range []string{"send_request", "send_override"} {
		var reg *string
		if err := owner.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, table).Scan(&reg); err != nil || reg != nil {
			t.Errorf("%s after down: %v %v, want gone", table, reg, err)
		}
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
		AND table_name = 'outreach_batch' AND column_name = 'manual'`); n != 0 {
		t.Error("outreach_batch.manual left after down")
	}
	if _, err := p.UpTo(ctx, sendOverrideVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if b, err := f.q.BatchByID(ctx, batch); err != nil || b.Manual {
		t.Errorf("batch after down and up: %+v %v, want not manual", b, err)
	}
}

// TestSendCodesClosedSet: a request or override naming a code outside the closed set, or a
// hard stop, is refused by the table; every code of the set is taken.
func TestSendCodesClosedSet(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	batch, _ := f.batch(t, false, lead)
	for _, codes := range [][]string{{"hard_ceiling"}, {leadstore.OverrideRamp, "suppressed"}, {"domain_mailbox"}, {"spacing"}} {
		if _, err := f.q.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: batch, Codes: codes, HashPrefix: "abc", Operator: "op"}); sqlState(err) != "23514" {
			t.Errorf("request codes %v: %v, want a check violation", codes, err)
		}
		if err := f.q.InsertSendOverride(ctx, leadstore.SendOverride{BatchID: batch, LeadID: lead, Codes: codes, Operator: "op"}); sqlState(err) != "23514" {
			t.Errorf("override codes %v: %v, want a check violation", codes, err)
		}
	}
	if _, err := f.q.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: batch, Codes: leadstore.OverrideCodes, HashPrefix: "abc", Operator: "op"}); err != nil {
		t.Errorf("request with every code: %v", err)
	}
	if err := f.q.InsertSendOverride(ctx, leadstore.SendOverride{BatchID: batch, LeadID: lead, Codes: leadstore.OverrideCodes, Operator: "op"}); err != nil {
		t.Errorf("override with every code: %v", err)
	}
}

// TestManualBatchNeverBlocks: a manual batch is not the unapproved batch a new one waits for;
// a normal one still is. Marking is refused on an approved or discarded batch and is a no-op
// the second time.
func TestManualBatchNeverBlocks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	b := f.siteLead(t, "southwind.localhost", intp(2), 7, "info@southwind.localhost")

	if _, ok, err := f.q.UnapprovedBatch(ctx, 0, false); err != nil || ok {
		t.Fatalf("cold start: %v %v, want none", ok, err)
	}
	manual, _ := f.sendNowBatch(t, a)
	if _, ok, err := f.q.UnapprovedBatch(ctx, 0, false); err != nil || ok {
		t.Errorf("with only a manual batch waiting: %v %v, want none", ok, err)
	}
	if got, err := f.q.BatchByID(ctx, manual); err != nil || !got.Manual {
		t.Errorf("manual batch read back: %+v %v, want Manual", got, err)
	}
	if err := f.q.MarkBatchManual(ctx, manual); err != nil {
		t.Errorf("mark again: %v, want a no-op", err)
	}
	normal, hash := f.batch(t, false, b)
	if id, ok, err := f.q.UnapprovedBatch(ctx, 0, false); err != nil || !ok || id != normal {
		t.Errorf("with a normal batch waiting: %d %v %v, want %d", id, ok, err, normal)
	}
	if got, _ := f.q.BatchByID(ctx, normal); got.Manual {
		t.Error("a normal batch reads as manual")
	}
	f.approve(t, normal, hash)
	if err := f.q.MarkBatchManual(ctx, normal); !errors.Is(err, leadstore.ErrBatchApproved) {
		t.Errorf("mark an approved batch: %v, want ErrBatchApproved", err)
	}
	if _, err := f.q.DiscardBatch(ctx, manual); err != nil {
		t.Fatal(err)
	}
	if err := f.q.MarkBatchManual(ctx, manual); !errors.Is(err, leadstore.ErrBatchDiscarded) {
		t.Errorf("mark a discarded batch: %v, want ErrBatchDiscarded", err)
	}
	if err := f.q.MarkBatchManual(ctx, 999999); !errors.Is(err, leadstore.ErrBatchNotFound) {
		t.Errorf("mark a missing batch: %v, want ErrBatchNotFound", err)
	}
}

// TestDiscardStaleManualBatches discards only manual batches, unapproved, created before the
// cut: an approved manual one, a fresh manual one and a stale normal one stay.
func TestDiscardStaleManualBatches(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	cut := sendNow.Add(-30 * time.Minute)
	old := sendNow.Add(-time.Hour)

	if n, err := f.q.DiscardStaleManualBatches(ctx, cut); err != nil || n != 0 {
		t.Fatalf("cold start: %d %v, want 0", n, err)
	}
	lead := func(domain string) int64 {
		return f.siteLead(t, domain, intp(1), 7, "info@"+domain)
	}
	staleLead := lead("stale.localhost")
	stale, _ := f.sendNowBatch(t, staleLead)
	approved, approvedHash := f.sendNowBatch(t, lead("approved.localhost"))
	f.approve(t, approved, approvedHash)
	fresh, _ := f.sendNowBatch(t, lead("fresh.localhost"))
	normal, _ := f.batch(t, false, lead("normal.localhost"))
	for _, id := range []int64{stale, approved, normal} {
		f.ownerRun(t, `UPDATE outreach_batch SET created_at = $2 WHERE id = $1`, id, old)
	}
	f.ownerRun(t, `UPDATE outreach_batch SET created_at = $2 WHERE id = $1`, fresh, sendNow)

	n, err := f.q.DiscardStaleManualBatches(ctx, cut)
	if err != nil || n != 1 {
		t.Fatalf("discard stale: %d %v, want 1", n, err)
	}
	for id, want := range map[int64]bool{stale: true, approved: false, fresh: false, normal: false} {
		b, err := f.q.BatchByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.DiscardedAt != nil; got != want {
			t.Errorf("batch %d discarded = %v, want %v", id, got, want)
		}
	}
	// Discarded as an operator discard is: the messages cancelled, the lead back to new.
	eqStates(t, "stale batch messages", f.msgStates(t, stale, staleLead), "cancelled", "cancelled")
	if got := f.leadState(t, staleLead); got != leadstore.LeadNew {
		t.Errorf("stale batch lead = %s, want new", got)
	}
	var actor string
	if err := f.owner.QueryRow(ctx, `SELECT actor FROM lead_event WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, staleLead).Scan(&actor); err != nil || actor != leadstore.ActorSystem {
		t.Errorf("stale discard lead event actor = %q (%v), want system", actor, err)
	}
	if n, err := f.q.DiscardStaleManualBatches(ctx, cut); err != nil || n != 0 {
		t.Errorf("second pass: %d %v, want 0", n, err)
	}
}

// TestClaimSendRequest: the newest unused request at or after notBefore is claimed, once; a
// stale one and another batch's are never taken.
func TestClaimSendRequest(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	batch, _ := f.sendNowBatch(t, f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost"))
	other, _ := f.sendNowBatch(t, f.siteLead(t, "southwind.localhost", intp(2), 7, "info@southwind.localhost"))
	notBefore := sendNow.Add(-15 * time.Minute)

	if _, ok, err := f.q.ClaimSendRequest(ctx, batch, notBefore, sendNow); err != nil || ok {
		t.Fatalf("cold start: %v %v, want none", ok, err)
	}
	insert := func(b int64, codes []string, at time.Time) int64 {
		id, err := f.q.InsertSendRequest(ctx, leadstore.SendRequest{BatchID: b, Codes: codes, HashPrefix: "0123456789ab", Operator: "op"})
		if err != nil {
			t.Fatal(err)
		}
		f.ownerRun(t, `UPDATE send_request SET created_at = $2 WHERE id = $1`, id, at)
		return id
	}
	insert(batch, []string{leadstore.OverrideRamp}, sendNow.Add(-time.Hour)) // stale
	insert(other, nil, sendNow.Add(-time.Minute))                            // another batch
	if _, ok, err := f.q.ClaimSendRequest(ctx, batch, notBefore, sendNow); err != nil || ok {
		t.Fatalf("stale and other-batch only: %v %v, want none", ok, err)
	}
	older := insert(batch, []string{leadstore.OverrideWindow}, sendNow.Add(-10*time.Minute))
	newest := insert(batch, []string{leadstore.OverrideWindow, leadstore.OverrideSchedule}, sendNow.Add(-2*time.Minute))

	got, ok, err := f.q.ClaimSendRequest(ctx, batch, notBefore, sendNow)
	if err != nil || !ok || got.ID != newest {
		t.Fatalf("claim: %+v %v %v, want request %d", got, ok, err, newest)
	}
	if got.BatchID != batch || got.HashPrefix != "0123456789ab" || got.Operator != "op" ||
		!reflect.DeepEqual(got.Codes, []string{leadstore.OverrideWindow, leadstore.OverrideSchedule}) ||
		got.UsedAt == nil || !got.UsedAt.Equal(sendNow) {
		t.Errorf("claimed row: %+v", got)
	}
	// The newest is used; the next claim takes the older one, then none is left.
	if got, ok, err := f.q.ClaimSendRequest(ctx, batch, notBefore, sendNow); err != nil || !ok || got.ID != older {
		t.Errorf("second claim: %+v %v %v, want request %d", got, ok, err, older)
	}
	if _, ok, err := f.q.ClaimSendRequest(ctx, batch, notBefore, sendNow); err != nil || ok {
		t.Errorf("third claim: %v %v, want none", ok, err)
	}
	// A request is claimed once even when a used one's row is looked at again.
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM send_request WHERE batch_id = $1 AND used_at IS NULL`, batch); n != 1 {
		t.Errorf("unused requests left on the batch: %d, want 1 (the stale one)", n)
	}
}

// TestSendOverrideRoundTrip: an override reads back as written, in order, per batch; a nil
// detail stores {} and a zero time stores now.
func TestSendOverrideRoundTrip(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "northwind.localhost", intp(1), 7, "info@northwind.localhost")
	batch, _ := f.sendNowBatch(t, lead)
	other, _ := f.sendNowBatch(t, f.siteLead(t, "southwind.localhost", intp(2), 7, "info@southwind.localhost"))

	if got, err := f.q.SendOverrides(ctx, batch); err != nil || len(got) != 0 {
		t.Fatalf("cold start: %+v %v", got, err)
	}
	msg := f.msgID(t, batch, lead, 0)
	run := f.runID
	detail := json.RawMessage(`{"ramp": {"used": 12, "allowed": 10}}`)
	first := leadstore.SendOverride{RunID: &run, BatchID: batch, LeadID: lead, MessageID: &msg,
		Codes: []string{leadstore.OverrideRamp}, Detail: detail, Operator: "op", At: sendNow}
	if err := f.q.InsertSendOverride(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := leadstore.SendOverride{BatchID: batch, LeadID: lead, Codes: []string{leadstore.OverrideWindow}, Operator: "op"}
	if err := f.q.InsertSendOverride(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := f.q.InsertSendOverride(ctx, leadstore.SendOverride{BatchID: other, LeadID: lead, Codes: []string{}, Operator: "op", At: sendNow}); err != nil {
		t.Fatal(err)
	}
	got, err := f.q.SendOverrides(ctx, batch)
	if err != nil || len(got) != 2 {
		t.Fatalf("overrides: %+v %v, want 2", got, err)
	}
	g := got[0]
	if g.RunID == nil || *g.RunID != run || g.MessageID == nil || *g.MessageID != msg || g.BatchID != batch ||
		g.LeadID != lead || !reflect.DeepEqual(g.Codes, first.Codes) || g.Operator != "op" || !g.At.Equal(sendNow) {
		t.Errorf("first override: %+v", g)
	}
	var gotDetail, wantDetail any
	_ = json.Unmarshal(g.Detail, &gotDetail)
	_ = json.Unmarshal(detail, &wantDetail)
	if !reflect.DeepEqual(gotDetail, wantDetail) {
		t.Errorf("detail = %s, want %s", g.Detail, detail)
	}
	if s := got[1]; s.RunID != nil || s.MessageID != nil || string(s.Detail) != `{}` || s.At.IsZero() || g.ID >= s.ID {
		t.Errorf("second override: %+v", s)
	}
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, `UPDATE send_override SET codes = '{}'`); sqlState(err) != "42501" {
		t.Errorf("app rewrites an override: %v, want 42501", err)
	}
}
