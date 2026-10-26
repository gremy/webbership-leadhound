//go:build integration

package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
)

// The send run over many mailboxes (MB-R9, MB-R11). Until the cursor key is swapped to the
// mailbox, every mailbox but the seeded one shows its folders under its own prefix
// (renamedReader), so two INBOX cursors never share a row.

// multiWorld is a live send world (mailbox A: testUser) with more mailboxes added by add.
type multiWorld struct {
	*sendWorld
	a leadstore.Mailbox

	mu    sync.Mutex
	sends []timedSend // every SMTP call of every mailbox, in order
}

// timedSend is one SMTP call: the mailbox's login (the envelope sender), the recipient and
// the fake clock's time.
type timedSend struct {
	from, to string
	at       time.Time
}

func newMultiWorld(t *testing.T, capPerDay int) *multiWorld {
	t.Helper()
	w := &multiWorld{sendWorld: newSendWorld(t, true, capPerDay)}
	w.a = w.mailbox
	w.watch(w.a.Address)
	plain := w.mail.readerFor
	leadsMail.readerFor = func(ctx context.Context, addr string, user, password config.Secret) (mailbox.Reader, error) {
		r, err := plain(ctx, addr, user, password)
		if err == nil && !strings.EqualFold(user.Reveal(), testUser) {
			return renamedReader{Reader: r, prefix: strings.ToUpper(user.Reveal()[:1]) + "-"}, nil
		}
		return r, err
	}
	return w
}

// add seeds a mailbox for address, with passing DNS records for its domain (selector sel1).
func (w *multiWorld) add(t *testing.T, address string) leadstore.Mailbox {
	t.Helper()
	m := seedMailbox(t, w.lq, address)
	if m.Domain != testSenderDomain {
		w.dns[m.Domain] = []string{"v=spf1 -all"}
		w.dns["sel1._domainkey."+m.Domain] = []string{"v=DKIM1; k=rsa; p=MIIB"}
		w.dns["_dmarc."+m.Domain] = []string{"v=DMARC1; p=none"}
	}
	w.watch(address)
	return m
}

// watch records every send of login's fake SMTP with the clock's time.
func (w *multiWorld) watch(login string) {
	f := w.mail.smtpOf(login)
	f.onSend = func(_ int, to string) {
		w.mu.Lock()
		w.sends = append(w.sends, timedSend{from: strings.ToLower(login), to: to, at: w.clock.now()})
		w.mu.Unlock()
	}
}

// leads makes one lead per domain and returns them for batchFrom.
func (w *multiWorld) leads(t *testing.T, domains ...string) map[int64]string {
	t.Helper()
	out := map[int64]string{}
	for _, d := range domains {
		out[w.lead(t, d)] = d
	}
	return out
}

func (w *multiWorld) sentFrom(login string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, s := range w.sends {
		if s.from == strings.ToLower(login) {
			out = append(out, s.to)
		}
	}
	return out
}

// TestSendManyMailboxes: one run sends from every ready mailbox, each message from its own
// mailbox (envelope, From header and Sent copy), with the one spacing between any two sends.
func TestSendManyMailboxes(t *testing.T) {
	w := newMultiWorld(t, 20)
	w.editConfig(t, `spacing_minutes: \[3, 9\]`, `spacing_minutes: [11, 11]`)
	b := w.add(t, "b@"+"other.example")
	w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example"))
	w.batchFrom(t, b, false, w.leads(t, "b1.example", "b2.example"))

	out := w.mustRun(t, "leads", "send")
	wantLines(t, "two mailboxes", out, "Sent 4: ")
	if got := w.sentFrom(w.a.Address); len(got) != 2 || !slices.Contains(got, "info@a1.example") || !slices.Contains(got, "info@a2.example") {
		t.Errorf("A sent %v", got)
	}
	if got := w.sentFrom(b.Address); len(got) != 2 || !slices.Contains(got, "info@b1.example") || !slices.Contains(got, "info@b2.example") {
		t.Errorf("B sent %v", got)
	}
	for i := 1; i < len(w.sends); i++ {
		if gap := w.sends[i].at.Sub(w.sends[i-1].at); gap < 11*time.Minute {
			t.Errorf("send %d came %s after the one before, want the 11-minute spacing", i, gap)
		}
	}
	for _, m := range w.mail.smtpOf(b.Address).sent {
		if m.from != b.Address || !strings.Contains(string(m.raw), "From: ") || !strings.Contains(string(m.raw), b.Address) {
			t.Errorf("B's message went as %q:\n%s", m.from, m.raw)
		}
	}
	if w.box.appended != 2 || w.mail.boxOf(b.Address).appended != 2 {
		t.Errorf("Sent copies: A %d, B %d, want 2 each in its own mailbox", w.box.appended, w.mail.boxOf(b.Address).appended)
	}
	for _, c := range w.mail.callsOf("smtp") {
		if !c.passwordOK {
			t.Errorf("an SMTP sender was built with a wrong password: %+v", c)
		}
	}
	w.sweep(t, b.Address)
}

// TestSendCapsPerMailboxAndDomain: each mailbox keeps its own cap; two mailboxes on one
// domain share caps.domain_per_day; the one-per-recipient-domain rule spans mailboxes.
func TestSendCapsPerMailboxAndDomain(t *testing.T) {
	t.Run("per mailbox", func(t *testing.T) {
		w := newMultiWorld(t, 2)
		b := w.add(t, "b@"+"other.example")
		w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example", "a3.example"))
		w.batchFrom(t, b, false, w.leads(t, "b1.example", "b2.example", "b3.example"))
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "caps", out, "Sent 4: ", "Held for a later run: today's cap 2.")
		if a, bs := len(w.sentFrom(w.a.Address)), len(w.sentFrom(b.Address)); a != 2 || bs != 2 {
			t.Errorf("A sent %d, B %d, want 2 each (its own cap)", a, bs)
		}
	})
	t.Run("domain ceiling", func(t *testing.T) {
		w := newMultiWorld(t, 20)
		w.editConfig(t, `domain_per_day: 50`, `domain_per_day: 3`)
		b := w.add(t, "b@"+testSenderDomain)
		w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example", "a3.example"))
		w.batchFrom(t, b, false, w.leads(t, "b1.example", "b2.example", "b3.example"))
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "domain ceiling", out, "Sent 3: ", "Held for a later run: today's cap 3.")
		if n := len(w.sends); n != 3 {
			t.Errorf("%d sends from one domain, want caps.domain_per_day 3", n)
		}
	})
	t.Run("recipient domain across mailboxes", func(t *testing.T) {
		w := newMultiWorld(t, 20)
		b := w.add(t, "b@"+"other.example")
		w.batchFrom(t, w.a, false, w.leads(t, "shared.example"))
		// Another lead, written to at the same recipient domain from B.
		w.batchFrom(t, b, false, map[int64]string{w.lead(t, "other-org.example"): "shared.example"})
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "one per domain", out, "Sent 1: ", "one per domain per run 1")
	})
}

// TestSendMailboxHealth: two hard bounces pause their mailbox while the other keeps sending;
// a policy block (5.7.x) pauses every mailbox on its domain and the message waits; a refused
// login pauses its mailbox alone and the run exits non-zero.
func TestSendMailboxHealth(t *testing.T) {
	t.Run("bounces", func(t *testing.T) {
		w := newMultiWorld(t, 20)
		b := w.add(t, "b@"+"other.example")
		w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example", "a3.example"))
		w.batchFrom(t, b, false, w.leads(t, "b1.example", "b2.example"))
		bounce := &mailbox.SendError{Kind: mailbox.Refused, Stage: "rcpt", Code: 550, Bounce: true, Enhanced: [3]int{5, 1, 1}, Text: "no such user"}
		w.smtp.errs = map[int]error{0: bounce, 1: bounce}
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "bounces", out, fmt.Sprintf("Mailbox %d: nothing is sent from it in this run: it hard-bounced too often", w.a.ID),
			"Sent 2: ", "its mailbox cannot send now 1")
		if got := w.sentFrom(w.a.Address); len(got) != 2 {
			t.Errorf("A tried %v, want two and then paused", got)
		}
		if got := w.sentFrom(b.Address); len(got) != 2 {
			t.Errorf("B sent %v, want both", got)
		}
		if rows := mailboxStateRows(t, w.a.ID); len(rows) != 1 || rows[0] != "paused mailbox system: 2 hard bounces in its last 50 live sends" {
			t.Errorf("A state rows %q", rows)
		}
		if rows := mailboxStateRows(t, b.ID); len(rows) != 0 {
			t.Errorf("B state rows %q", rows)
		}
		if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE hard_bounce AND mailbox_id = $1`, w.a.ID); n != 2 {
			t.Errorf("%d hard bounces recorded", n)
		}
		if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "mailbox paused") {
			t.Errorf("note %q", note)
		}
	})
	t.Run("policy block", func(t *testing.T) {
		w := newMultiWorld(t, 20)
		b := w.add(t, "b@"+testSenderDomain)
		c := w.add(t, "c@"+"other.example")
		w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example"))
		w.batchFrom(t, b, false, w.leads(t, "b1.example"))
		w.batchFrom(t, c, false, w.leads(t, "c1.example"))
		w.smtp.errs = map[int]error{0: &mailbox.SendError{Kind: mailbox.Refused, Stage: "rcpt", Code: 550, Enhanced: [3]int{5, 7, 1}, Text: "blocked"}}
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "policy", out, "a receiving server blocked mail from its domain (5.7.1)", "Sent 1: c1.example")
		if got := w.sentFrom(b.Address); len(got) != 0 {
			t.Errorf("B on the blocked domain sent %v", got)
		}
		want := "paused domain system: a receiving server blocked mail from " + testSenderDomain + " (5.7.1)"
		for _, id := range []int64{w.a.ID, b.ID} {
			if rows := mailboxStateRows(t, id); len(rows) != 1 || rows[0] != want {
				t.Errorf("mailbox %d state rows %q, want [%q]", id, rows, want)
			}
		}
		if rows := mailboxStateRows(t, c.ID); len(rows) != 0 {
			t.Errorf("C state rows %q", rows)
		}
		if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 0 AND state = 'approved' AND mailbox_id IN ($1, $2)`, w.a.ID, b.ID); n != 3 {
			t.Errorf("%d first emails of the blocked domain still approved, want 3", n)
		}
		if n := w.count(t, `SELECT count(*) FROM suppression`); n != 0 {
			t.Error("a policy block suppressed the recipient")
		}
		w.sweep(t, b.Address, c.Address)
	})
	t.Run("login refused", func(t *testing.T) {
		w := newMultiWorld(t, 20)
		b := w.add(t, "b@"+"other.example")
		w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example"))
		w.batchFrom(t, b, false, w.leads(t, "b1.example"))
		w.smtp.errs = map[int]error{0: errLoginRefused()}
		code, out := w.run(t, "leads", "send")
		if code != 1 {
			t.Errorf("exited %d, want 1:\n%s", code, out)
		}
		wantLines(t, "login", out, "the server refused the login (auth, 535)", "Sent 1: b1.example")
		if rows := mailboxStateRows(t, w.a.ID); len(rows) != 1 || rows[0] != "paused mailbox system: the server refused the login" {
			t.Errorf("A state rows %q", rows)
		}
		if len(w.sentFrom(w.a.Address)) != 1 {
			t.Errorf("A tried %v after its login was refused", w.sentFrom(w.a.Address))
		}
	})
}

// TestSendCredentialsBound: a mailbox whose servers changed in the database without its
// password sealed again is paused, and its password never goes to the new host.
func TestSendCredentialsBound(t *testing.T) {
	w := newMultiWorld(t, 20)
	b := w.add(t, "b@"+"other.example")
	w.batchFrom(t, w.a, false, w.leads(t, "a1.example"))
	w.batchFrom(t, b, false, w.leads(t, "b1.example"))
	evil := "smtp.evil" + ".invalid:465"
	ownerExec(t, `UPDATE sender_mailbox SET smtp_addr = $2 WHERE id = $1`, w.a.ID, evil)

	out := w.mustRun(t, "leads", "send")
	wantLines(t, "rebound", out, "Sent 1: b1.example")
	for _, c := range w.mail.calls {
		if c.addr == evil {
			t.Errorf("the password went to the new host: %+v", c)
		}
	}
	if len(w.sentFrom(w.a.Address)) != 0 {
		t.Error("A sent with a password sealed for other servers")
	}
	if rows := mailboxStateRows(t, w.a.ID); len(rows) != 1 || rows[0] != "paused mailbox system: re-enter the password: the mailbox's settings changed since it was saved" {
		t.Errorf("A state rows %q", rows)
	}
	if s := w.msgState(t, "a1.example", 0); s != leadstore.MsgApproved {
		t.Errorf("A's message %s, want approved", s)
	}
}

// TestSendPauseAndRetire: the operator's global pause still wins over every mailbox; a
// retired mailbox's messages are cancelled and the others send.
func TestSendPauseAndRetire(t *testing.T) {
	w := newMultiWorld(t, 20)
	b := w.add(t, "b@"+"other.example")
	w.batchFrom(t, w.a, false, w.leads(t, "a1.example"))
	w.batchFrom(t, b, false, w.leads(t, "b1.example"))
	ctx := context.Background()
	if err := w.lq.SetSendPause(ctx, true, ""); err != nil {
		t.Fatal(err)
	}
	out := w.mustRun(t, "leads", "send")
	wantLines(t, "paused", out, "Sending is paused by the operator")
	if len(w.sends) != 0 || len(w.mail.calls) != 0 {
		t.Fatal("a paused run reached a mailbox")
	}
	if err := w.lq.SetSendPause(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.lq.SetMailboxState(ctx, w.a.ID, leadstore.MailboxRetired, "done", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "retired", out, "Sent 1: b1.example", "3 pending messages cancelled")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE mailbox_id = $1 AND state = 'cancelled' AND cancel_reason = 'mailbox retired'`, w.a.ID); n != 3 {
		t.Errorf("%d of A's messages cancelled as mailbox retired, want 3", n)
	}
	if len(w.sentFrom(w.a.Address)) != 0 {
		t.Error("a retired mailbox sent")
	}
}

// TestSelftestPerMailbox: with two mailboxes a selftest names one; it goes from that one.
func TestSelftestPerMailbox(t *testing.T) {
	w := newMultiWorld(t, 20)
	b := w.add(t, "b@"+"other.example")
	code, out := w.run(t, "leads", "selftest", "--to", testOperator)
	if code != 2 || !strings.Contains(out, "--mailbox") {
		t.Errorf("selftest without --mailbox exited %d:\n%s", code, out)
	}
	if code, out := w.run(t, "leads", "selftest", "--to", testOperator, "--mailbox", "nobody@"+"other.example"); code != 2 || strings.Contains(out, "nobody@") {
		t.Errorf("selftest from an unknown mailbox exited %d:\n%s", code, out)
	}
	out = w.mustRun(t, "leads", "selftest", "--to", testOperator, "--mailbox", fmt.Sprint(b.ID))
	wantLines(t, "selftest from B", out, "Sent 1: your selftest address")
	if got := w.sentFrom(b.Address); len(got) != 1 || got[0] != testOperator {
		t.Errorf("B sent %v", got)
	}
	if len(w.sentFrom(w.a.Address)) != 0 {
		t.Error("the selftest went from A")
	}
	if id := w.str(t, `SELECT m.message_id FROM outreach_message m JOIN lead l ON l.id = m.lead_id WHERE l.kind = 'selftest'`); !strings.HasSuffix(id, "@other.example") {
		t.Errorf("selftest Message-ID %q, want B's domain", id)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE mailbox_id = $1`, b.ID); n != 1 {
		t.Errorf("%d messages on B, want the selftest", n)
	}
	out = w.mustRun(t, "leads", "selftest", "--to", testOperator, "--mailbox", strings.ToUpper(w.a.Address))
	wantLines(t, "selftest from A", out, "Sent 1: your selftest address")
	if len(w.sentFrom(w.a.Address)) != 1 {
		t.Error("the selftest by address did not go from A")
	}
	w.sweep(t, b.Address)
}
