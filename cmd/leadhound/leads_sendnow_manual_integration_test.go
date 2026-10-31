//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
	"github.com/gremy/webbership-leadhound/internal/verify"
)

// manualScene is leadsScene live, with the send timer counted as enabled (no systemd here:
// readTimers infers it from these settings) and the console backend; it returns the backend,
// the data directory and the new leads whose route is an email, lowest id first.
func manualScene(t *testing.T) (*backend, string, []int64) {
	t.Helper()
	dir := leadsScene(t)
	goLive(t)
	t.Setenv("LEADHOUND_LEADS_TIMERS", "on")
	setCaps(t, dir, "first-send", "[{from_day: 0, per_day: 4}]")
	return leadsBackend(t), dir, emailLeads(t)
}

func emailLeads(t *testing.T) []int64 {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT l.id FROM lead l JOIN contact_route r ON r.id = l.route_id
		 WHERE l.state = 'new' AND r.kind = 'email' ORDER BY l.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) < 2 {
		t.Fatalf("the scene has %d email leads", len(ids))
	}
	return ids
}

// confirmOf is the press of the page that showed p: its hash and every warning code.
func confirmOf(p console.SendPlan) console.ManualConfirm {
	c := console.ManualConfirm{BatchID: p.BatchID, HashPrefix: p.HashPrefix, Operator: "operator@example.test"}
	for _, w := range p.Warnings {
		if !slices.Contains(c.Codes, w.Code) {
			c.Codes = append(c.Codes, w.Code)
		}
	}
	return c
}

func planLead(p console.SendPlan, id int64) console.SendPlanLead {
	for _, l := range p.Leads {
		if l.LeadID == id {
			return l
		}
	}
	return console.SendPlanLead{}
}

func approvedCount(t *testing.T, lead int64) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT count(*)::int FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE m.lead_id = $1 AND b.manual AND m.state NOT IN ('draft', 'cancelled', 'skipped')`, lead).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSendNowPlanPreviewConfirm: a preview of two chosen leads makes one manual batch, the
// same preview again reuses it, confirm approves it with the first emails due now, writes the
// send_request row with the warnings shown and then exactly one JSON line in the request
// file; a second confirm is refused and asks for nothing more.
func TestSendNowPlanPreviewConfirm(t *testing.T) {
	ctx := context.Background()
	b, dir, ids := manualScene(t)
	pick := ids[:2]
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: pick})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan %+v", p)
	if !p.CanSend || len(p.HashPrefix) != leadstore.MinHashPrefix {
		t.Fatalf("plan cannot send: %+v", p)
	}
	for _, id := range pick {
		if l := planLead(p, id); !l.OK && l.Stop == "" && l.Waiting == "" {
			t.Errorf("lead %d not in the plan: %+v", id, l)
		}
	}
	var manual bool
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT manual FROM outreach_batch WHERE id = $1`, p.BatchID).Scan(&manual); err != nil || !manual {
		t.Fatalf("batch %d manual %v: %v", p.BatchID, manual, err)
	}
	again, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: pick})
	if err != nil || again.BatchID != p.BatchID {
		t.Fatalf("the same leads again: batch %d (was %d): %v", again.BatchID, p.BatchID, err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch WHERE discarded_at IS NULL`); n != 1 {
		t.Errorf("%d open batches", n)
	}
	// The daily batch is not blocked by a manual one.
	if id, ok, err := b.lq().UnapprovedBatch(ctx, leadstore.DefaultAccountID, false); err != nil || ok {
		t.Errorf("UnapprovedBatch sees %d %v %v", id, ok, err)
	}
	c := confirmOf(p)
	out, err := b.SendNowConfirm(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out.Message)
	if !strings.HasPrefix(out.Message, "Asked. ") {
		t.Errorf("message %q", out.Message)
	}
	var codes []string
	var hash, op string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT codes, hash_prefix, operator FROM send_request WHERE batch_id = $1`, p.BatchID).
		Scan(&codes, &hash, &op); err != nil {
		t.Fatal(err)
	}
	slices.Sort(codes)
	want := slices.Clone(c.Codes)
	slices.Sort(want)
	if !slices.Equal(codes, want) || hash != p.HashPrefix || op != c.Operator {
		t.Errorf("send_request %v %q %q, want %v %q", codes, hash, op, want, p.HashPrefix)
	}
	raw, err := os.ReadFile(filepath.Join(dir, runRequestDir, sendNowAction)) // #nosec G304 -- the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 1 || !strings.HasSuffix(string(raw), "\n") {
		t.Errorf("request file %q is not one line", raw)
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil || len(req) != 2 || req["batch_id"] != float64(p.BatchID) {
		t.Errorf("request file %q: %v", raw, err)
	}
	if at, err := time.Parse(time.RFC3339Nano, req["at"].(string)); err != nil || !strings.HasSuffix(req["at"].(string), "Z") {
		t.Errorf("at %v %v", at, err)
	}
	if id, kind, err := readSendNowRequest(dir); err != nil || id != p.BatchID || kind != sendRequestManual {
		t.Errorf("the send reads %d %q %v", id, kind, err)
	}
	if st, err := os.Stat(filepath.Join(dir, runRequestDir, sendNowAction)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("request file mode %v %v", st.Mode(), err)
	}
	// The first emails are due now, never at a later slot.
	var late int
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT count(*)::int FROM outreach_message WHERE batch_id = $1 AND step = 0 AND state = 'approved' AND (due_at IS NULL OR due_at > $2)`,
		p.BatchID, leadsNow()).Scan(&late); err != nil || late != 0 {
		t.Errorf("%d first emails not due now: %v", late, err)
	}
	// Twice: refused, and nothing more is asked.
	if err := os.Remove(filepath.Join(dir, runRequestDir, sendNowAction)); err != nil {
		t.Fatal(err)
	}
	_, err = b.SendNowConfirm(ctx, c)
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "already sent") {
		t.Errorf("confirm twice: %v", err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM send_request`); n != 1 {
		t.Errorf("%d send requests", n)
	}
	if _, err := os.Stat(filepath.Join(dir, runRequestDir, sendNowAction)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a second request file: %v", err)
	}
	if _, err := b.SendNowPlan(ctx, p.BatchID); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("the plan of a sent batch: %v", err)
	}
}

// TestSendNowPlanTamper: a different hash, or codes missing a warning shown, is
// PlanChangedError and approves nothing.
func TestSendNowPlanTamper(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	// The scene's morning is before the recipients' business hours, so the page warns.
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) == 0 {
		t.Fatalf("no warnings before business hours: %+v", p)
	}
	for _, w := range p.Warnings {
		t.Logf("%s: %s", w.Code, w.Sentence)
	}
	var changed *console.PlanChangedError
	bad := confirmOf(p)
	bad.HashPrefix = "0123456789ab"
	if _, err := b.SendNowConfirm(ctx, bad); !errors.As(err, &changed) {
		t.Errorf("a wrong hash: %v", err)
	}
	short := confirmOf(p)
	short.Codes = short.Codes[1:]
	if _, err := b.SendNowConfirm(ctx, short); !errors.As(err, &changed) {
		t.Errorf("a warning not posted: %v", err)
	}
	if approvedCount(t, ids[0]) != 0 || batchCount(t, `SELECT count(*)::int FROM send_request`) != 0 {
		t.Fatal("a tampered confirm approved something")
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
		t.Errorf("the honest press: %v", err)
	}
}

// TestSendNowPlanHardStops: every hard stop holds against every warning code posted: the lead
// is never approved and no send_request can lift it.
func TestSendNowPlanHardStops(t *testing.T) {
	all := []string{console.WarnWindow, console.WarnBusinessHours, console.WarnRamp, console.WarnDomainDaily,
		console.WarnSpacingJitter, console.WarnSchedule}
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, b *backend, lead int64) // between preview and confirm
		whole bool                                       // the stop holds for every lead
	}{
		{name: "suppressed", after: func(t *testing.T, b *backend, lead int64) {
			ownerExec(t, `INSERT INTO suppression (account_id, kind, value, reason)
			  SELECT 1, 'domain', lower(o.domain), 'manual' FROM lead l JOIN org o ON o.id = l.org_id WHERE l.id = $1`, lead)
		}},
		{name: "email check invalid", after: func(t *testing.T, b *backend, lead int64) {
			setVerify(t, true)
			ownerExec(t, `INSERT INTO email_check (account_id, address, provider, status, checked_at)
			  SELECT 1, lower(m.to_address), 'reoon', 'invalid', now() FROM outreach_message m WHERE m.lead_id = $1 AND m.step = 0`, lead)
		}},
		{name: "mailbox paused", whole: true, after: func(t *testing.T, b *backend, lead int64) {
			ownerExec(t, `INSERT INTO mailbox_state (mailbox_id, state, reason, actor)
			  SELECT DISTINCT m.mailbox_id, 'paused', 'test', 'operator' FROM outreach_message m WHERE m.lead_id = $1`, lead)
		}},
		{name: "mailbox ceiling", whole: true, after: func(t *testing.T, b *backend, lead int64) {
			fillMailbox(t, lead, 40)
		}},
		{name: "sending paused", whole: true, after: func(t *testing.T, b *backend, lead int64) {
			if err := b.lq().SetSendPause(context.Background(), true, "test", leadstore.ActorGremy); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "shadow mode", whole: true, after: func(t *testing.T, b *backend, lead int64) {
			t.Setenv("LEADHOUND_OUTREACH", "")
		}},
		{name: "mailbox unassigned", whole: true, after: func(t *testing.T, b *backend, lead int64) {
			ownerExec(t, `DELETE FROM campaign_mailbox WHERE campaign_id = (SELECT campaign_id FROM lead WHERE id = $1)`, lead)
		}},
		{name: "reserved domain", after: func(t *testing.T, b *backend, lead int64) {
			saved := leadsMail
			leadsMail.allowReserved = false
			t.Cleanup(func() { leadsMail = saved })
		}},
		{name: "draft changed", whole: true, after: func(t *testing.T, b *backend, lead int64) {
			ownerExec(t, `UPDATE outreach_message SET body_text = body_text || ' changed' WHERE lead_id = $1 AND step = 0`, lead)
		}},
		{name: "lead skipped", after: func(t *testing.T, b *backend, lead int64) {
			ownerExec(t, `UPDATE lead SET state = 'closed' WHERE id = $1`, lead)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, _, ids := manualScene(t)
			lead := ids[0]
			p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: []int64{lead}})
			if err != nil {
				t.Fatal(err)
			}
			tc.after(t, b, lead)
			c := confirmOf(p)
			c.Codes = all
			for range 2 { // the second press is after the page showed the stop
				_, err := b.SendNowConfirm(ctx, c)
				var changed *console.PlanChangedError
				switch {
				case errors.As(err, &changed):
					if l := planLead(changed.Plan, lead); l.Stop == "" || l.OK || changed.Plan.CanSend {
						t.Errorf("the plan now: %+v", changed.Plan)
					} else {
						t.Logf("stop: %s", l.Stop)
					}
				case err == nil:
					t.Fatal("confirm went through")
				}
			}
			if n := approvedCount(t, lead); n != 0 {
				t.Errorf("%d messages approved", n)
			}
			if n := batchCount(t, `SELECT count(*)::int FROM send_request`); n != 0 {
				t.Errorf("%d send requests", n)
			}
			if n := batchCount(t, `SELECT count(*)::int FROM send_override`); n != 0 {
				t.Errorf("%d overrides", n)
			}
		})
	}
}

// fillMailbox records n live sends today from the mailbox of lead's first email: n batches of
// one sent copy of it each, as another lead's (a lead has one message per step and batch, and
// its own first email elsewhere would stop it).
func fillMailbox(t *testing.T, lead int64, n int) {
	t.Helper()
	ownerExec(t, `WITH bs AS (
	    INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256, account_id, campaign_id)
	    SELECT false, '\x00'::bytea, '\x00'::bytea, 1, (SELECT campaign_id FROM lead WHERE id = $1) FROM generate_series(1, $2)
	    RETURNING id)
	  INSERT INTO outreach_message (batch_id, lead_id, step, channel, to_address, subject, body_text, message_id, in_reply_to,
	    slots, mailbox_id, from_name, condition, state, sent_at)
	  SELECT bs.id, (SELECT x.id FROM lead x WHERE x.id <> $1 ORDER BY x.id DESC LIMIT 1), 0, 'email', m.to_address, m.subject, m.body_text, bs.id || m.message_id, m.in_reply_to,
	    m.slots, m.mailbox_id, m.from_name, m.condition, 'sent', $3
	  FROM bs, (SELECT * FROM outreach_message WHERE lead_id = $1 AND step = 0 ORDER BY id DESC LIMIT 1) m`, lead, n, leadsNow())
}

// TestSendNowPlanRamp: a first-ever send (ramp day 0, nothing sent) goes with no ramp warning;
// once the day's warm-up room is used, the same send warns with the numbers, and confirm
// goes past it only with the code posted.
func TestSendNowPlanRamp(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range p.Warnings {
		if w.Code == console.WarnRamp || w.Code == console.WarnDomainDaily {
			t.Errorf("cold start warns: %s", w.Sentence)
		}
	}
	fillMailbox(t, ids[0], 4)
	if p, err = b.SendNowPlan(ctx, p.BatchID); err != nil {
		t.Fatal(err)
	}
	var ramp string
	for _, w := range p.Warnings {
		if w.Code == console.WarnRamp {
			ramp = w.Sentence
		}
	}
	if !strings.Contains(ramp, "has sent 4 of 4 today (warm-up). This goes 1 over.") || !p.CanSend {
		t.Fatalf("steady state: %q %+v", ramp, p)
	}
	t.Log(ramp)
	c := confirmOf(p)
	c.Codes = slices.DeleteFunc(slices.Clone(c.Codes), func(s string) bool { return s == console.WarnRamp })
	if _, err := b.SendNowConfirm(ctx, c); !errors.As(err, new(*console.PlanChangedError)) {
		t.Errorf("ramp not posted: %v", err)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
		t.Errorf("ramp posted: %v", err)
	}
}

// TestSendNowPlanWaitsForEmailCheck: with the Email check on, an unchecked address waits (no
// send), Check email now writes the request naming the batch, and the check run reads it.
func TestSendNowPlanWaitsForEmailCheck(t *testing.T) {
	ctx := context.Background()
	b, dir, ids := manualScene(t)
	setVerify(t, true)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if l := planLead(p, ids[0]); l.Waiting != "Email not checked yet" || p.CanSend {
		t.Fatalf("plan %+v", p)
	}
	out, err := b.SendNowCheckEmail(ctx, p.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(out.Message)
	if id, err := readVerifyNowRequest(dir); err != nil || id != p.BatchID {
		t.Errorf("the check reads %d %v", id, err)
	}
	if p, err = b.SendNowPlan(ctx, p.BatchID); err != nil || p.Waiting == "" {
		t.Errorf("the page does not poll while the check runs: %+v %v", p, err)
	}
	bs := newBatchSetup(mustSetup(t, b), b.loc)
	addrs, err := batchVerifyQueue(ctx, b.lq(), bs, leadsNow(), p.BatchID, 25)
	if err != nil || len(addrs) != 1 {
		t.Errorf("the check's addresses %v %v", addrs, err)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); !errors.As(err, new(*console.PlanChangedError)) {
		t.Errorf("confirm while waiting: %v", err)
	}
}

func mustSetup(t *testing.T, b *backend) leadsSetup {
	t.Helper()
	s, err := b.leadsSetup(context.Background(), leadsForOutreach)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSendNowPlanLeadStates: a chosen lead in each state but new is left out at the preview with
// why, and with no other lead there is no batch at all; a new lead goes.
func TestSendNowPlanLeadStates(t *testing.T) {
	for _, tc := range []struct{ state, why string }{
		{"new", ""},
		{"batched", "already in a batch"},
		{"contacted", "contacted"},
		{"replied", "replied"},
		{"suppressed", "suppressed"},
		{"opted_out", "opted out"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			ctx := context.Background()
			b, _, ids := manualScene(t)
			ownerExec(t, `UPDATE lead SET state = $2 WHERE id = $1`, ids[0], tc.state)
			p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
			if tc.why == "" {
				if err != nil || !planLead(p, ids[0]).OK {
					t.Fatalf("a new lead: %+v %v", p, err)
				}
				return
			}
			if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), tc.why) {
				t.Errorf("preview: %v", err)
			}
			if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch`); n != 0 {
				t.Errorf("%d batches", n)
			}
			// With a sendable lead beside it, the batch goes and the plan names it left out.
			p, err = b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
			if err != nil {
				t.Fatal(err)
			}
			if l := planLead(p, ids[0]); l.Stop == "" || l.OK || !planLead(p, ids[1]).OK || !p.CanSend {
				t.Errorf("plan %+v", p)
			} else {
				t.Logf("%s: %s", tc.state, l.Stop)
			}
			if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
				t.Fatal(err)
			}
			if n := approvedCount(t, ids[0]); n != 0 {
				t.Errorf("the %s lead has %d approved", tc.state, n)
			}
		})
	}
}

// TestSendNowPlanTooMany: more than 25 chosen leads is refused before anything is read.
func TestSendNowPlanTooMany(t *testing.T) {
	b, _, _ := manualScene(t)
	ids := make([]int64, manualMaxLeads+1)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if _, err := b.SendNowPreview(context.Background(), console.ManualSendRequest{LeadIDs: ids}); !errors.Is(err, console.ErrUserFixable) ||
		!strings.Contains(err.Error(), "Split it: at most 25 per send") {
		t.Errorf("26 leads: %v", err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch`); n != 0 {
		t.Errorf("%d batches", n)
	}
}

// TestSendNowPlanDiscardsStale: a manual batch left unconfirmed past the window is discarded by
// the next preview, and choosing other leads discards an open one holding a chosen lead.
func TestSendNowPlanDiscardsStale(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	q, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err != nil || q.BatchID == p.BatchID {
		t.Fatalf("other leads: batch %d (was %d) %v", q.BatchID, p.BatchID, err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch WHERE discarded_at IS NULL`); n != 1 {
		t.Errorf("%d open batches", n)
	}
	ownerExec(t, `UPDATE outreach_batch SET created_at = $2 WHERE id = $1`, q.BatchID, leadsNow().Add(-31*time.Minute))
	r, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[1:2]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendNowPlan(ctx, q.BatchID); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("the stale batch: %v", err)
	}
	if _, err := b.SendNowPlan(ctx, r.BatchID); err != nil {
		t.Error(err)
	}
}

// TestSendNowPlanCheckEmailRun: Check email now's run (leads verify --from-request) checks only
// the addresses of the batch the request names, and the plan then lets the lead go; a request
// file that is not one request line checks nothing.
func TestSendNowPlanCheckEmailRun(t *testing.T) {
	ctx := context.Background()
	b, dir, ids := manualScene(t)
	setVerify(t, true)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err != nil {
		t.Fatal(err)
	}
	addr := planLead(p, ids[0]).Email
	f := &fakeReoon{status: map[string]string{addr: verify.StatusSafe}, bal: verify.Balance{Daily: 10}}
	useReoon(t, f)
	path := filepath.Join(dir, runRequestDir, verifyNowAction)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"batch_id":1,"at":"2026-10-27T08:00:00Z","extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := leadsRun(t, "leads", "verify", "--from-request"); code == 0 || len(f.checked()) != 0 {
		t.Errorf("a malformed request ran: exit %d, checked %v", code, f.checked())
	}
	if _, err := b.SendNowCheckEmail(ctx, p.BatchID); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "verify", "--from-request")
	if got := f.checked(); !slices.Equal(got, []string{addr}) {
		t.Errorf("checked %v, want %s", got, addr)
	}
	if p, err = b.SendNowPlan(ctx, p.BatchID); err != nil || !planLead(p, ids[0]).OK || !p.CanSend || p.Waiting != "" {
		t.Errorf("after the check: %+v %v", p, err)
	}
}

// TestSendNowPlanDailyBatch: "Approve and send now" on the daily batch previews it without
// marking it (so the stale-batch trim never takes it). Its contact form and LinkedIn note are
// no hard stop: confirm approves them as Approve does, to do by hand, and the run sends the
// emails now. No lead is skipped.
func TestSendNowPlanDailyBatch(t *testing.T) {
	ctx := context.Background()
	b, _, _ := manualScene(t)
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch})
	if err != nil || out.BatchID == 0 {
		t.Fatalf("daily batch: %+v %v", out, err)
	}
	if n := batchCount(t, `SELECT count(DISTINCT lead_id)::int FROM outreach_message WHERE step = 0 AND channel <> 'email'`); n == 0 {
		t.Fatal("the daily batch has no contact form or LinkedIn lead")
	}
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{BatchID: out.BatchID})
	if err != nil {
		t.Fatal(err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch WHERE manual`); n != 0 {
		t.Errorf("previewing marked %d batches manual", n)
	}
	oks := 0
	for _, l := range p.Leads {
		if l.Stop != "" || l.Waiting != "" {
			t.Errorf("%s: stop %q waiting %q", l.Domain, l.Stop, l.Waiting)
		}
		if l.OK {
			oks++
		}
	}
	if oks == 0 || !p.CanSend || !slices.ContainsFunc(p.Notes, func(n string) bool { return strings.Contains(n, "approved for you to do by hand") }) {
		t.Fatalf("plan %+v", p)
	}
	t.Log(p.Notes)
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
		t.Fatal(err)
	}
	leadsMail.sleep = func(context.Context, time.Duration) error { return nil } // goLive restores leadsMail
	mustRun(t, "leads", "send", "--from-request")
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch WHERE manual AND approved_at IS NOT NULL`); n != 1 {
		t.Errorf("%d manual approved batches", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE step = 0 AND channel <> 'email' AND (state <> 'approved' OR due_at IS NOT NULL)`); n != 0 {
		t.Errorf("%d contact form or LinkedIn items not approved to do by hand", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE step = 0 AND channel = 'email' AND state = 'sent'`); n == 0 {
		t.Error("no email sent")
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE state = 'skipped'`); n != 0 {
		t.Errorf("%d messages skipped", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM lead WHERE state = 'skipped'`); n != 0 {
		t.Errorf("%d leads skipped", n)
	}
}

// TestSendNowPlanDailyBatchBlocked: on the daily batch, a hard stop on one of its email leads
// stops the whole Send now: the plan says why and points at Approve, confirm is refused, and
// nothing is approved or skipped.
func TestSendNowPlanDailyBatchBlocked(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch})
	if err != nil || out.BatchID == 0 {
		t.Fatalf("daily batch: %+v %v", out, err)
	}
	ownerExec(t, `INSERT INTO mailbox_state (mailbox_id, state, reason, actor)
	  SELECT DISTINCT m.mailbox_id, 'paused', 'test', 'operator' FROM outreach_message m WHERE m.lead_id = $1`, ids[0])
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{BatchID: out.BatchID})
	if err != nil {
		t.Fatal(err)
	}
	if p.CanSend || !slices.ContainsFunc(p.Notes, func(n string) bool {
		return strings.Contains(n, "can't go now: Mailbox") && strings.Contains(n, "Use Approve to send the batch on its normal schedule")
	}) {
		t.Fatalf("plan %+v", p)
	}
	t.Log(p.Notes)
	c := confirmOf(p)
	c.Codes = []string{console.WarnWindow, console.WarnBusinessHours, console.WarnRamp, console.WarnDomainDaily, console.WarnSpacingJitter, console.WarnSchedule}
	for range 2 {
		if _, err := b.SendNowConfirm(ctx, c); !errors.As(err, new(*console.PlanChangedError)) {
			t.Fatalf("confirm: %v", err)
		}
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch WHERE approved_at IS NOT NULL OR manual`); n != 0 {
		t.Errorf("%d batches approved or marked", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE state <> 'draft'`); n != 0 {
		t.Errorf("%d messages past draft", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM lead WHERE state = 'skipped'`); n != 0 {
		t.Errorf("%d leads skipped", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM send_request`); n != 0 {
		t.Errorf("%d send requests", n)
	}
}

// TestSendNowPlanRunContract: what the confirm page says goes now is what the send run sends,
// with every warning the page showed confirmed and nothing more: before the recipients'
// business hours, with the jitter, nothing is held.
func TestSendNowPlanRunContract(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	ok := 0
	for _, l := range p.Leads {
		if l.OK {
			ok++
		}
	}
	if ok != 2 || len(p.Warnings) == 0 {
		t.Fatalf("plan %+v", p)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	leadsMail.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil } // goLive restores leadsMail
	out := mustRun(t, "leads", "send", "--from-request")
	if !slices.Equal(slept, []time.Duration{time.Minute}) {
		t.Errorf("spacing %v, want one minute", slept)
	}
	t.Log(out)
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE step = 0 AND state = 'sent'`); n != ok {
		t.Errorf("%d first emails sent, the page said %d", n, ok)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM send_override`); n != ok {
		t.Errorf("%d override rows for %d emails", n, ok)
	}
}

// TestSendNowPlanFirstEmailElsewhere: in dev shadow mode (leads stay new however often they are
// batched), a lead whose first email is already in another batch is never offered again: a
// preview of it after its Send now went is refused with the batch named, and a batch approved
// under an open confirm page makes the confirm a PlanChangedError with the lead stopped.
func TestSendNowPlanFirstEmailElsewhere(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	b.dev = true
	a, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if err != nil || !planLead(a, ids[0]).OK {
		t.Fatalf("shadow preview: %+v %v", a, err)
	}
	out, err := b.SendNowConfirm(ctx, confirmOf(a))
	if err != nil {
		t.Fatal(err)
	}
	if want := "Asked. In dev nothing watches the request: run leadhound leads send --from-request to write them to the shadow outbox."; out.Message != want {
		t.Errorf("dev message %q", out.Message)
	}
	_, err = b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:1]})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Its first email is already in batch ") {
		t.Errorf("the same lead again: %v", err)
	}
	// With another lead beside it, the other goes and this one is named left out.
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil {
		t.Fatal(err)
	}
	if l := planLead(p, ids[0]); !strings.HasPrefix(l.Stop, "Its first email is already in batch ") || !planLead(p, ids[1]).OK {
		t.Errorf("plan %+v", p)
	}
	// An open confirm page, then the same lead's first email approved in another batch.
	mustRun(t, "leads", "send", "--from-request") // batch a goes (shadow): its first email shadow_sent
	c, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[2:3]})
	if err != nil || !planLead(c, ids[2]).OK {
		t.Fatalf("third lead: %+v %v", c, err)
	}
	mustRun(t, "leads", "batch", "new")
	var daily int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT m.batch_id FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
	  WHERE NOT b.manual AND m.lead_id = $1 AND m.step = 0`, ids[2]).Scan(&daily); err != nil {
		t.Fatalf("the daily batch holds lead %d: %v", ids[2], err)
	}
	mustRun(t, "leads", "batch", "approve", strconv.FormatInt(daily, 10), "--hash", batchHash(t, daily))
	_, err = b.SendNowConfirm(ctx, confirmOf(c))
	var changed *console.PlanChangedError
	if !errors.As(err, &changed) || !strings.HasPrefix(planLead(changed.Plan, ids[2]).Stop, "Its first email is already in batch ") || changed.Plan.CanSend {
		t.Fatalf("confirm after the lead was approved elsewhere: %v", err)
	}
	if n := approvedCount(t, ids[2]); n != 0 {
		t.Errorf("%d manual messages approved", n)
	}
}

// TestSendNowPlanShownWarnings: the send_request row carries the warnings exactly as the page
// showed them, code and sentence with its numbers.
func TestSendNowPlanShownWarnings(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: ids[:2]})
	if err != nil || len(p.Warnings) == 0 {
		t.Fatalf("plan %+v %v", p, err)
	}
	if _, err := b.SendNowConfirm(ctx, confirmOf(p)); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT shown FROM send_request WHERE batch_id = $1`, p.BatchID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var got []struct{ Code, Sentence string }
	if err := json.Unmarshal(raw, &got); err != nil || len(got) != len(p.Warnings) {
		t.Fatalf("shown %s: %v", raw, err)
	}
	for i, w := range p.Warnings {
		if got[i].Code != w.Code || got[i].Sentence != w.Sentence {
			t.Errorf("shown %d: %+v, page %+v", i, got[i], w)
		}
	}
	t.Logf("%s", raw)
}

// TestSendNowPlanNoEmailAddress: a chosen lead with no email address is left out saying so.
func TestSendNowPlanNoEmailAddress(t *testing.T) {
	ctx := context.Background()
	b, _, ids := manualScene(t)
	var form int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT l.id FROM lead l WHERE l.state = 'new'
	  AND NOT EXISTS (SELECT 1 FROM contact_route r WHERE r.org_id = l.org_id AND r.kind = 'email') ORDER BY l.id LIMIT 1`).Scan(&form); err != nil {
		t.Skipf("the scene has no lead without an email address: %v", err)
	}
	// Out of the queue for any reason, its reason is the missing address.
	ownerExec(t, `INSERT INTO lead_priority (lead_id, action, until, actor) VALUES ($1, 'defer', '2099-01-01', 'gremy')`, form)
	p, err := b.SendNowPreview(ctx, console.ManualSendRequest{LeadIDs: []int64{form, ids[0]}})
	if err != nil {
		t.Fatal(err)
	}
	if l := planLead(p, form); l.Stop != "It has no email address to send to." {
		t.Errorf("left out %q", l.Stop)
	}
}
