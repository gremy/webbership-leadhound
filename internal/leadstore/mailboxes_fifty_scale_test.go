//go:build scale

package leadstore_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The fifty-mailbox world (MB-R18): the seed's ten mailboxes over five domains, plus forty
// more, two on each of twenty more domains, added in a transaction the test rolls back, so
// the shared seed is never changed (another run on the same scale database never sees them).
const (
	fiftyMailboxes = 50
	fiftyDomains   = 25
	seedMailboxes  = 10 // what leadstoretest's seed holds, two on each of five domains
)

// fiftyMailboxWorld adds, inside tx, mailboxes 11..50 on mail6..mail25.example with mixed
// states (paused alone, a domain paused, retired, a failed check, a check still requested,
// no check), a year of DNS checks for every new domain (a few failing today), and a year of
// sends: each new mailbox gets a copy of one seed mailbox's messages, in copies of the seed's
// batches, so every mailbox has a first live send, sends today and approved mail due.
func fiftyMailboxWorld(t *testing.T, ctx context.Context, tx pgx.Tx, day time.Time) {
	t.Helper()
	var n, maxBatch, maxMsg int64
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM sender_mailbox), (SELECT max(id) FROM outreach_batch),
		(SELECT max(id) FROM outreach_message)`).Scan(&n, &maxBatch, &maxMsg); err != nil {
		t.Fatal(err)
	}
	if n != seedMailboxes {
		t.Fatalf("seed mailboxes = %d, want %d", n, seedMailboxes)
	}
	// New mailbox id (11..50) -> its copy number k (1..4) and the seed mailbox o (1..10) it copies.
	copyOf := fmt.Sprintf("(1 + (n - %d) %% %d)", seedMailboxes+1, seedMailboxes)
	copyNo := fmt.Sprintf("(1 + (n - %d) / %d)", seedMailboxes+1, seedMailboxes)
	steps := []struct {
		name string
		sql  string
		args []any
	}{
		{"sender_mailbox", `INSERT INTO sender_mailbox (id, address, domain, login, smtp_addr, imap_addr, dkim_selector,
			  from_name, password_sealed, sealed_by, created_at)
			SELECT n, 'out' || n || '@' || d.domain, d.domain, 'out' || n || '@' || d.domain,
			       'smtp.' || d.domain || ':465', 'imap.' || d.domain || ':993', 'lh', '',
			       convert_to('placeholder, sealed by no key', 'UTF8'), 'placeholder', $1::timestamptz - interval '401 days'
			FROM generate_series($2::int, $3::int) n
			CROSS JOIN LATERAL (SELECT 'mail' || (6 + (n - $2::int) / 2) || '.example' AS domain) d`,
			[]any{day, seedMailboxes + 1, fiftyMailboxes}},
		// Every new mailbox passed a check 200 days ago; then its latest: failed on n % 8 = 0,
		// still requested on 1, none further on 2, passed the day before Day otherwise.
		{"mailbox_check old", `INSERT INTO mailbox_check (mailbox_id, requested_at, actor, checked_at, smtp_ok, imap_ok, folders_ok,
			  spf, dkim, dmarc, outcome)
			SELECT n, $1::timestamptz - interval '200 days', 'operator', $1::timestamptz - interval '200 days' + interval '1 minute',
			       true, true, true, true, true, true, 'passed'
			FROM generate_series($2::int, $3::int) n`, []any{day, seedMailboxes + 1, fiftyMailboxes}},
		{"mailbox_check latest", `INSERT INTO mailbox_check (mailbox_id, requested_at, actor, checked_at, smtp_ok, imap_ok, folders_ok,
			  spf, dkim, dmarc, fix, outcome)
			SELECT n, $1::timestamptz - interval '1 day', 'operator',
			       CASE WHEN n % 8 = 1 THEN NULL ELSE $1::timestamptz - interval '1 day' + interval '1 minute' END,
			       CASE WHEN n % 8 = 1 THEN NULL ELSE true END, CASE WHEN n % 8 = 1 THEN NULL ELSE n % 8 <> 0 END,
			       CASE WHEN n % 8 IN (0, 1) THEN NULL ELSE true END,
			       CASE WHEN n % 8 = 1 THEN NULL ELSE true END, CASE WHEN n % 8 = 1 THEN NULL ELSE true END,
			       CASE WHEN n % 8 = 1 THEN NULL ELSE true END,
			       CASE WHEN n % 8 = 0 THEN 'The IMAP login was refused.' ELSE '' END,
			       CASE n % 8 WHEN 0 THEN 'failed' WHEN 1 THEN 'requested' ELSE 'passed' END
			FROM generate_series($2::int, $3::int) n WHERE n % 8 <> 2`, []any{day, seedMailboxes + 1, fiftyMailboxes}},
		// States: every new mailbox active at creation; paused alone on n % 9 = 4 (resumed
		// once before); mail20's two paused by a domain pause; 13 and 37 retired after a pause.
		{"mailbox_state", `INSERT INTO mailbox_state (mailbox_id, state, reason, scope, actor, at)
			SELECT n, s.state, s.reason, s.scope, s.actor, $1::timestamptz - s.ago * interval '1 day'
			FROM generate_series($2::int, $3::int) n
			CROSS JOIN LATERAL (VALUES
			  (1, 'active', '', 'mailbox', 'operator', 401, true),
			  (2, 'paused', 'hard bounces', 'mailbox', 'system', 120, n % 9 = 4 OR n IN (13, 37)),
			  (3, 'active', '', 'mailbox', 'operator', 100, n % 9 = 4),
			  (4, 'paused', 'checking the account', 'mailbox', 'operator', 3, n % 9 = 4),
			  (5, 'paused', 'domain under review', 'domain', 'operator', 2, n IN (39, 40)),
			  (6, 'retired', 'account closed', 'mailbox', 'operator', 30, n IN (13, 37))
			) s(ord, state, reason, scope, actor, ago, applies)
			WHERE s.applies
			ORDER BY n, s.ord`, []any{day, seedMailboxes + 1, fiftyMailboxes}},
		// A year of daily DNS checks per new domain; today's fails on mail11 and mail22.
		{"sender_check", `INSERT INTO sender_check (domain_sha256, selector, spf, dkim, dmarc, note, checked_at)
			SELECT sha256(convert_to('mail' || d || '.example', 'UTF8')), 'lh', true,
			       NOT (i = 0 AND d IN (11, 22)) AND i % 50 <> 7, true, '',
			       $1::timestamptz - i * interval '1 day' + interval '6 hours'
			FROM generate_series(6, $2::int) d CROSS JOIN generate_series(0, 364) i
			ORDER BY i DESC, d`, []any{day, fiftyDomains}},
		// Copy k of every seed batch, id + k * maxBatch, for the copied messages to hang from.
		{"outreach_batch", `INSERT INTO outreach_batch (id, shadow, created_at, content_sha256, approved_at, approved_hash, templates_sha256)
			SELECT b.id + k * $1::bigint, b.shadow, b.created_at, b.content_sha256, b.approved_at, b.approved_hash, b.templates_sha256
			FROM outreach_batch b CROSS JOIN generate_series(1, 4) k
			WHERE b.id <= $1::bigint`, []any{maxBatch}},
		{"outreach_message", `INSERT INTO outreach_message (id, batch_id, lead_id, mailbox_id, step, channel, to_address, subject,
			  body_text, message_id, in_reply_to, slots, approved_sha256, state, due_at, sent_at, smtp_reply, cancel_reason,
			  created_at, from_name, hard_bounce)
			SELECT m.id + ` + copyNo + ` * $2::bigint, m.batch_id + ` + copyNo + ` * $1::bigint, m.lead_id, n, m.step, m.channel,
			       m.to_address, m.subject, m.body_text,
			       CASE WHEN m.message_id = '' THEN '' ELSE replace(m.message_id, '@', '-c' || n || '@') END,
			       m.in_reply_to, m.slots, m.approved_sha256, m.state, m.due_at, m.sent_at, m.smtp_reply, m.cancel_reason,
			       m.created_at, m.from_name, m.hard_bounce
			FROM generate_series($3::int, $4::int) n
			JOIN outreach_message m ON m.mailbox_id = ` + copyOf + ` AND m.id <= $2::bigint`,
			[]any{maxBatch, maxMsg, seedMailboxes + 1, fiftyMailboxes}},
	}
	start := time.Now()
	for _, s := range steps {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("fifty mailboxes: %s: %v", s.name, err)
		}
	}
	t.Logf("fifty-mailbox world added in %v (rolled back at the end)", time.Since(start).Round(time.Millisecond))
}

// fiftyConfig is the example leads.yaml with caps.start_date first-send, so each mailbox's
// ramp counts from its own first live send, as production runs.
func fiftyConfig(t *testing.T) leadconfig.Config {
	t.Helper()
	cfg, err := leadconfig.Load("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte("start_date: "+leadconfig.FirstSend+"\n"), &cfg.Caps); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestScaleFiftyMailboxes (MB-R18): with 50 mailboxes over 25 domains, mixed states and a year
// of sends on the scale database, Mailboxes, the batch sizing's RoomOver and Assign for a
// ten-lead batch (with the reads it needs), and the send run's planning step (its due rows,
// RoomToday and PlanMailboxes) each stay under 200 ms.
func TestScaleFiftyMailboxes(t *testing.T) {
	_, _, now, loc := scaleLeads(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, storetest.ScaleMigrateDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping scale test: connect as owner: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The rolled-back rows stay behind as dead tuples until vacuumed, and dead tuples
		// slow the next scale run's reads of these tables (measured: Mailboxes 47 ms on a
		// vacuumed table, over 300 ms after five runs); vacuum them away now.
		bg := context.Background()
		if err := tx.Rollback(bg); err != nil {
			t.Errorf("rollback: %v", err)
			return
		}
		if _, err := conn.Exec(bg, `VACUUM outreach_message, outreach_batch, sender_check, sender_mailbox, mailbox_state, mailbox_check`); err != nil {
			t.Errorf("vacuum after rollback: %v", err)
		}
	})
	day := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	fiftyMailboxWorld(t, ctx, tx, day)
	q := leadstore.NewQueries(tx)
	cfg := fiftyConfig(t)
	const budget = 200 * time.Millisecond

	// 1. The mailbox list, as the console, the brief and every run read it.
	var mbs []leadstore.MailboxStatus
	within(t, "Mailboxes(50)", budget, func() (err error) {
		mbs, err = q.Mailboxes(ctx, leadstore.DefaultAccountID, day)
		return err
	})
	if len(mbs) != fiftyMailboxes {
		t.Fatalf("mailboxes = %d, want %d", len(mbs), fiftyMailboxes)
	}
	domains := map[string]int{}
	states := map[string]int{}
	ready, noFirst, sentToday := 0, 0, 0
	for _, m := range mbs {
		domains[m.Domain]++
		states[m.State+"/"+m.Scope]++
		if fiftyReadyForLeads(m) {
			ready++
		}
		if m.FirstLiveSend == nil {
			noFirst++
		}
		sentToday += m.SentToday
	}
	if len(domains) != fiftyDomains {
		t.Errorf("domains = %d, want %d", len(domains), fiftyDomains)
	}
	if states["paused/mailbox"] == 0 || states["paused/domain"] != 2 || states["retired/mailbox"] != 2 {
		t.Errorf("states = %v, want mailbox pauses, two domain-paused and two retired", states)
	}
	if noFirst != 0 || sentToday == 0 {
		t.Errorf("mailboxes without a first live send = %d, sends today = %d: want a year of sends on every one", noFirst, sentToday)
	}
	t.Logf("ready for new leads: %d of %d; states %v; sent today %d", ready, len(mbs), states, sentToday)

	// 2. The batch sizing for a ten-lead batch: the reads it needs, RoomOver and Assign.
	var assigned map[int64]int64
	steps := 1 + len(cfg.Schedule.FollowupBusinessDays)
	within(t, "sizing: Mailboxes+WaitingByMailbox+RoomOver+Assign(10)", budget, func() error {
		ms, err := q.Mailboxes(ctx, leadstore.DefaultAccountID, day)
		if err != nil {
			return err
		}
		waiting, err := q.WaitingByMailbox(ctx)
		if err != nil {
			return err
		}
		loads := make([]outreach.MailboxLoad, 0, len(ms))
		for _, m := range ms {
			ml := outreach.MailboxLoad{ID: m.ID, Domain: m.Domain, SentToday: m.SentToday, Waiting: waiting[m.ID], Ready: fiftyReadyForLeads(m)}
			if m.FirstLiveSend != nil {
				fs := m.FirstLiveSend.In(loc)
				ml.FirstSend = &fs
			}
			loads = append(loads, ml)
		}
		pure := time.Now()
		room := outreach.RoomOver(cfg, loads, now.In(loc), 3, cfg.Schedule.BusinessHours)
		leads := make([]int64, 10)
		for i := range leads {
			leads[i] = int64(i + 1)
		}
		assigned = outreach.Assign(leads, room, steps)
		t.Logf("RoomOver+Assign alone: %v", time.Since(pure).Round(time.Microsecond))
		return nil
	})
	byID := map[int64]leadstore.MailboxStatus{}
	for _, m := range mbs {
		byID[m.ID] = m
	}
	if len(assigned) != 10 {
		t.Errorf("assigned = %d of 10 leads", len(assigned))
	}
	for lead, mb := range assigned {
		if !fiftyReadyForLeads(byID[mb]) {
			t.Errorf("lead %d went to mailbox %d, which is not ready for new leads", lead, mb)
		}
	}

	// 3. The send run's planning step: its due rows, each mailbox's room today, the plan. It
	// runs at 11:00 on the first business day from the seed's day (the seed's day may be a
	// weekend, when every due row is held for hours and nothing would be picked).
	runAt := time.Date(day.Year(), day.Month(), day.Day(), 11, 0, 0, 0, loc)
	for runAt.Weekday() == time.Saturday || runAt.Weekday() == time.Sunday {
		runAt = runAt.AddDate(0, 0, 1)
	}
	runDay := time.Date(runAt.Year(), runAt.Month(), runAt.Day(), 0, 0, 0, 0, loc)
	var plan []outreach.PlanItem
	var due int
	var held map[outreach.HoldReason]int
	within(t, "send planning: SendRows+Mailboxes+RoomToday+PlanMailboxes", budget, func() error {
		rows, err := q.SendRows(ctx, runAt, false)
		if err != nil {
			return err
		}
		ms, err := q.Mailboxes(ctx, leadstore.DefaultAccountID, runDay)
		if err != nil {
			return err
		}
		sendable := map[int64]bool{}
		loads := make([]outreach.MailboxLoad, 0, len(ms))
		for _, m := range ms {
			ok := m.State == leadstore.MailboxActive && m.LastCheck != nil && m.LastCheck.Outcome == leadstore.CheckPassed
			sendable[m.ID] = ok
			ml := outreach.MailboxLoad{ID: m.ID, Domain: m.Domain, SentToday: m.SentToday, Ready: ok}
			if m.FirstLiveSend != nil {
				fs := m.FirstLiveSend.In(loc)
				ml.FirstSend = &fs
			}
			loads = append(loads, ml)
		}
		items := make([]outreach.PlanItem, 0, len(rows))
		for _, r := range rows {
			if !sendable[r.MailboxID] {
				continue
			}
			_, domain, _ := strings.Cut(r.ToAddress, "@")
			items = append(items, outreach.PlanItem{
				MessageID: r.ID, BatchID: r.BatchID, LeadID: r.LeadID, Step: r.Step, Domain: domain, Loc: loc,
				DueAt: *r.DueAt, MailboxID: r.MailboxID,
			})
		}
		due = len(rows)
		pure := time.Now()
		plan, held = outreach.PlanMailboxes(items, outreach.RoomToday(cfg, loads, runAt), runAt, cfg.Schedule.BusinessHours)
		t.Logf("RoomToday+PlanMailboxes alone over %d items: %v", len(items), time.Since(pure).Round(time.Microsecond))
		return nil
	})
	if due == 0 || len(plan) == 0 {
		t.Errorf("due rows %d, planned %d: want a run that picks some", due, len(plan))
	}
	for _, it := range plan {
		if m := byID[it.MailboxID]; m.State != leadstore.MailboxActive || m.LastCheck == nil || m.LastCheck.Outcome != leadstore.CheckPassed {
			t.Errorf("message %d planned from mailbox %d, which may not send", it.MessageID, it.MailboxID)
		}
	}
	t.Logf("due rows %d, planned %d, held %v (run at %s)", due, len(plan), held, runAt.Format(time.RFC1123))
}

// fiftyReadyForLeads is cmd's readyForLeads: active, latest check passed, DNS passed today.
func fiftyReadyForLeads(m leadstore.MailboxStatus) bool {
	return m.State == leadstore.MailboxActive && m.LastCheck != nil && m.LastCheck.Outcome == leadstore.CheckPassed && m.DNSPassedToday
}
