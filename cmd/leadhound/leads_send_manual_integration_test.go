//go:build integration

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadbrief"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
	"github.com/gremy/webbership-leadhound/internal/verify"
)

// The manual send's run (Checkpoint 4, amendments 1, 2, 5, 8, 9, 10, 11): leads send
// --from-request claims the console's send_request row once, checks it again, sends that
// batch alone with its overrides in the run's planning, and leaves a send_override row for
// each message sent past a warning. Every hard stop stays absolute.

// manualOperator is the operator a test's send_request rows name.
const manualOperator = "gremy"

// manualBatch drafts leads (lead id to domain) as one manual batch from mb and approves it,
// every first email due now, as the console's confirm does; it returns the batch id and the
// hash prefix the confirm page showed.
func (w *sendWorld) manualBatch(t *testing.T, mb leadstore.Mailbox, shadow bool, leads map[int64]string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	var drafts []leadstore.DraftRow
	for _, id := range slices.Sorted(maps.Keys(leads)) {
		domain := leads[id]
		first := ""
		for step := range 3 {
			d := leadstore.DraftRow{LeadID: id, Step: step, Channel: "email", To: "info@" + domain,
				Subject: fmt.Sprintf("Your Drupal site, note %d", step+1), Body: fmt.Sprintf("Hello %s, note %d.\n", domain, step+1),
				MessageID: fmt.Sprintf("m%dl%ds%d@%s", len(drafts), id, step, testSenderDomain)}
			if step == 0 {
				d.MailboxID, d.FromName = mb.ID, w.fromName
				first = d.MessageID
			} else {
				d.InReplyTo = first
			}
			drafts = append(drafts, d)
		}
	}
	tx, err := storetest.AppConn(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := leadstore.NewQueries(tx)
	batchID, sum, err := q.CreateBatch(ctx, shadow, drafts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.MarkBatchManual(ctx, batchID); err != nil {
		t.Fatal(err)
	}
	prefix := hex.EncodeToString(sum)[:16]
	now := w.clock.now()
	if _, err := q.ApproveBatch(ctx, batchID, prefix, nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return now
		}
		return time.Time{}
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return batchID, prefix
}

// confirm writes what the console's confirm writes for batchID: the send_request row (codes,
// hash prefix, operator) and the request file's JSON line naming the batch.
func (w *sendWorld) confirm(t *testing.T, batchID int64, prefix string, codes ...string) int64 {
	t.Helper()
	return w.confirmShown(t, batchID, prefix, nil, codes...)
}

// confirmShown is confirm with the warnings the confirm page showed (send_request.shown).
func (w *sendWorld) confirmShown(t *testing.T, batchID int64, prefix string, shown json.RawMessage, codes ...string) int64 {
	t.Helper()
	id, err := w.lq.InsertSendRequest(context.Background(), leadstore.SendRequest{BatchID: batchID, Codes: codes, HashPrefix: prefix,
		Operator: manualOperator, Shown: shown})
	if err != nil {
		t.Fatal(err)
	}
	now := w.clock.now()
	writeSendNowFile(t, w.dir, fmt.Sprintf(`{"batch_id":%d,"at":%q}`+"\n", batchID, now.UTC().Format(time.RFC3339Nano)), now)
	return id
}

// checked stores a fresh email check of status for info@<domain> of each domain.
func (w *sendWorld) checked(t *testing.T, status string, domains ...string) {
	t.Helper()
	for _, d := range domains {
		if _, err := w.lq.AddEmailCheck(context.Background(), leadstore.EmailCheck{Address: "info@" + d, Provider: verify.Provider,
			Status: status, CheckedAt: w.clock.now()}); err != nil {
			t.Fatal(err)
		}
	}
}

// overrides is batchID's send_override rows.
func (w *sendWorld) overrides(t *testing.T, batchID int64) []leadstore.SendOverride {
	t.Helper()
	rows, err := w.lq.SendOverrides(context.Background(), batchID)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// sendFromRequest runs the send-now unit's command.
func (w *sendWorld) sendFromRequest(t *testing.T) (int, string) {
	t.Helper()
	return w.run(t, "leads", "send", "--from-request")
}

// sentToday marks n messages of a fresh batch from mb as live sent today, so mb's sends today
// count n (the ramp and the domain's limit read them).
func (w *sendWorld) sentToday(t *testing.T, mb leadstore.Mailbox, n int, prefix string) {
	t.Helper()
	leads := map[int64]string{}
	for i := range (n + 2) / 3 {
		d := fmt.Sprintf("%s%d.example", prefix, i)
		leads[w.lead(t, d)] = d
	}
	w.batchFrom(t, mb, false, leads)
	ids := []int64{}
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT m.id FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 WHERE o.domain LIKE $1 ORDER BY m.id LIMIT $2`, prefix+"%", n)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != n {
		t.Fatalf("seeded %d messages, want %d", len(ids), n)
	}
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`UPDATE outreach_message SET state = 'sent', sent_at = $1, mailbox_id = $2 WHERE id = ANY($3)`,
		w.clock.now(), mb.ID, ids); err != nil {
		t.Fatal(err)
	}
	// The rest of the seeding batch never sends in the test.
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`UPDATE outreach_message m SET state = 'cancelled' FROM lead l, org o
		 WHERE l.id = m.lead_id AND o.id = l.org_id AND o.domain LIKE $1 AND m.state IN ('approved', 'draft')`, prefix+"%"); err != nil {
		t.Fatal(err)
	}
}

// manualWorld is a live world at 10:00 London on Tuesday 27 Oct 2026 (inside the window) with
// a cap of capPerDay and one lead per domain, every address checked safe.
func manualWorld(t *testing.T, capPerDay int, domains ...string) (*sendWorld, map[int64]string) {
	t.Helper()
	w := newSendWorld(t, true, capPerDay)
	leads := map[int64]string{}
	for _, d := range domains {
		leads[w.lead(t, d)] = d
	}
	w.checked(t, verify.StatusSafe, domains...)
	return w, leads
}

// TestManualSendLegacyRequest: an old request file (a time) and an empty one still run today's
// Send now: the window skipped, every limit kept, the run noted send now.
func TestManualSendLegacyRequest(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"time", "2026-10-27T21:00:00Z\n"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := sendNowWorld(t, 2, time.Date(2026, 10, 27, 21, 0, 0, 0, time.UTC))
			writeSendNowFile(t, w.dir, tc.text, w.clock.now())
			code, out := w.sendFromRequest(t)
			if code != 0 {
				t.Fatalf("exited %d:\n%s", code, out)
			}
			if n := w.smtp.count(); n != 2 {
				t.Fatalf("sent %d, want 2 (the cap):\n%s", n, out)
			}
			wantLines(t, tc.name, out, "Send now: this run skips the sending window only", "Held for a later run: today's cap 1.")
			if note := w.lastNote(t, commandLeadsSend); !leadbrief.NoteWords(note)[noteSendNow] || strings.Contains(note, noteOverride) {
				t.Errorf("run note %q", note)
			}
			w.sweep(t)
		})
	}
}

// TestManualSendRequestRefused: a malformed request file, batch_id 0 or no file sends nothing,
// says why, exits non-zero and notes the refusal; nothing is claimed.
func TestManualSendRequestRefused(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"malformed", `{"batch_id":`},
		{"batch 0", `{"batch_id":0,"at":"2026-10-27T10:00:00Z"}`},
		{"codes in the file", `{"batch_id":1,"codes":["ramp"]}`},
		{"no file", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, leads := manualWorld(t, 20, "alpha.example")
			id, prefix := w.manualBatch(t, w.mailbox, false, leads)
			w.confirm(t, id, prefix, allCodes()...)
			if tc.name == "no file" {
				if err := removeSendNowFile(w.dir); err != nil {
					t.Fatal(err)
				}
			} else {
				writeSendNowFile(t, w.dir, tc.text, w.clock.now())
			}
			code, out := w.sendFromRequest(t)
			if code == 0 {
				t.Errorf("a refused request exited 0:\n%s", out)
			}
			if n := w.smtp.count(); n != 0 {
				t.Fatalf("a refused request sent %d:\n%s", n, out)
			}
			wantLines(t, tc.name, out, "the Send now request is refused")
			if note := w.lastNote(t, commandLeadsSend); !leadbrief.NoteWords(note)[noteRequestRefused] {
				t.Errorf("run note %q", note)
			}
			if n := w.count(t, `SELECT count(*) FROM send_request WHERE used_at IS NOT NULL`); n != 0 {
				t.Errorf("%d requests claimed", n)
			}
			if n := len(w.overrides(t, id)); n != 0 {
				t.Errorf("%d override rows", n)
			}
		})
	}
}

// TestManualSendClaim: the run sends its batch alone (another approved batch due now waits for
// the hourly send), claims its request once, refuses a request past 15 minutes, a wrong hash, a
// batch that is not manual or not approved.
func TestManualSendClaim(t *testing.T) {
	ctx := context.Background()
	t.Run("once, its batch alone", func(t *testing.T) {
		w, leads := manualWorld(t, 20, "alpha.example", "bravo.example")
		other := w.lead(t, "charlie.example")
		w.checked(t, verify.StatusSafe, "charlie.example")
		w.batch(t, false, map[int64]string{other: "charlie.example"})
		id, prefix := w.manualBatch(t, w.mailbox, false, leads)
		w.confirm(t, id, prefix)
		out := w.mustRun(t, "leads", "send", "--from-request")
		got := w.smtp.recipients()
		slices.Sort(got)
		if !slices.Equal(got, []string{"info@alpha.example", "info@bravo.example"}) {
			t.Fatalf("sent %v, want the manual batch's two:\n%s", got, out)
		}
		wantLines(t, "manual", out, fmt.Sprintf("Send now: batch %d alone, no warning is overridden", id), "Sent 2: ")
		note := w.lastNote(t, commandLeadsSend)
		if !leadbrief.NoteWords(note)[noteSendNow] || strings.Contains(note, noteOverride) {
			t.Errorf("run note %q", note)
		}
		if n := len(w.overrides(t, id)); n != 0 {
			t.Errorf("%d override rows for a send with no codes", n)
		}
		if s := w.msgState(t, "charlie.example", 0); s != leadstore.MsgApproved {
			t.Errorf("the other batch's email is %s, want approved for the hourly send", s)
		}
		// Claimed once: the same request again sends nothing.
		code, out := w.sendFromRequest(t)
		if code != 0 || w.smtp.count() != 2 {
			t.Fatalf("second run exited %d, %d sent:\n%s", code, w.smtp.count(), out)
		}
		wantLines(t, "second", out, fmt.Sprintf("No sends: batch %d has no confirmed Send now waiting", id))
		if note := w.lastNote(t, commandLeadsSend); !leadbrief.NoteWords(note)[noteNoRequest] {
			t.Errorf("second run note %q", note)
		}
		w.sweep(t)
	})
	t.Run("older than 15 minutes", func(t *testing.T) {
		w, leads := manualWorld(t, 20, "alpha.example")
		id, prefix := w.manualBatch(t, w.mailbox, false, leads)
		req := w.confirm(t, id, prefix)
		if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE send_request SET created_at = $1 WHERE id = $2`,
			w.clock.now().Add(-16*time.Minute), req); err != nil {
			t.Fatal(err)
		}
		code, out := w.sendFromRequest(t)
		if code != 0 || w.smtp.count() != 0 {
			t.Fatalf("an old request exited %d, sent %d:\n%s", code, w.smtp.count(), out)
		}
		wantLines(t, "old", out, "has no confirmed Send now waiting")
	})
	refused := []struct {
		name  string
		setup func(t *testing.T, w *sendWorld, id int64, prefix string) string // the prefix to confirm
	}{
		{"wrong hash", func(t *testing.T, w *sendWorld, id int64, prefix string) string {
			return strings.Repeat("0", len(prefix))
		}},
		{"short hash", func(t *testing.T, w *sendWorld, id int64, prefix string) string { return prefix[:8] }},
		{"not manual", func(t *testing.T, w *sendWorld, id int64, prefix string) string {
			if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE outreach_batch SET manual = false WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
			return prefix
		}},
		{"not approved", func(t *testing.T, w *sendWorld, id int64, prefix string) string {
			if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE outreach_batch SET approved_at = NULL WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
			return prefix
		}},
		{"discarded", func(t *testing.T, w *sendWorld, id int64, prefix string) string {
			if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE outreach_batch SET discarded_at = now() WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
			return prefix
		}},
		{"edited after confirm", func(t *testing.T, w *sendWorld, id int64, prefix string) string {
			if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE outreach_message SET body_text = body_text || 'x' WHERE batch_id = $1`, id); err != nil {
				t.Fatal(err)
			}
			return prefix
		}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			w, leads := manualWorld(t, 20, "alpha.example")
			id, prefix := w.manualBatch(t, w.mailbox, false, leads)
			w.confirm(t, id, tc.setup(t, w, id, prefix), allCodes()...)
			code, out := w.sendFromRequest(t)
			if code == 0 || w.smtp.count() != 0 {
				t.Fatalf("exited %d, sent %d:\n%s", code, w.smtp.count(), out)
			}
			wantLines(t, tc.name, out, "the Send now request is refused")
			if n := len(w.overrides(t, id)); n != 0 {
				t.Errorf("%d override rows", n)
			}
			// Checked before it is claimed: a refused request is not used up.
			if n := w.count(t, `SELECT count(*) FROM send_request WHERE used_at IS NOT NULL`); n != 0 {
				t.Errorf("a refused request was claimed (%d used)", n)
			}
		})
	}
}

// TestManualSendHardStops (the invariant sweep): every hard stop the run enforces holds with
// all six codes overridden: the message is never sent and no send_override row is written.
func TestManualSendHardStops(t *testing.T) {
	ctx := context.Background()
	owner := func(t *testing.T, sql string, args ...any) {
		t.Helper()
		if _, err := storetest.OwnerConn(t).Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, w *sendWorld, id int64)
	}{
		{"control: no stop", nil},
		{"suppressed", func(t *testing.T, w *sendWorld, _ int64) {
			if _, err := w.lq.Suppress(ctx, 0, leadstore.SuppressAddress, "info@alpha.example", leadstore.ReasonOptOut, "test"); err != nil {
				t.Fatal(err)
			}
		}},
		{"email check invalid", func(t *testing.T, w *sendWorld, _ int64) {
			w.clock.set(w.clock.now().Add(time.Minute))
			w.checked(t, verify.StatusInvalid, "alpha.example")
		}},
		{"email check invalid, check switched off", func(t *testing.T, w *sendWorld, _ int64) {
			w.clock.set(w.clock.now().Add(time.Minute))
			w.checked(t, verify.StatusInvalid, "alpha.example")
			if _, err := w.lq.SetJobSwitch(ctx, 0, leadstore.JobVerify, false, "test", leadstore.ActorGremy); err != nil {
				t.Fatal(err)
			}
		}},
		{"no email check", func(t *testing.T, w *sendWorld, _ int64) {
			owner(t, `DELETE FROM email_check`)
		}},
		{"mailbox paused", func(t *testing.T, w *sendWorld, _ int64) {
			if err := w.lq.SetMailboxState(ctx, w.mailbox.ID, leadstore.MailboxPaused, "test", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
				t.Fatal(err)
			}
		}},
		{"mailbox retired", func(t *testing.T, w *sendWorld, _ int64) {
			if err := w.lq.SetMailboxState(ctx, w.mailbox.ID, leadstore.MailboxRetired, "test", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
				t.Fatal(err)
			}
		}},
		{"mailbox DNS failing", func(t *testing.T, w *sendWorld, _ int64) {
			delete(w.dns, "_dmarc."+testSenderDomain)
		}},
		{"mailbox unassigned", func(t *testing.T, w *sendWorld, _ int64) {
			if _, err := w.lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, w.mailbox.ID, false, leadstore.ActorGremy); err != nil {
				t.Fatal(err)
			}
		}},
		{"sending paused", func(t *testing.T, w *sendWorld, _ int64) {
			w.mustRun(t, "leads", "send", "pause", "--reason", "testing")
		}},
		{"reserved domain", func(t *testing.T, w *sendWorld, _ int64) {
			leadsMail.allowReserved = false
		}},
		{"lead moved on (CanSend)", func(t *testing.T, w *sendWorld, _ int64) {
			owner(t, `UPDATE lead SET state = 'replied' WHERE id IN (SELECT lead_id FROM outreach_message WHERE to_address = 'info@alpha.example')`)
		}},
		{"approved text tampered", func(t *testing.T, w *sendWorld, id int64) {
			owner(t, `UPDATE outreach_message SET approved_sha256 = sha256('x') WHERE batch_id = $1 AND step = 0`, id)
		}},
		{"mailbox at the hard ceiling", func(t *testing.T, w *sendWorld, _ int64) {
			w.sentToday(t, w.mailbox, leadconfig.HardDailyCeiling, "ceil")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, leads := manualWorld(t, 20, "alpha.example")
			w.editConfig(t, `per_day: 20`, `per_day: 40`)
			if tc.stop == nil {
				// The control: with no stop, every code overridden, the message goes and leaves its row.
				id, prefix := w.manualBatch(t, w.mailbox, false, leads)
				w.confirm(t, id, prefix, allCodes()...)
				out := w.mustRun(t, "leads", "send", "--from-request")
				if got := w.smtp.recipients(); !slices.Equal(got, []string{"info@alpha.example"}) || len(w.overrides(t, id)) != 1 {
					t.Fatalf("control: sent %v, %d rows:\n%s", got, len(w.overrides(t, id)), out)
				}
				return
			}
			id, prefix := w.manualBatch(t, w.mailbox, false, leads)
			tc.stop(t, w, id)
			w.confirm(t, id, prefix, allCodes()...)
			_, out := w.sendFromRequest(t)
			if got := w.smtp.recipients(); slices.Contains(got, "info@alpha.example") {
				t.Fatalf("a hard stop was passed with every code overridden:\n%s", out)
			}
			if n := len(w.overrides(t, id)); n != 0 {
				t.Errorf("%d override rows for a message not sent", n)
			}
			if s := w.msgState(t, "alpha.example", 0); s == leadstore.MsgSent || s == "sending" || s == "maybe_sent" {
				t.Errorf("the message is %s", s)
			}
		})
	}
}

// TestManualSendOverrides: ramp lets a mailbox past its ramp but never past the hard ceiling;
// domain_daily lets a domain past its daily limit; spacing_jitter waits MinSpacing exactly;
// without codes the limits hold. Each send past a warning leaves its send_override row, and the
// run note names the codes. The contract-drift check: CheckLimits, driven with the run's own
// state, warns about the mailbox the run holds without the code and sends from with it.
func TestManualSendOverrides(t *testing.T) {
	ctx := context.Background()
	domains := []string{"alpha.example", "bravo.example", "charlie.example"}

	// limitsNow is CheckLimits over the world's mailboxes now, for need sends from mailbox mb.
	limitsNow := func(t *testing.T, w *sendWorld, need map[int64]int) ([]outreach.LimitWarning, []outreach.LimitStop) {
		t.Helper()
		cfg, err := leadconfig.Load(w.dir)
		if err != nil {
			t.Fatal(err)
		}
		loc, _ := time.LoadLocation("Europe/London")
		now := w.clock.now()
		ms, err := w.lq.Mailboxes(ctx, leadstore.DefaultAccountID, startOfDay(now, loc))
		if err != nil {
			t.Fatal(err)
		}
		var loads []outreach.MailboxLoad
		for _, m := range ms {
			loads = append(loads, outreach.MailboxLoad{ID: m.ID, Domain: m.Domain, SentToday: m.SentToday, Ready: true})
		}
		return outreach.CheckLimits(cfg, loads, now.In(loc), need)
	}

	for _, tc := range []struct {
		name  string
		codes []string
		sent  int
	}{
		{"ramp held without the code", nil, 1},
		{"ramp lifted with it", []string{"ramp"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, leads := manualWorld(t, 1, domains...)
			id, prefix := w.manualBatch(t, w.mailbox, false, leads)
			warns, stops := limitsNow(t, w, map[int64]int{w.mailbox.ID: 3})
			if len(stops) != 0 || len(warns) != 1 || warns[0].Code != outreach.OverrideRamp || warns[0].MailboxID != w.mailbox.ID || warns[0].Over != 2 {
				t.Fatalf("CheckLimits = %+v, %+v; want one ramp warning, 2 over", warns, stops)
			}
			shown := json.RawMessage(fmt.Sprintf(`[{"code":"ramp","mailbox_id":%d,"used":0,"allowed":1,"over":2}]`, w.mailbox.ID))
			w.confirmShown(t, id, prefix, shown, tc.codes...)
			out := w.mustRun(t, "leads", "send", "--from-request")
			if n := w.smtp.count(); n != tc.sent {
				t.Fatalf("sent %d, want %d:\n%s", n, tc.sent, out)
			}
			rows := w.overrides(t, id)
			note := w.lastNote(t, commandLeadsSend)
			if tc.codes == nil {
				wantLines(t, "held", out, "Held for a later run: today's cap 2.")
				if len(rows) != 0 || strings.Contains(note, noteOverride) {
					t.Errorf("no codes: %d rows, note %q", len(rows), note)
				}
				return
			}
			if !leadbrief.NoteWords(note)[noteOverride+"ramp"] {
				t.Errorf("run note %q lacks the override", note)
			}
			if len(rows) != 3 {
				t.Fatalf("%d override rows, want 3", len(rows))
			}
			runID := int64(w.count(t, `SELECT max(id)::int FROM run WHERE command = $1`, commandLeadsSend))
			for _, r := range rows {
				if r.Operator != manualOperator || !slices.Equal(r.Codes, []string{"ramp"}) || r.RunID == nil || *r.RunID != runID || r.MessageID == nil {
					t.Errorf("override row %+v", r)
				}
				var d limitDetail
				if err := json.Unmarshal(r.Detail, &d); err != nil {
					t.Fatalf("detail %s: %v", r.Detail, err)
				}
				var got, want any
				if err := json.Unmarshal(d.Shown, &got); err != nil {
					t.Fatalf("detail shown %s: %v", d.Shown, err)
				}
				_ = json.Unmarshal(shown, &want)
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Errorf("detail shown %s, want what the confirm page showed %s", d.Shown, shown)
				}
				if d.MailboxID != w.mailbox.ID || d.Need != 3 || d.RoomPlain != 1 || d.Room != leadconfig.HardDailyCeiling || len(d.Warnings) != 1 ||
					d.Warnings[0].Code != "ramp" || d.Warnings[0].Allowed != 1 || d.Warnings[0].Over != 2 {
					t.Errorf("detail %s", r.Detail)
				}
			}
			w.sweep(t)
		})
	}

	t.Run("ramp never past the hard ceiling", func(t *testing.T) {
		w, leads := manualWorld(t, 2, domains...)
		w.sentToday(t, w.mailbox, leadconfig.HardDailyCeiling-1, "hist")
		id, prefix := w.manualBatch(t, w.mailbox, false, leads)
		w.confirm(t, id, prefix, "ramp", "domain_daily")
		out := w.mustRun(t, "leads", "send", "--from-request")
		if n := w.smtp.count(); n != 1 {
			t.Fatalf("sent %d with 39 sent today, want 1 (the ceiling of 40):\n%s", n, out)
		}
		wantLines(t, "ceiling", out, "Held for a later run: today's cap 2.")
		if n := len(w.overrides(t, id)); n != 1 {
			t.Errorf("%d override rows, want 1", n)
		}
	})

	t.Run("domain_daily", func(t *testing.T) {
		for _, codes := range [][]string{nil, {"domain_daily"}} {
			w, leads := manualWorld(t, 20, domains[:2]...)
			w.editConfig(t, `domain_per_day: 50`, `domain_per_day: 3`)
			// The domain's three sends today already went, from the seeded mailbox.
			w.sentToday(t, w.mailbox, 3, "dom")
			id, prefix := w.manualBatch(t, w.mailbox, false, leads)
			warns, _ := limitsNow(t, w, map[int64]int{w.mailbox.ID: 2})
			if len(warns) != 1 || warns[0].Code != outreach.OverrideDomainDaily || warns[0].Used != 3 || warns[0].Allowed != 3 {
				t.Fatalf("CheckLimits warnings %+v, want one domain_daily", warns)
			}
			w.confirm(t, id, prefix, codes...)
			out := w.mustRun(t, "leads", "send", "--from-request")
			want := 0
			if codes != nil {
				want = 2
			}
			if n := w.smtp.count(); n != want {
				t.Fatalf("codes %v: sent %d, want %d:\n%s", codes, n, want, out)
			}
			if n := len(w.overrides(t, id)); n != want {
				t.Errorf("codes %v: %d override rows, want %d", codes, n, want)
			}
		}
	})

	t.Run("spacing_jitter", func(t *testing.T) {
		for _, tc := range []struct {
			codes []string
			gap   time.Duration
		}{{nil, 11 * time.Minute}, {[]string{"spacing_jitter"}, outreach.MinSpacing}} {
			w := newMultiWorld(t, 20)
			for _, d := range domains {
				w.checked(t, verify.StatusSafe, d)
			}
			w.editConfig(t, `spacing_minutes: \[3, 9\]`, `spacing_minutes: [11, 11]`)
			id, prefix := w.manualBatch(t, w.a, false, w.leads(t, domains...))
			w.confirm(t, id, prefix, tc.codes...)
			out := w.mustRun(t, "leads", "send", "--from-request")
			if len(w.sends) != 3 {
				t.Fatalf("codes %v: sent %d:\n%s", tc.codes, len(w.sends), out)
			}
			for i := 1; i < len(w.sends); i++ {
				if gap := w.sends[i].at.Sub(w.sends[i-1].at); gap != tc.gap {
					t.Errorf("codes %v: send %d came %s after the one before, want %s", tc.codes, i, gap, tc.gap)
				}
			}
		}
	})

	t.Run("window and business hours", func(t *testing.T) {
		// 21:00 London on a Tuesday: outside the window.
		for _, tc := range []struct {
			codes []string
			sent  int
		}{{nil, 0}, {[]string{"window"}, 0}, {[]string{"window", "business_hours"}, 2}, {[]string{"schedule", "business_hours"}, 2}} {
			w, leads := manualWorld(t, 20, domains[:2]...)
			id, prefix := w.manualBatch(t, w.mailbox, false, leads)
			w.clock.set(time.Date(2026, 10, 27, 21, 0, 0, 0, time.UTC))
			w.confirm(t, id, prefix, tc.codes...)
			out := w.mustRun(t, "leads", "send", "--from-request")
			if n := w.smtp.count(); n != tc.sent {
				t.Errorf("codes %v at 21:00: sent %d, want %d:\n%s", tc.codes, n, tc.sent, out)
			}
		}
	})
}

// TestManualSendPausedKeepsRequest: a run stopped by the operator's pause sends nothing and
// leaves the request unclaimed; after resume, the same request sends within its 15 minutes.
func TestManualSendPausedKeepsRequest(t *testing.T) {
	w, leads := manualWorld(t, 20, "alpha.example")
	id, prefix := w.manualBatch(t, w.mailbox, false, leads)
	w.confirm(t, id, prefix, "ramp")
	w.mustRun(t, "leads", "send", "pause", "--reason", "testing")
	out := w.mustRun(t, "leads", "send", "--from-request")
	if n := w.smtp.count(); n != 0 {
		t.Fatalf("a paused run sent %d:\n%s", n, out)
	}
	wantLines(t, "paused", out, "Sending is paused by the operator")
	if n := w.count(t, `SELECT count(*) FROM send_request WHERE used_at IS NOT NULL`); n != 0 {
		t.Fatalf("a paused run claimed %d requests", n)
	}
	w.mustRun(t, "leads", "send", "resume")
	w.clock.set(w.clock.now().Add(10 * time.Minute))
	out = w.mustRun(t, "leads", "send", "--from-request")
	if got := w.smtp.recipients(); !slices.Equal(got, []string{"info@alpha.example"}) {
		t.Fatalf("after resume sent %v:\n%s", got, out)
	}
	if n := w.count(t, `SELECT count(*) FROM send_request WHERE used_at IS NOT NULL`); n != 1 {
		t.Errorf("%d requests claimed after resume, want 1", n)
	}
	if n := len(w.overrides(t, id)); n != 1 {
		t.Errorf("%d override rows, want 1", n)
	}
	w.sweep(t)
}

// TestOneFirstEmailPerLead (defence in depth): a lead in two batches gets one first email,
// whichever run sends it: the other first email is refused, named with the batch the first
// went in, in shadow and live alike. A shadow first email never stops a live one.
func TestOneFirstEmailPerLead(t *testing.T) {
	firstEmails := func(t *testing.T, w *sendWorld, state string) int {
		t.Helper()
		return w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 0 AND state = $1`, state)
	}
	t.Run("shadow, two manual batches", func(t *testing.T) {
		w := newSendWorld(t, false, 20)
		lead := w.lead(t, "alpha.example")
		a, pa := w.manualBatch(t, w.mailbox, true, map[int64]string{lead: "alpha.example"})
		w.clock.set(w.clock.now().Add(time.Minute))
		b, pb := w.manualBatch(t, w.mailbox, true, map[int64]string{lead: "alpha.example"})
		w.confirm(t, a, pa)
		w.mustRun(t, "leads", "send", "--from-request")
		w.confirm(t, b, pb, "ramp")
		out := w.mustRun(t, "leads", "send", "--from-request")
		if n := firstEmails(t, w, "shadow_sent"); n != 1 {
			t.Fatalf("%d first emails written for one lead, want 1:\n%s", n, out)
		}
		wantLines(t, "second batch", out, fmt.Sprintf("(its first email already went in batch %d)", a))
		if note := w.lastNote(t, commandLeadsSend); !leadbrief.NoteWords(note)[noteFirstGone] || !strings.Contains(note, "refused 1") {
			t.Errorf("run note %q", note)
		}
		if n := len(w.overrides(t, b)); n != 0 {
			t.Errorf("%d override rows for a refused first email", n)
		}
		// The hourly shadow send never picks it up again.
		w.mustRun(t, "leads", "send")
		if n := firstEmails(t, w, "shadow_sent"); n != 1 {
			t.Errorf("%d first emails after the hourly send", n)
		}
	})
	t.Run("live, an ordinary and a manual batch", func(t *testing.T) {
		w, _ := manualWorld(t, 20)
		lead := w.lead(t, "alpha.example")
		w.checked(t, verify.StatusSafe, "alpha.example")
		w.batch(t, false, map[int64]string{lead: "alpha.example"})
		// However the lead got back to new, a second batch drafts it.
		if _, err := storetest.OwnerConn(t).Exec(context.Background(), `UPDATE lead SET state = 'new' WHERE id = $1`, lead); err != nil {
			t.Fatal(err)
		}
		b, pb := w.manualBatch(t, w.mailbox, false, map[int64]string{lead: "alpha.example"})
		w.mustRun(t, "leads", "send")
		w.confirm(t, b, pb, allCodes()...)
		out := w.mustRun(t, "leads", "send", "--from-request")
		if n := w.smtp.count(); n != 1 {
			t.Fatalf("one lead got %d first emails:\n%s", n, out)
		}
		wantLines(t, "manual", out, "its first email already went in batch")
		if s := w.str(t, `SELECT state FROM outreach_message WHERE batch_id = $1 AND step = 0`, b); s != "refused" {
			t.Errorf("the manual batch's first email is %s, want refused", s)
		}
		w.sweep(t)
	})
	t.Run("a shadow first email never stops a live one", func(t *testing.T) {
		w := newSendWorld(t, false, 20)
		lead := w.lead(t, "alpha.example")
		w.checked(t, verify.StatusSafe, "alpha.example")
		a, pa := w.manualBatch(t, w.mailbox, true, map[int64]string{lead: "alpha.example"})
		w.confirm(t, a, pa)
		w.mustRun(t, "leads", "send", "--from-request")
		if n := firstEmails(t, w, "shadow_sent"); n != 1 {
			t.Fatalf("shadow wrote %d", n)
		}
		t.Setenv("LEADHOUND_OUTREACH", "live")
		w.batch(t, false, map[int64]string{lead: "alpha.example"})
		out := w.mustRun(t, "leads", "send")
		if n := w.smtp.count(); n != 1 {
			t.Errorf("live sent %d after a shadow rehearsal, want 1:\n%s", n, out)
		}
	})
}

// TestManualSendDomainPerRun (amendment 9): two leads at one recipient domain go one per run;
// the other is held for the next.
func TestManualSendDomainPerRun(t *testing.T) {
	w, _ := manualWorld(t, 20)
	a := w.lead(t, "shared.example")
	b := w.lead(t, "other-org.example")
	w.checked(t, verify.StatusSafe, "shared.example")
	id, prefix := w.manualBatch(t, w.mailbox, false, map[int64]string{a: "shared.example", b: "shared.example"})
	w.confirm(t, id, prefix, allCodes()...)
	out := w.mustRun(t, "leads", "send", "--from-request")
	if n := w.smtp.count(); n != 1 {
		t.Fatalf("sent %d to one domain in one run:\n%s", n, out)
	}
	wantLines(t, "one per domain", out, "one per domain per run 1")
}

// TestManualSendShadow (amendment 10): in shadow mode a manual shadow batch is written to the
// outbox as shadow sends are, with its override rows; a live batch is refused in shadow.
func TestManualSendShadow(t *testing.T) {
	w := newSendWorld(t, false, 20)
	leads := map[int64]string{w.lead(t, "alpha.example"): "alpha.example"}
	id, prefix := w.manualBatch(t, w.mailbox, true, leads)
	w.confirm(t, id, prefix, "ramp", "window")
	out := w.mustRun(t, "leads", "send", "--from-request")
	wantLines(t, "shadow", out, "Shadow: wrote 1 to the outbox")
	if s := w.msgState(t, "alpha.example", 0); s != "shadow_sent" {
		t.Errorf("message %s, want shadow_sent", s)
	}
	if rows := w.overrides(t, id); len(rows) != 1 || !slices.Equal(rows[0].Codes, []string{"ramp", "window"}) {
		t.Errorf("override rows %+v", rows)
	}
	if note := w.lastNote(t, commandLeadsSend); !leadbrief.NoteWords(note)[noteOverride+"ramp,window"] {
		t.Errorf("run note %q", note)
	}

	w2 := newSendWorld(t, false, 20)
	leads = map[int64]string{w2.lead(t, "bravo.example"): "bravo.example"}
	id, prefix = w2.manualBatch(t, w2.mailbox, false, leads)
	w2.confirm(t, id, prefix)
	if code, out := w2.sendFromRequest(t); code == 0 || !strings.Contains(out, "is a live batch and sending is shadow") {
		t.Errorf("a live batch in shadow exited %d:\n%s", code, out)
	}
}

// TestManualSendSelftest (amendment 10): a manual batch of a selftest lead runs as the selftest
// does: its one message goes whatever the cap, alone.
func TestManualSendSelftest(t *testing.T) {
	ctx := context.Background()
	w := newSendWorld(t, true, 1)
	w.sentToday(t, w.mailbox, 1, "full")
	var st leadstore.Selftest
	pool := storetest.AppConn(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err = leadstore.NewQueries(tx).CreateSelftest(ctx, leadstore.SelftestDraft{To: testOperator, Subject: "Leadhound selftest", Body: "test\n",
		MessageID: "st1@" + testSenderDomain, Due: w.clock.now(), MailboxID: w.mailbox.ID, FromName: "Demo"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leadstore.NewQueries(tx).CancelPending(ctx, leadstore.CancelTarget{LeadID: st.LeadID}, "test"); err != nil {
		t.Fatal(err)
	}
	// Back to new, so a manual batch can draft it as the console's preview would.
	if _, err := tx.Exec(ctx, `UPDATE lead SET state = 'new' WHERE id = $1`, st.LeadID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	other := w.lead(t, "alpha.example")
	w.checked(t, verify.StatusSafe, "alpha.example")
	w.batch(t, false, map[int64]string{other: "alpha.example"})

	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := leadstore.NewQueries(tx)
	id, sum, err := q.CreateBatch(ctx, false, []leadstore.DraftRow{{LeadID: st.LeadID, Step: 0, Channel: "email", To: testOperator,
		Subject: "Manual selftest", Body: "test\n", MessageID: "st2@" + testSenderDomain, MailboxID: w.mailbox.ID, FromName: "Demo"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.MarkBatchManual(ctx, id); err != nil {
		t.Fatal(err)
	}
	prefix := hex.EncodeToString(sum)[:16]
	now := w.clock.now()
	if _, err := q.ApproveBatch(ctx, id, prefix, nil, func(leadstore.Message) time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w.confirm(t, id, prefix)
	out := w.mustRun(t, "leads", "send", "--from-request")
	if got := w.smtp.recipients(); !slices.Equal(got, []string{testOperator}) {
		t.Fatalf("sent %v, want the selftest alone past the full cap:\n%s", got, out)
	}
	wantLines(t, "selftest", out, "Sent 1: your selftest address")
	if s := w.msgState(t, "alpha.example", 0); s != leadstore.MsgApproved {
		t.Errorf("the ordinary batch's email is %s", s)
	}
}

// allCodes is every override code, as the console's confirm would send them.
func allCodes() []string {
	var out []string
	for _, c := range outreach.AllOverrideCodes() {
		out = append(out, string(c))
	}
	return out
}

// removeSendNowFile deletes the Send now request file under data.
func removeSendNowFile(data string) error {
	return os.Remove(filepath.Join(data, runRequestDir, sendNowAction))
}
