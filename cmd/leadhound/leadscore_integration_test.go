//go:build integration

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// leadsTuesday is the clock the core tests run at: a Tuesday morning in London and
// Bucharest alike.
var leadsTuesday = time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)

// leadsScene is a fresh leads database with the batch fixture built into leads, shadow mode,
// batches drawn from every lead (drawFromAll: its tests are about drafting, not about the
// operator's order) and the clock at leadsTuesday. It returns the data directory.
func leadsScene(t *testing.T) string {
	t.Helper()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	t.Setenv("LEADHOUND_TZ", "Europe/Bucharest")
	saved := leadsNow
	leadsNow = func() time.Time { return leadsTuesday }
	t.Cleanup(func() { leadsNow = saved })
	batchFixture(t)
	mustRun(t, "leads", "build")
	return dir
}

// goLive switches the commands to live outreach and lets the fixture's reserved names into
// a live batch (only tests do: leadsMail.allowReserved).
func goLive(t *testing.T) {
	t.Helper()
	t.Setenv("LEADHOUND_OUTREACH", "live")
	saved := leadsMail
	leadsMail.allowReserved = true
	newMailboxFakes().install(&leadsMail)
	t.Cleanup(func() { leadsMail = saved })
}

// setCaps rewrites leads.yaml's ramp start and steps.
func setCaps(t *testing.T, dir, startDate, ramp string) {
	t.Helper()
	path := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	out := regexp.MustCompile(`(?m)^  start_date: .*$`).ReplaceAll(raw, []byte("  start_date: "+startDate))
	out = regexp.MustCompile(`(?m)^  ramp: .*$`).ReplaceAll(out, []byte("  ramp: "+ramp))
	if string(out) == string(raw) {
		t.Fatal("leads.yaml caps not rewritten")
	}
	if err := os.WriteFile(path, out, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

// drawFromAll sets leads.yaml's batches_from to starred_then_rest, so batch new drafts
// leads nobody starred: for tests about drafting, not about the operator's order (D6).
func drawFromAll(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	out := regexp.MustCompile(`(?m)^batches_from: \S+`).ReplaceAll(raw, []byte("batches_from: "+leadconfig.BatchesStarredThenRest))
	if string(out) == string(raw) {
		t.Fatal("leads.yaml batches_from not rewritten")
	}
	if err := os.WriteFile(path, out, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

// batchHash is the approve hash of batch id.
func batchHash(t *testing.T, id int64) string {
	t.Helper()
	var sum []byte
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT content_sha256 FROM outreach_batch WHERE id = $1`, id).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum)[:leadstore.MinHashPrefix]
}

// coreEnv is what a core needs, read as the commands read it.
type coreEnv struct {
	s     *store.Store
	setup leadsSetup
	loc   *time.Location
}

func openCoreEnv(t *testing.T) coreEnv {
	t.Helper()
	cfg := mustConfig(t)
	setup, err := leadsEnv(cfg, leadsForOutreach)
	if err != nil {
		t.Fatal(err)
	}
	s, loc, err := openStoreAndZone(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return coreEnv{s: s, setup: setup, loc: loc}
}

func wantRefusal(t *testing.T, what string, err error, kind RefusalKind) {
	t.Helper()
	if r := asRefusal(err); r == nil || r.Kind != kind {
		t.Fatalf("%s: err = %v, want refusal %d", what, err, kind)
	}
}

// TestLeadsBatchGuard: batch new makes no batch while one of its kind waits for approval,
// live and shadow alike; an approved or discarded batch no longer blocks.
func TestLeadsBatchGuard(t *testing.T) {
	dir := leadsScene(t)

	// Shadow: cold start makes one, a second waits on it.
	wantLines(t, "first", mustRun(t, "leads", "batch", "new"), "Batch 1 (shadow): ")
	out := mustRun(t, "leads", "batch", "new")
	if out != "Batch 1 still waits for approval; no new batch.\n" {
		t.Errorf("second shadow batch new:\n%s", out)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 1 {
		t.Errorf("%d batches, want 1", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM run WHERE command = 'leads-batch-new' AND note = 'batch 1 still waits for approval'`); n != 1 {
		t.Errorf("the refused run is not noted: %d", n)
	}
	mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))
	wantLines(t, "after approval", mustRun(t, "leads", "batch", "new"), "Batch 2 (shadow): ")
	wantLines(t, "discard", mustRun(t, "leads", "batch", "discard", "2"), "Batch 2 (shadow) discarded: ", "0 leads back to new")
	wantLines(t, "after discard", mustRun(t, "leads", "batch", "new"), "Batch 3 (shadow): ")

	// Live: a waiting shadow batch never blocks a live one, nor the other way round.
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	wantLines(t, "live", mustRun(t, "leads", "batch", "new"), "Batch 4 (live): ")
	if out := mustRun(t, "leads", "batch", "new"); out != "Batch 4 still waits for approval; no new batch.\n" {
		t.Errorf("second live batch new:\n%s", out)
	}
	t.Setenv("LEADHOUND_OUTREACH", "")
	if out := mustRun(t, "leads", "batch", "new"); out != "Batch 3 still waits for approval; no new batch.\n" {
		t.Errorf("shadow batch new with both waiting:\n%s", out)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 4 {
		t.Errorf("%d batches, want 4", n)
	}
}

// TestLeadsBatchSizing: a live batch drafts no more leads than the ramp can send over the
// next three business days once the approved emails have gone; --size can only lower it.
func TestLeadsBatchSizing(t *testing.T) {
	dir := leadsScene(t)
	goLive(t)
	setCaps(t, dir, "2026-10-27", "[{from_day: 0, per_day: 4}]")

	// Room for 12 emails, 3 steps a lead: 4 leads; --size 2 asks for fewer.
	out := mustRun(t, "leads", "batch", "new", "--size", "2")
	wantLines(t, "size 2", out, "Batch 1 (live): ",
		"Sizing: 2 leads (the most asked for): room for 12 emails over the next 3 days, and each lead has 3 steps.")
	mustRun(t, "leads", "batch", "discard", "1")
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "ramp 4", out, "Batch 2 (live): ", "Sizing: 4 leads: room for 12 emails over the next 3 days, and each lead has 3 steps.")
	if n := batchCount(t, `SELECT count(DISTINCT lead_id) FROM outreach_message WHERE batch_id = 2`); n != 4 {
		t.Errorf("batch 2 drafts %d leads, want 4", n)
	}

	// Approved with exactly three email leads kept: 9 emails wait, room for 3, so 1 lead.
	mustRun(t, "leads", "batch", "approve", "2", "--hash", batchHash(t, 2), "--skip", skipToEmailLeads(t, 2, 3))
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE batch_id = 2 AND state = 'approved' AND channel = 'email'`); n != 9 {
		t.Fatalf("%d approved emails, want 9", n)
	}
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "9 waiting", out, "Batch 3 (live): ",
		"Sizing: 1 lead: room for 3 emails over the next 3 days (12 by the warm-up ramp, 9 approved and waiting), and each lead has 3 steps.")
	mustRun(t, "leads", "batch", "discard", "3")

	// A smaller ramp: the approved mail fills it, so no batch.
	setCaps(t, dir, "2026-10-27", "[{from_day: 0, per_day: 3}]")
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "no room", out, "Enough approved mail waits for the next days; no new batch.",
		"Sizing: 0 leads: room for 0 emails over the next 3 days (9 by the warm-up ramp, 9 approved and waiting)")
	// Before the ramp starts there is no room at all, and it says so.
	setCaps(t, dir, "2026-12-01", "[{from_day: 0, per_day: 3}]")
	wantLines(t, "ramp not started", mustRun(t, "leads", "batch", "new"), "Enough approved mail waits for the next days; no new batch.")
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 3 {
		t.Errorf("%d batches, want 3", n)
	}

	// The console never drafts a shadow batch: refused before anything is read or written.
	t.Setenv("LEADHOUND_OUTREACH", "")
	ce := openCoreEnv(t)
	_, err := makeBatch(context.Background(), ce.s, newBatchSetup(ce.setup, ce.loc), leadsNow(), NewBatchRequest{LiveOnly: true})
	wantRefusal(t, "shadow from the console", err, RefuseNotLive)
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 3 {
		t.Errorf("a refused shadow batch wrote one: %d batches", n)
	}
}

// skipToEmailLeads is the --skip list that leaves exactly keep email leads of batch id
// approved: every manual (contact-form) item, then email items after the first keep. Chosen
// by what each item is, not its position, so the priority order of the leads never changes
// what the caller proves.
func skipToEmailLeads(t *testing.T, id int64, keep int) string {
	t.Helper()
	v, err := batchView(context.Background(), leadstoretest.Q(t), id)
	if err != nil {
		t.Fatal(err)
	}
	var skip []string
	kept := 0
	for _, it := range v.Items {
		if it.Lead.Channel == leadstore.ChannelEmail && kept < keep {
			kept++
			continue
		}
		skip = append(skip, fmt.Sprint(it.N))
	}
	if kept != keep || len(skip) == 0 {
		t.Fatalf("batch %d has %d email leads and %d other items; want %d kept and at least one skipped", id, kept, len(skip), keep)
	}
	return strings.Join(skip, ",")
}

// liveMaybeSent makes a live batch of the fixture's leads, approves it and leaves the first
// lead's first email maybe_sent (or sending) at its attempt time. It returns that message.
func liveMaybeSent(t *testing.T, dir string, leaveSending bool) (msgID int64, at time.Time) {
	t.Helper()
	ctx := context.Background()
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	mustRun(t, "leads", "batch", "new", "--size", "2")
	mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))
	lq := leadstoretest.Q(t)
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT min(id) FROM outreach_message WHERE batch_id = 1 AND channel = 'email' AND step = 0`).Scan(&msgID); err != nil {
		t.Fatal(err)
	}
	at = leadsTuesday.Add(90 * time.Minute)
	if ok, err := lq.MarkSending(ctx, msgID); err != nil || !ok {
		t.Fatalf("mark sending: %v %v", ok, err)
	}
	if err := lq.StampAttempt(ctx, msgID, at); err != nil {
		t.Fatal(err)
	}
	if !leaveSending {
		if err := lq.MarkMaybeSent(ctx, msgID, "connection lost after DATA"); err != nil {
			t.Fatal(err)
		}
	}
	return msgID, at
}

// TestLeadsResolve: a maybe-sent email checked by hand is resolved only under the send lock;
// sent schedules its follow-up once, not sent clears its attempt time.
func TestLeadsResolve(t *testing.T) {
	dir := leadsScene(t)
	ctx := context.Background()
	msg, at := liveMaybeSent(t, dir, false)
	id := fmt.Sprint(msg)

	// A send run holds the lock: refused, nothing changes.
	other, err := store.Open(ctx, storetest.AppDSN(), appName())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lock, err := leadstore.TryLock(ctx, other.Pool(), leadstore.SendLockKey)
	if err != nil {
		t.Fatal(err)
	}
	code, _, errOut := leadsRun(t, "leads", "resolve", id, "sent")
	if code != 2 || !strings.Contains(errOut, "A send run is in progress; try again in a few minutes.") {
		t.Errorf("resolve under a send run exited %d: %s", code, errOut)
	}
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*) FROM outreach_message WHERE id = %d AND state = 'maybe_sent'`, msg)); n != 1 {
		t.Errorf("a refused resolve changed the message")
	}
	lock.Release()

	out := mustRun(t, "leads", "resolve", id, "sent")
	wantLines(t, "resolve sent", out, fmt.Sprintf("Message %d sent Tue 27 Oct 09:30 UTC; lead ", msg), "is now contacted")
	var (
		state, leadState string
		sentAt, dueAt    time.Time
		country          string
	)
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT m.state, m.sent_at, l.state, n.due_at, o.country
		 FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 JOIN outreach_message n ON n.batch_id = m.batch_id AND n.lead_id = m.lead_id AND n.step = 1
		 WHERE m.id = $1`, msg).Scan(&state, &sentAt, &leadState, &dueAt, &country); err != nil {
		t.Fatal(err)
	}
	setup, err := leadsEnv(mustConfig(t), leadsForProbe)
	if err != nil {
		t.Fatal(err)
	}
	want := outreach.FollowupDue(at, setup.leads.Schedule.FollowupBusinessDays[0], setup.leads.Policy(country).Location(), setup.leads.Schedule.BusinessHours)
	if state != leadstore.MsgSent || !sentAt.Equal(at) || leadState != leadstore.LeadContacted || !dueAt.Equal(want) {
		t.Errorf("after resolve sent: %s at %v, lead %s, follow-up due %v (want %v)", state, sentAt, leadState, dueAt, want)
	}
	// Once: a second resolve is refused and schedules nothing more.
	if code, _, errOut := leadsRun(t, "leads", "resolve", id, "sent"); code != 2 || !strings.Contains(errOut, "is not a live email that may have gone out") {
		t.Errorf("second resolve exited %d: %s", code, errOut)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_message WHERE batch_id = 1 AND step > 0 AND due_at IS NOT NULL`); n != 1 {
		t.Errorf("%d follow-ups scheduled, want 1", n)
	}
	for _, args := range [][]string{{"leads", "resolve", id}, {"leads", "resolve", id, "maybe"}, {"leads", "resolve", "x", "sent"}, {"leads", "resolve", "999", "sent"}} {
		if code, _, _ := leadsRun(t, args...); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}
}

// TestLeadsResolveNotSent: a row left sending, checked as not sent, is approved again with no
// attempt time, so today's cap no longer counts it.
func TestLeadsResolveNotSent(t *testing.T) {
	dir := leadsScene(t)
	ctx := context.Background()
	msg, at := liveMaybeSent(t, dir, true)
	lq := leadstoretest.Q(t)
	if n, _ := lq.SentSince(ctx, at.Add(-time.Hour)); n != 1 {
		t.Fatalf("the sending row is not counted: %d", n)
	}
	wantLines(t, "not sent", mustRun(t, "leads", "resolve", fmt.Sprint(msg), "not-sent"), fmt.Sprintf("Message %d not sent: it is approved again", msg))
	if n, _ := lq.SentSince(ctx, at.Add(-time.Hour)); n != 0 {
		t.Errorf("the cap still counts it: %d", n)
	}
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*) FROM outreach_message WHERE id = %d AND state = 'approved' AND sent_at IS NULL`, msg)); n != 1 {
		t.Errorf("not approved again with no attempt time")
	}
	if n := batchCount(t, `SELECT count(*) FROM lead WHERE state = 'contacted'`); n != 0 {
		t.Errorf("%d leads contacted by a not-sent resolve", n)
	}
}

// TestLeadsDiscardApprovedRefused: an approved batch is never discarded.
func TestLeadsDiscardApprovedRefused(t *testing.T) {
	leadsScene(t)
	mustRun(t, "leads", "batch", "new")
	mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))
	before := leadsRows(t)
	code, _, errOut := leadsRun(t, "leads", "batch", "discard", "1")
	if code != 2 || !strings.Contains(errOut, "batch 1 is approved; an approved batch is never discarded") {
		t.Errorf("discard approved exited %d: %s", code, errOut)
	}
	if after := leadsRows(t); !slices.Equal(before, after) {
		t.Errorf("a refused discard wrote rows:\n%s", rowsDiff(before, after))
	}
	code, _, errOut = leadsRun(t, "leads", "batch", "approve", "2", "--hash", "0123456789ab")
	if code != 2 || !strings.Contains(errOut, "no batch 2") {
		t.Errorf("approve missing exited %d: %s", code, errOut)
	}
	// A discarded batch is shown as such and cannot be approved.
	mustRun(t, "leads", "batch", "new")
	mustRun(t, "leads", "batch", "discard", "2")
	wantLines(t, "show discarded", mustRun(t, "leads", "batch", "show", "2"), "· discarded ", "nothing to approve")
	code, _, errOut = leadsRun(t, "leads", "batch", "approve", "2", "--hash", batchHash(t, 2))
	if code != 2 || !strings.Contains(errOut, "batch 2 was discarded; nothing was approved") {
		t.Errorf("approve discarded exited %d: %s", code, errOut)
	}
}

// leadsRows is every row a leads core can write, without ids that differ between runs
// (Message-IDs and hashes are random) and with times reduced to whether they are set,
// except due and sent times, which are deterministic under the test clock.
func leadsRows(t *testing.T) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for _, q := range []string{
		`SELECT format('lead %s %s %s', id, kind, state) FROM lead ORDER BY id`,
		`SELECT format('event %s %s %s %s', lead_id, state, reason, actor) FROM lead_event ORDER BY id`,
		`SELECT format('msg %s b%s l%s s%s %s %s [%s] due %s sent %s approved %s', id, batch_id, lead_id, step, channel, state, cancel_reason,
		   coalesce(to_char(due_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI'), '-'),
		   CASE WHEN state = 'done' THEN (sent_at IS NOT NULL)::text ELSE coalesce(to_char(sent_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI'), '-') END,
		   approved_sha256 IS NOT NULL) FROM outreach_message ORDER BY id`,
		`SELECT format('batch %s shadow %s approved %s discarded %s', id, shadow, approved_at IS NOT NULL, discarded_at IS NOT NULL) FROM outreach_batch ORDER BY id`,
		`SELECT format('suppression %s %s %s %s', kind, value, reason, source) FROM suppression ORDER BY id`,
		`SELECT format('rule %s %s %s %s %s %s', id, kind, field, pattern, reason, coalesce(disabled_reason, '-')) FROM rule ORDER BY id`,
		`SELECT format('org %s %s %s', id, kind, domain) FROM org ORDER BY id`,
		`SELECT format('run %s %s %s', command, note, finished_at IS NOT NULL) FROM run WHERE command LIKE 'leads-batch%' ORDER BY id`,
	} {
		rows, err := storetest.OwnerConn(t).Query(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func rowsDiff(a, b []string) string {
	var d []string
	for _, r := range a {
		if !slices.Contains(b, r) {
			d = append(d, "- "+r)
		}
	}
	for _, r := range b {
		if !slices.Contains(a, r) {
			d = append(d, "+ "+r)
		}
	}
	return strings.Join(d, "\n")
}

// leadsContract is one core driven through its CLI and directly with the same inputs.
type leadsContract struct {
	name string
	prep func(t *testing.T, dir string) // after leadsScene, the same for both paths
	cli  func(t *testing.T) []string
	exit int // the CLI's exit code
	core func(t *testing.T, ce coreEnv) error
	kind RefusalKind // the core's refusal, 0 for none
	// console is the source a console write records where the CLI records "cli".
	console bool
}

// TestLeadsCoresContract: every leads core writes through the CLI exactly what it writes
// when called directly with the same inputs, refusals included (nothing).
func TestLeadsCoresContract(t *testing.T) {
	ctx := context.Background()
	newShadow := func(t *testing.T, _ string) { mustRun(t, "leads", "batch", "new") }
	approved := func(t *testing.T, dir string) {
		newShadow(t, dir)
		mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))
	}
	formMsg := func(t *testing.T) int64 {
		return int64(batchCount(t, `SELECT min(id)::int FROM outreach_message WHERE channel = 'form'`))
	}
	maybe := func(t *testing.T, dir string) { liveMaybeSent(t, dir, false) }
	maybeMsg := func(t *testing.T) int64 {
		return int64(batchCount(t, `SELECT min(id)::int FROM outreach_message WHERE state = 'maybe_sent'`))
	}
	ruleAdded := func(t *testing.T, _ string) {
		mustRun(t, "rules", "add", "--kind", "skip", "--field", "company", "--pattern", "acme", "--reason", "agency")
	}
	cases := []leadsContract{
		{name: "batch new", cli: func(*testing.T) []string { return []string{"leads", "batch", "new", "--size", "2"} },
			core: func(t *testing.T, ce coreEnv) error {
				_, err := makeBatch(ctx, ce.s, newBatchSetup(ce.setup, ce.loc), leadsNow(), NewBatchRequest{Size: 2})
				return err
			}},
		{name: "batch new waits", prep: newShadow, cli: func(*testing.T) []string { return []string{"leads", "batch", "new"} },
			core: func(t *testing.T, ce coreEnv) error {
				_, err := makeBatch(ctx, ce.s, newBatchSetup(ce.setup, ce.loc), leadsNow(), NewBatchRequest{})
				return err
			}, kind: RefuseBatchWaiting},
		{name: "approve with a skip", prep: newShadow,
			cli: func(t *testing.T) []string {
				return []string{"leads", "batch", "approve", "1", "--hash", batchHash(t, 1), "--skip", "2"}
			},
			core: func(t *testing.T, ce coreEnv) error {
				_, err := approveBatch(ctx, ce.s, ce.setup.leads, leadsNow, ApproveRequest{ID: 1, HashPrefix: batchHash(t, 1), Skip: []int{2}})
				return err
			}},
		{name: "approve wrong hash", prep: newShadow, exit: 2,
			cli: func(*testing.T) []string { return []string{"leads", "batch", "approve", "1", "--hash", "0123456789ab"} },
			core: func(t *testing.T, ce coreEnv) error {
				_, err := approveBatch(ctx, ce.s, ce.setup.leads, leadsNow, ApproveRequest{ID: 1, HashPrefix: "0123456789ab"})
				return err
			}, kind: RefuseHashMismatch},
		{name: "approve bad skip", prep: newShadow, exit: 2,
			cli: func(t *testing.T) []string {
				return []string{"leads", "batch", "approve", "1", "--hash", batchHash(t, 1), "--skip", "9"}
			},
			core: func(t *testing.T, ce coreEnv) error {
				_, err := approveBatch(ctx, ce.s, ce.setup.leads, leadsNow, ApproveRequest{ID: 1, HashPrefix: batchHash(t, 1), Skip: []int{9}})
				if r := asRefusal(err); r != nil && r.Items != 5 {
					t.Errorf("bad skip refusal names %d items, want 5", r.Items)
				}
				return err
			}, kind: RefuseBadSkip},
		{name: "discard", prep: newShadow, cli: func(*testing.T) []string { return []string{"leads", "batch", "discard", "1"} },
			core: func(t *testing.T, ce coreEnv) error { _, err := discardBatch(ctx, ce.s, 1); return err }},
		{name: "discard approved", prep: approved, exit: 2, cli: func(*testing.T) []string { return []string{"leads", "batch", "discard", "1"} },
			core: func(t *testing.T, ce coreEnv) error { _, err := discardBatch(ctx, ce.s, 1); return err }, kind: RefuseBatchApproved},
		{name: "done", prep: approved, cli: func(t *testing.T) []string { return []string{"leads", "done", fmt.Sprint(formMsg(t))} },
			core: func(t *testing.T, ce coreEnv) error { _, err := markDone(ctx, ce.s, formMsg(t)); return err }},
		{name: "done an email", prep: approved, exit: 2, cli: func(*testing.T) []string { return []string{"leads", "done", "1"} },
			core: func(t *testing.T, ce coreEnv) error { _, err := markDone(ctx, ce.s, 1); return err }, kind: RefuseNotManual},
		{name: "suppress", cli: func(*testing.T) []string {
			return []string{"leads", "suppress", "add", "domain", "litware.example", "--reason", "opt_out"}
		},
			core: func(t *testing.T, ce coreEnv) error {
				_, err := suppressAdd(ctx, ce.s, "domain", "litware.example", "opt_out", suppressFromConsole)
				return err
			}, console: true},
		{name: "suppress bad reason", exit: 2, cli: func(*testing.T) []string {
			return []string{"leads", "suppress", "add", "domain", "litware.example", "--reason", "annoyed"}
		},
			core: func(t *testing.T, ce coreEnv) error {
				_, err := suppressAdd(ctx, ce.s, "domain", "litware.example", "annoyed", suppressFromConsole)
				return err
			}, kind: RefuseSuppressReason},
		{name: "mark contacted", cli: func(*testing.T) []string {
			return []string{"leads", "mark-contacted", "northwind.example", "--note", "called them"}
		},
			core: func(t *testing.T, ce coreEnv) error {
				_, err := markContacted(ctx, ce.s, "northwind.example", "called them")
				return err
			}},
		{name: "rule add", cli: func(*testing.T) []string {
			return []string{"rules", "add", "--kind", "flag", "--field", "title", "--pattern", "contract", "--reason", "contract roles"}
		},
			core: func(t *testing.T, ce coreEnv) error {
				_, err := ruleAdd(ctx, ce.s, RuleAddRequest{Kind: "flag", Field: "title", Pattern: "contract", Reason: "contract roles"})
				return err
			}},
		{name: "rule add bad pattern", exit: 2, cli: func(*testing.T) []string {
			return []string{"rules", "add", "--kind", "skip", "--field", "company", "--pattern", "acme(", "--reason", "x"}
		},
			core: func(t *testing.T, ce coreEnv) error {
				_, err := ruleAdd(ctx, ce.s, RuleAddRequest{Kind: "skip", Field: "company", Pattern: "acme(", Reason: "x"})
				return err
			}, kind: RefuseRuleInvalid},
		{name: "rule disable", prep: ruleAdded, cli: func(*testing.T) []string { return []string{"rules", "disable", "1", "--reason", "fine"} },
			core: func(t *testing.T, ce coreEnv) error { return ruleDisable(ctx, ce.s, 1, "fine") }},
		{name: "rule disable unknown", exit: 2, cli: func(*testing.T) []string { return []string{"rules", "disable", "7", "--reason", "fine"} },
			core: func(t *testing.T, ce coreEnv) error { return ruleDisable(ctx, ce.s, 7, "fine") }, kind: RefuseRuleNotFound},
		{name: "resolve sent", prep: maybe, cli: func(t *testing.T) []string { return []string{"leads", "resolve", fmt.Sprint(maybeMsg(t)), "sent"} },
			core: func(t *testing.T, ce coreEnv) error {
				_, err := resolveMaybeSent(ctx, ce.s, ce.setup, ce.loc, maybeMsg(t), true)
				return err
			}},
		{name: "resolve not sent", prep: maybe, cli: func(t *testing.T) []string { return []string{"leads", "resolve", fmt.Sprint(maybeMsg(t)), "not-sent"} },
			core: func(t *testing.T, ce coreEnv) error {
				_, err := resolveMaybeSent(ctx, ce.s, ce.setup, ce.loc, maybeMsg(t), false)
				return err
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := func(viaCLI bool) (before, after []string) {
				dir := leadsScene(t)
				if c.prep != nil {
					c.prep(t, dir)
				}
				before = leadsRows(t)
				if viaCLI {
					if code, out, errOut := leadsRun(t, c.cli(t)...); code != c.exit {
						t.Fatalf("CLI exited %d, want %d: %s%s", code, c.exit, out, errOut)
					}
				} else {
					err := c.core(t, openCoreEnv(t))
					if c.kind == 0 && err != nil {
						t.Fatalf("core: %v", err)
					}
					if c.kind != 0 {
						wantRefusal(t, "core", err, c.kind)
					}
				}
				after = leadsRows(t)
				if !viaCLI && c.console {
					for i, r := range after {
						// The source is in the suppression row and the lead event's reason.
						r = strings.Replace(r, " "+suppressFromConsole, " "+suppressFromCLI, 1)
						after[i] = strings.Replace(r, "("+suppressFromConsole+")", "("+suppressFromCLI+")", 1)
					}
				}
				return before, after
			}
			b1, viaCLI := run(true)
			b2, direct := run(false)
			if !slices.Equal(b1, b2) {
				t.Fatalf("the two starts differ:\n%s", rowsDiff(b1, b2))
			}
			if !slices.Equal(viaCLI, direct) {
				t.Errorf("CLI and core wrote different rows:\n%s", rowsDiff(viaCLI, direct))
			}
			wrote := !slices.Equal(b1, viaCLI)
			if wrote != (c.kind == 0) && c.kind != RefuseBatchWaiting {
				t.Errorf("wrote rows = %v, want %v", wrote, c.kind == 0)
			}
		})
	}
}

// TestLeadsSizingCountsTodayAndToCheck: a live batch's sizing takes today's sends off
// today's cap and counts a maybe-sent email as still waiting.
func TestLeadsSizingCountsTodayAndToCheck(t *testing.T) {
	dir := leadsScene(t)
	liveMaybeSent(t, dir, false) // batch 1: 2 email leads, 6 emails; one maybe sent today
	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "sizing", out, "Batch 2 (live): ",
		// Today: 10 less 1 sent; then 10 and 10. Waiting: 5 approved, 1 to check. 23 / 3 = 7.
		"Sizing: 7 leads: room for 23 emails over the next 3 days (29 left in the warm-up ramp after 1 sent today, 5 approved and waiting, 1 to check by hand)")
}

// TestLeadsSelftestNeverCheckedByHand: a selftest that may have gone is left out of the
// console's check-by-hand list and refused by resolve, through the CLI and the core.
func TestLeadsSelftestNeverCheckedByHand(t *testing.T) {
	dir := leadsScene(t)
	ctx := context.Background()
	live, _ := liveMaybeSent(t, dir, false)
	lq := leadstoretest.Q(t)
	var mailboxID int64
	if err := storetest.AppConn(t).QueryRow(ctx, `SELECT min(id) FROM sender_mailbox`).Scan(&mailboxID); err != nil {
		t.Fatal(err)
	}
	st, err := lq.CreateSelftest(ctx, leadstore.SelftestDraft{To: "operator@" + "sender.example", Subject: "selftest", Body: "selftest",
		MessageID: "<selftest.1@shadow.invalid>", Due: leadsTuesday, MailboxID: mailboxID})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := lq.MarkSending(ctx, st.MessageID); err != nil || !ok {
		t.Fatalf("mark sending: %v %v", ok, err)
	}
	if err := lq.MarkMaybeSent(ctx, st.MessageID, "connection lost"); err != nil {
		t.Fatal(err)
	}
	items, err := checkItems(ctx, storetest.AppConn(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].MessageID != live {
		t.Errorf("check-by-hand list = %+v, want only message %d", items, live)
	}
	code, _, errOut := leadsRun(t, "leads", "resolve", fmt.Sprint(st.MessageID), "not-sent")
	if code != 2 || !strings.Contains(errOut, "is a selftest's; a selftest is never checked by hand") {
		t.Errorf("resolve a selftest exited %d: %s", code, errOut)
	}
	ce := openCoreEnv(t)
	_, err = resolveMaybeSent(ctx, ce.s, ce.setup, ce.loc, st.MessageID, true)
	wantRefusal(t, "core resolve of a selftest", err, RefuseSelftest)

	// A hand-made console request: the console's own words, 422, nothing changed.
	before := leadsRows(t)
	h, cookie := jobsHandler(t, leadsBackend(t))
	r := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/leads/messages/%d/resolve", st.MessageID),
		strings.NewReader(url.Values{"sent": {"sent"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Cookie", cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	want := fmt.Sprintf("Message %d belongs to a selftest; a selftest is never checked by hand.", st.MessageID)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), "leadhound leads") {
		t.Errorf("console resolve of a selftest: %d, want 422 with %q:\n%s", w.Code, want, w.Body.String())
	}
	if after := leadsRows(t); !slices.Equal(before, after) {
		t.Errorf("the console's refused resolve wrote rows:\n%s", rowsDiff(before, after))
	}
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*) FROM outreach_message WHERE id = %d AND state = 'maybe_sent'`, st.MessageID)); n != 1 {
		t.Errorf("a refused resolve changed the selftest")
	}
}
