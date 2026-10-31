//go:build integration

package leadstore_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// statusNow is the fixed "now" of these tests: a Saturday afternoon, UTC.
var statusNow = time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)

// ownerRun runs one statement as the owner, for setup the app may not do, whatever rows it
// touches (ownerExec wants exactly one).
func (f outreachFixture) ownerRun(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.owner.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("owner exec %q: %v", sql, err)
	}
}

// msgID returns the id of lead's message at step in batch.
func (f outreachFixture) msgID(t *testing.T, batchID, leadID int64, step int) int64 {
	t.Helper()
	var id int64
	if err := f.owner.QueryRow(context.Background(),
		`SELECT id FROM outreach_message WHERE batch_id = $1 AND lead_id = $2 AND step = $3`, batchID, leadID, step).Scan(&id); err != nil {
		t.Fatalf("message %d/%d/%d: %v", batchID, leadID, step, err)
	}
	return id
}

// sent marks message id as state at at.
func (f outreachFixture) sent(t *testing.T, id int64, state string, at time.Time) {
	t.Helper()
	f.ownerRun(t, `UPDATE outreach_message SET state = $2, sent_at = $3 WHERE id = $1`, id, state, at)
}

// TestServiceStatus: no row is ErrServiceNotFound; a success and a failure each keep the
// other's columns; an older reading or failure never replaces a newer one; a provider outside
// the closed set and a reading that is not a JSON object are refused; the error is one line.
func TestServiceStatus(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	t0 := statusNow

	if _, err := q.ServiceStatus(ctx, leadstore.ServiceReoon); !errors.Is(err, leadstore.ErrServiceNotFound) {
		t.Fatalf("cold start: %v, want ErrServiceNotFound", err)
	}

	if err := q.RecordServiceOK(ctx, leadstore.ServiceReoon, []byte(`{"daily": 40}`), t0); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordServiceError(ctx, leadstore.ServiceReoon, "\n  timeout reading balance  \nsecond line", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	s, err := q.ServiceStatus(ctx, leadstore.ServiceReoon)
	if err != nil {
		t.Fatal(err)
	}
	if string(s.Data) != `{"daily": 40}` || s.FetchedAt == nil || !s.FetchedAt.Equal(t0) ||
		s.Error != "timeout reading balance" || s.ErrorAt == nil || !s.ErrorAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("after ok then error: %+v", s)
	}

	// A later success keeps the error; an older success and an older error are ignored.
	if err := q.RecordServiceOK(ctx, leadstore.ServiceReoon, []byte(`{"daily": 39}`), t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordServiceOK(ctx, leadstore.ServiceReoon, []byte(`{"daily": 1}`), t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordServiceError(ctx, leadstore.ServiceReoon, "stale", t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	s, _ = q.ServiceStatus(ctx, leadstore.ServiceReoon)
	if string(s.Data) != `{"daily": 39}` || !s.FetchedAt.Equal(t0.Add(2*time.Minute)) || s.Error != "timeout reading balance" {
		t.Errorf("after a later ok and two stale writes: %+v", s)
	}

	// An error first: no reading yet. A blank message is "failed"; a long one is cut.
	if err := q.RecordServiceError(ctx, leadstore.ServiceClaude, "  ", t0); err != nil {
		t.Fatal(err)
	}
	c, err := q.ServiceStatus(ctx, leadstore.ServiceClaude)
	if err != nil || c.Data != nil || c.FetchedAt != nil || c.Error != "failed" {
		t.Errorf("claude error only: %+v %v", c, err)
	}
	if err := q.RecordServiceError(ctx, leadstore.ServiceClaude, strings.Repeat("é", 1000), t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if c, _ = q.ServiceStatus(ctx, leadstore.ServiceClaude); len([]rune(c.Error)) != leadstore.MaxServiceError {
		t.Errorf("long error kept %d characters, want %d", len([]rune(c.Error)), leadstore.MaxServiceError)
	}

	// Refused: another provider (by the table), a reading that is not an object.
	if err := q.RecordServiceOK(ctx, "zerobounce", []byte(`{}`), t0); sqlState(err) != "23514" {
		t.Errorf("unknown provider ok: %v, want a check violation", err)
	}
	if err := q.RecordServiceError(ctx, "zerobounce", "x", t0); sqlState(err) != "23514" {
		t.Errorf("unknown provider error: %v, want a check violation", err)
	}
	for _, data := range []string{``, `[1]`, `null`, `{bad`} {
		if err := q.RecordServiceOK(ctx, leadstore.ServiceClaude, []byte(data), t0); !errors.Is(err, leadstore.ErrBadServiceStatus) {
			t.Errorf("data %q: %v, want ErrBadServiceStatus", data, err)
		}
	}
	// The app upserts (UPDATE kept) but never deletes.
	owner := storetest.OwnerConn(t)
	for _, table := range []string{"service_status", "claude_topup"} {
		if !appHas(t, owner, table, "INSERT") || appHas(t, owner, table, "DELETE") || appHas(t, owner, table, "TRUNCATE") {
			t.Errorf("%s: the app wants INSERT and no DELETE or TRUNCATE", table)
		}
	}
	if !appHas(t, owner, "service_status", "UPDATE") {
		t.Error("service_status: the app lacks the UPDATE an upsert needs")
	}
	app := storetest.AppConn(t)
	for _, stmt := range []string{
		`INSERT INTO service_status (provider) VALUES ('other')`,
		`UPDATE service_status SET data = '[1]', fetched_at = now()`,
		`UPDATE service_status SET data = '{}', fetched_at = NULL`,
		`UPDATE service_status SET error = 'two' || chr(10) || 'lines'`,
		`UPDATE service_status SET error = 'x', error_at = NULL`,
	} {
		if _, err := app.Exec(ctx, stmt); sqlState(err) != "23514" {
			t.Errorf("%s: %v, want a check violation", stmt, err)
		}
	}
}

// TestClaudeTopup: none is ErrTopupNotFound; the latest is by when it was made, then id; a
// top-up of 0 or less is refused.
func TestClaudeTopup(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	if _, err := q.LatestClaudeTopup(ctx); !errors.Is(err, leadstore.ErrTopupNotFound) {
		t.Fatalf("cold start: %v, want ErrTopupNotFound", err)
	}
	for _, tp := range []struct {
		cents int64
		at    time.Time
	}{{2000, statusNow}, {500, statusNow.Add(-24 * time.Hour)}, {1000, statusNow}} {
		if err := q.AddClaudeTopup(ctx, tp.cents, tp.at); err != nil {
			t.Fatal(err)
		}
	}
	got, err := q.LatestClaudeTopup(ctx)
	if err != nil || got.AmountCents != 1000 || !got.At.Equal(statusNow) || got.ID != 3 {
		t.Errorf("latest: %+v %v; want the second top-up at now (id 3)", got, err)
	}
	for _, cents := range []int64{0, -5} {
		if err := q.AddClaudeTopup(ctx, cents, statusNow); sqlState(err) != "23514" {
			t.Errorf("%d cents: %v, want a check violation", cents, err)
		}
	}
}

// TestMailboxHealth: no mailbox is no rows; each mailbox that is not retired comes with its
// state, latest check, latest sender check, today's and the week's live sends, the week's hard
// bounces and its latest sync; shadow sends, sends before the week and after now never count.
func TestMailboxHealth(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

	if rows, err := q.MailboxHealth(ctx, day, time.UTC, statusNow); err != nil || len(rows) != 0 {
		t.Fatalf("cold start: %+v %v", rows, err)
	}

	a := f.mailbox(t)
	b := leadstoretest.SeedMailbox(t, q, "second@fixture.localhost").ID
	gone := leadstoretest.SeedMailbox(t, q, "gone@fixture.localhost").ID
	if err := q.SetMailboxState(ctx, gone, leadstore.MailboxRetired, "done with it", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if err := q.SetMailboxState(ctx, b, leadstore.MailboxPaused, "3 hard bounces", leadstore.ScopeMailbox, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	// b's latest check failed on IMAP.
	if _, err := q.RequestCheck(ctx, b, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	checks, err := q.RequestedChecks(ctx)
	if err != nil || len(checks) != 1 {
		t.Fatalf("requested checks: %+v %v", checks, err)
	}
	yes, no := true, false
	if err := q.FinishCheck(ctx, checks[0].ID, leadstore.MailboxCheckResult{SMTPOK: &yes, IMAPOK: &no, Outcome: leadstore.CheckFailed,
		Fix: "check the IMAP login"}, statusNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// a's host: an older pass, then a newer DMARC miss (the latest wins).
	host := sha256.Sum256([]byte("fixture.localhost"))
	if err := q.RecordSenderCheckSelector(ctx, host[:], "lh", mailbox.DNSResult{SPF: true, DKIM: true, DMARC: true}, statusNow.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordSenderCheckSelector(ctx, host[:], "lh", mailbox.DNSResult{SPF: true, DKIM: true, Note: "no DMARC record"}, statusNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := q.AdvanceCursorFor(ctx, a, "INBOX", 1, 10, statusNow.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := q.AdvanceCursorFor(ctx, a, "Junk", 1, 3, statusNow.Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	var leads []int64
	for _, d := range []string{"h1.localhost", "h2.localhost", "h3.localhost", "h4.localhost"} {
		leads = append(leads, f.siteLead(t, d, nil, 7, "info@"+d))
	}
	shadow, _ := f.batch(t, true, leads[0]) // a shadow batch first: it leaves the lead new
	live, _ := f.batch(t, false, leads...)
	// a: two today (one maybe_sent), one in the week that hard-bounced, one before the week,
	// one stamped after now; a shadow send today.
	f.sent(t, f.msgID(t, live, leads[0], 0), "sent", day.Add(10*time.Hour))
	f.sent(t, f.msgID(t, live, leads[0], 1), "maybe_sent", day.Add(11*time.Hour))
	f.sent(t, f.msgID(t, live, leads[1], 0), "sent", day.AddDate(0, 0, -4))
	f.ownerRun(t, `UPDATE outreach_message SET hard_bounce = true WHERE id = $1`, f.msgID(t, live, leads[1], 0))
	f.sent(t, f.msgID(t, live, leads[1], 1), "sent", day.AddDate(0, 0, -6).Add(-time.Hour))
	f.sent(t, f.msgID(t, live, leads[2], 0), "sent", statusNow.Add(time.Hour))
	f.sent(t, f.msgID(t, shadow, leads[0], 0), "shadow_sent", day.Add(time.Hour))
	// b: one in the week, from b.
	f.ownerRun(t, `UPDATE outreach_message SET mailbox_id = $2 WHERE id = $1`, f.msgID(t, live, leads[3], 0), b)
	f.sent(t, f.msgID(t, live, leads[3], 0), "sent", day.AddDate(0, 0, -6))

	rows, err := q.MailboxHealth(ctx, day, time.UTC, statusNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != a || rows[1].ID != b {
		t.Fatalf("rows: %+v; want a and b, not the retired one", rows)
	}
	ra, rb := rows[0], rows[1]
	if ra.State != leadstore.MailboxActive || ra.Reason != "" || ra.StateAt != nil || ra.Address != "outreach@fixture.localhost" {
		t.Errorf("a's state: %+v", ra)
	}
	if ra.LastCheck == nil || ra.LastCheck.Outcome != leadstore.CheckPassed {
		t.Errorf("a's check: %+v", ra.LastCheck)
	}
	if sc := ra.SenderCheck; sc == nil || !sc.SPF || !sc.DKIM || sc.DMARC || sc.Note != "no DMARC record" {
		t.Errorf("a's sender check: %+v", sc)
	}
	if ra.SentToday != 2 || ra.Sent7d != 3 || ra.Bounces7d != 1 {
		t.Errorf("a's sends: today %d week %d bounces %d; want 2, 3, 1", ra.SentToday, ra.Sent7d, ra.Bounces7d)
	}
	if ra.SyncedAt == nil || !ra.SyncedAt.Equal(statusNow.Add(-20*time.Minute)) {
		t.Errorf("a's sync: %v", ra.SyncedAt)
	}
	if rb.State != leadstore.MailboxPaused || rb.Reason != "3 hard bounces" || rb.StateAt == nil {
		t.Errorf("b's state: %+v", rb)
	}
	if c := rb.LastCheck; c == nil || c.Outcome != leadstore.CheckFailed || c.IMAPOK == nil || *c.IMAPOK || c.FoldersOK != nil || c.Fix == "" {
		t.Errorf("b's check: %+v", c)
	}
	// b shares a's host and selector, so a's sender check is b's too.
	if rb.SenderCheck == nil || *rb.SenderCheck != *ra.SenderCheck || rb.SyncedAt != nil || rb.SentToday != 0 || rb.Sent7d != 1 || rb.Bounces7d != 0 {
		t.Errorf("b: %+v", rb)
	}
}

// TestEmailChecksOn: no checks is zeros; a span's checks are counted by outcome, from
// inclusive and to exclusive.
func TestEmailChecksOn(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	day := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	got, err := q.EmailChecksOn(ctx, day, day.AddDate(0, 0, 1))
	if err != nil || got.Total != 0 || got.ByStatus == nil || len(got.ByStatus) != 0 {
		t.Fatalf("cold start: %+v %v", got, err)
	}
	for i, c := range []struct {
		status string
		at     time.Time
	}{
		{"safe", day}, {"role_account", day.Add(time.Hour)}, {"invalid", day.Add(2 * time.Hour)},
		{"disposable", day.Add(3 * time.Hour)}, {"catch_all", day.Add(4 * time.Hour)}, {"unknown", day.Add(5 * time.Hour)},
		{"safe", day.Add(-time.Second)}, {"invalid", day.AddDate(0, 0, 1)},
	} {
		if _, err := q.AddEmailCheck(ctx, leadstore.EmailCheck{Address: "a" + string(rune('a'+i)) + "@x.localhost",
			Provider: "reoon", Status: c.status, CheckedAt: c.at}); err != nil {
			t.Fatal(err)
		}
	}
	got, err = q.EmailChecksOn(ctx, day, day.AddDate(0, 0, 1))
	if err != nil || got.Total != 6 || got.Valid != 2 || got.Invalid != 2 || got.CatchAll != 1 || got.Other != 1 ||
		got.ByStatus["safe"] != 1 || got.ByStatus["unknown"] != 1 {
		t.Errorf("the day: %+v %v", got, err)
	}
}

// TestNeedsMe: no lead is none; a replied lead and a lead connected on a call wait on the
// operator, newest first; a replied lead with an approved step still ahead, a selftest lead
// and a closed lead do not; the total counts past the limit.
func TestNeedsMe(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	rows, total, err := q.NeedsMe(ctx, 10)
	if err != nil || len(rows) != 0 || total != 0 {
		t.Fatalf("cold start: %+v %d %v", rows, total, err)
	}

	replied := f.siteLead(t, "replied.localhost", nil, 7, "info@replied.localhost")
	waiting := f.siteLead(t, "waiting.localhost", nil, 7, "info@waiting.localhost")
	connected := f.siteLead(t, "connected.localhost", nil, 7, "info@connected.localhost")
	closed := f.siteLead(t, "closed.localhost", nil, 7, "info@closed.localhost")
	self := f.siteLead(t, "self.localhost", nil, 7, "info@self.localhost")
	f.ownerRun(t, `UPDATE lead SET kind = 'selftest', probe_id = NULL WHERE id = $1`, self)
	live, _ := f.batch(t, false, replied, waiting, connected, closed, self)
	// Every draft cancelled but waiting's step 1, which is approved for later.
	f.ownerRun(t, `UPDATE outreach_message SET state = 'cancelled' WHERE batch_id = $1`, live)
	f.ownerRun(t, `UPDATE outreach_message SET state = 'approved', due_at = $2 WHERE id = $1`,
		f.msgID(t, live, waiting, 1), statusNow.Add(48*time.Hour))
	for id, at := range map[int64]time.Time{replied: statusNow.Add(-2 * time.Hour), waiting: statusNow.Add(-time.Hour),
		closed: statusNow, self: statusNow} {
		f.ownerRun(t, `UPDATE lead SET state = 'replied', state_at = $2 WHERE id = $1`, id, at)
	}
	f.ownerRun(t, `UPDATE lead SET state = 'closed' WHERE id = $1`, closed)
	// connected: a call step whose first outcome was no_answer and latest connected.
	f.ownerRun(t, `UPDATE lead SET state = 'contacted' WHERE id = $1`, connected)
	var call int64
	if err := f.owner.QueryRow(ctx, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, state, mailbox_id)
		VALUES ($1, $2, 2, 'phone', '', '{}', 'done', $3) RETURNING id`, live, connected, f.mailbox(t)).Scan(&call); err != nil {
		t.Fatal(err)
	}
	f.ownerRun(t, `INSERT INTO call_outcome (message_id, outcome, actor, at) VALUES ($1, 'no_answer', 'operator', $2)`, call, statusNow.Add(-3*time.Hour))
	f.ownerRun(t, `INSERT INTO call_outcome (message_id, outcome, actor, at) VALUES ($1, 'connected', 'operator', $2)`, call, statusNow.Add(-90*time.Minute))

	rows, total, err = q.NeedsMe(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("rows %+v total %d; want connected then replied", rows, total)
	}
	if rows[0].LeadID != connected || rows[0].What != leadstore.NeedsConnected || !rows[0].At.Equal(statusNow.Add(-90*time.Minute)) ||
		rows[0].Domain != "connected.localhost" || rows[0].Name == "" {
		t.Errorf("first: %+v", rows[0])
	}
	if rows[1].LeadID != replied || rows[1].What != leadstore.NeedsReplied || !rows[1].At.Equal(statusNow.Add(-2*time.Hour)) {
		t.Errorf("second: %+v", rows[1])
	}
	if rows, total, err = q.NeedsMe(ctx, 1); err != nil || len(rows) != 1 || total != 2 || rows[0].LeadID != connected {
		t.Errorf("limit 1: %+v %d %v", rows, total, err)
	}

	// Once waiting's step goes, it waits on the operator too.
	f.sent(t, f.msgID(t, live, waiting, 1), "sent", statusNow)
	if rows, total, err = q.NeedsMe(ctx, 0); err != nil || total != 3 || len(rows) != 1 || rows[0].LeadID != waiting {
		t.Errorf("after waiting's step went (limit 0 reads as 1): %+v %d %v", rows, total, err)
	}
}

// TestCampaignFunnel: nothing is no rows; each campaign counts its own live email sends,
// replies and consents since the time given; a campaign with nothing is left out.
func TestCampaignFunnel(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	since := statusNow.AddDate(0, 0, -30)
	if rows, err := q.CampaignFunnel(ctx, since); err != nil || len(rows) != 0 {
		t.Fatalf("cold start: %+v %v", rows, err)
	}

	f.ownerRun(t, `INSERT INTO campaign (id, account_id, slug, name) VALUES (2, 1, 'cafes', 'Cafes'), (3, 1, 'quiet', 'Quiet')`)
	one := f.siteLead(t, "one.localhost", nil, 7, "info@one.localhost")
	two := f.siteLead(t, "two.localhost", nil, 7, "info@two.localhost")
	three := f.siteLead(t, "three.localhost", nil, 7, "info@three.localhost")
	sh, _ := f.batch(t, true, one) // a shadow batch first: it leaves the lead new
	b1, _ := f.batch(t, false, one)
	b2, _ := f.batch(t, false, two, three)
	f.ownerRun(t, `UPDATE outreach_batch SET campaign_id = 2 WHERE id = $1`, b2)
	f.ownerRun(t, `UPDATE lead SET campaign_id = 2 WHERE id IN ($1, $2)`, two, three)

	// Campaign 1: two sends since, one before; a shadow send; one reply.
	f.sent(t, f.msgID(t, b1, one, 0), "sent", since.Add(time.Hour))
	f.sent(t, f.msgID(t, b1, one, 1), "maybe_sent", since.Add(2*time.Hour))
	f.sent(t, f.msgID(t, sh, one, 0), "shadow_sent", since.Add(time.Hour))
	f.ownerRun(t, `INSERT INTO lead_event (lead_id, state, actor, at) VALUES ($1, 'replied', 'system', $2)`, one, since.Add(3*time.Hour))
	// Campaign 2: one send since, one before; a reply before since; a consent on a call.
	f.sent(t, f.msgID(t, b2, two, 0), "sent", since.Add(time.Hour))
	f.sent(t, f.msgID(t, b2, three, 0), "sent", since.Add(-time.Hour))
	f.ownerRun(t, `INSERT INTO lead_event (lead_id, state, actor, at) VALUES ($1, 'replied', 'system', $2)`, three, since.Add(-time.Minute))
	var call, outcome, org int64
	if err := f.owner.QueryRow(ctx, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, state, mailbox_id)
		VALUES ($1, $2, 2, 'phone', '', '{}', 'done', $3) RETURNING id`, b2, two, f.mailbox(t)).Scan(&call); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.QueryRow(ctx, `INSERT INTO call_outcome (message_id, outcome, email_consent, consent_address, actor, at)
		VALUES ($1, 'connected', true, 'owner@two.localhost', 'operator', $2) RETURNING id`, call, since.Add(time.Hour)).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.QueryRow(ctx, `SELECT org_id FROM lead WHERE id = $1`, two).Scan(&org); err != nil {
		t.Fatal(err)
	}
	f.ownerRun(t, `INSERT INTO email_consent (org_id, campaign_id, address, call_outcome_id, wording, actor, at)
		VALUES ($1, 2, 'owner@two.localhost', $2, 'You agree to get our email.', 'operator', $3)`, org, outcome, since.Add(time.Hour))

	rows, err := q.CampaignFunnel(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %+v; want campaigns 1 and 2", rows)
	}
	if r := rows[0]; r.CampaignID != 1 || r.Slug != "drupal-eol" || r.Sent != 2 || r.Replied != 1 || r.Positive != 0 {
		t.Errorf("campaign 1: %+v", r)
	}
	if r := rows[1]; r.CampaignID != 2 || r.Name != "Cafes" || r.Sent != 1 || r.Replied != 0 || r.Positive != 1 {
		t.Errorf("campaign 2: %+v", r)
	}
}
