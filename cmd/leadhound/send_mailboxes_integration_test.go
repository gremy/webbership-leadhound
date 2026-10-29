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
	"github.com/gremy/webbership-leadhound/internal/secretbox"
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

// add seeds a mailbox for address, with passing DNS records for its host (selector sel1).
func (w *multiWorld) add(t *testing.T, address string) leadstore.Mailbox {
	t.Helper()
	m := seedMailbox(t, w.lq, address)
	if h := m.Host(); h != testSenderDomain {
		w.dns[h] = []string{"v=spf1 -all"}
		w.dns["sel1._domainkey."+h] = []string{"v=DKIM1; k=rsa; p=MIIB"}
		w.dns["_dmarc."+h] = []string{"v=DMARC1; p=none"}
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
// a policy block (5.7.x) pauses every mailbox on its domain and the message is refused; a refused
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
		if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 0 AND state = 'approved' AND mailbox_id IN ($1, $2)`, w.a.ID, b.ID); n != 2 {
			t.Errorf("%d first emails of the blocked domain still approved, want the 2 not tried", n)
		}
		if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 0 AND state = 'refused' AND mailbox_id = $1`, w.a.ID); n != 1 {
			t.Errorf("%d blocked messages refused, want 1", n)
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
	if err := w.lq.SetSendPause(ctx, true, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	out := w.mustRun(t, "leads", "send")
	wantLines(t, "paused", out, "Sending is paused by the operator")
	if len(w.sends) != 0 || len(w.mail.calls) != 0 {
		t.Fatal("a paused run reached a mailbox")
	}
	if err := w.lq.SetSendPause(ctx, false, "", leadstore.ActorGremy); err != nil {
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

// policyBlock is a receiving server's 5.7.1 at RCPT.
func policyBlock() error {
	return &mailbox.SendError{Kind: mailbox.Refused, Stage: "rcpt", Code: 550, Enhanced: [3]int{5, 7, 1}, Text: "blocked"}
}

// TestSendPolicyBlockRefusesTheMessage: the message a receiving server blocked by policy
// (5.7.x) is refused, its address not suppressed, and the domain stays paused; once the
// operator resumes the domain, the next run sends the others and does not pause it again,
// though that recipient's server still blocks.
func TestSendPolicyBlockRefusesTheMessage(t *testing.T) {
	w := newMultiWorld(t, 20)
	b := w.add(t, "b@"+testSenderDomain)
	w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example"))
	w.batchFrom(t, b, false, w.leads(t, "b1.example"))
	w.smtp.errs = map[int]error{0: policyBlock()}
	w.mustRun(t, "leads", "send")
	blocked := w.smtp.recipients()
	if len(blocked) != 1 {
		t.Fatalf("A tried %v, want one send and then the domain pause", blocked)
	}
	w.smtp.errTo = map[string]error{blocked[0]: policyBlock()}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE to_address = $1 AND step = 0 AND state = 'refused'`, blocked[0]); n != 1 {
		t.Errorf("the blocked message is not refused (%d refused)", n)
	}
	if n := w.count(t, `SELECT count(*) FROM suppression`); n != 0 {
		t.Error("a policy block suppressed the recipient")
	}
	want := "paused domain system: a receiving server blocked mail from " + testSenderDomain + " (5.7.1)"
	for _, id := range []int64{w.a.ID, b.ID} {
		if rows := mailboxStateRows(t, id); len(rows) != 1 || rows[0] != want {
			t.Errorf("mailbox %d state rows %q, want [%q]", id, rows, want)
		}
	}

	ctx := context.Background()
	if _, err := setDomainPause(ctx, w.lq, w.clock.now(), testSenderDomain, false, ""); err != nil {
		t.Fatal(err)
	}
	w.mustRun(t, "leads", "send")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 0 AND state = 'sent' AND mailbox_id IN ($1, $2)`, w.a.ID, b.ID); n != 2 {
		t.Errorf("%d first emails sent after the resume, want the other 2", n)
	}
	for _, id := range []int64{w.a.ID, b.ID} {
		st, err := mailboxStatusByID(ctx, w.lq, id, w.clock.now())
		if err != nil {
			t.Fatal(err)
		}
		if st.State != leadstore.MailboxActive || len(mailboxStateRows(t, id)) != 2 {
			t.Errorf("mailbox %d after the resume: %s, rows %q", id, st.State, mailboxStateRows(t, id))
		}
	}
}

// TestSendDomainPauseKeepsOwnPause: a policy block on a domain writes no domain row over a
// mailbox there already paused on its own (by bounces), so the operator's resume of the
// domain leaves that mailbox paused with its own reason.
func TestSendDomainPauseKeepsOwnPause(t *testing.T) {
	w := newMultiWorld(t, 20)
	b := w.add(t, "b@"+testSenderDomain)
	ctx := context.Background()
	const bounced = "2 hard bounces in its last 50 live sends"
	if err := w.lq.SetMailboxState(ctx, w.a.ID, leadstore.MailboxPaused, bounced, leadstore.ScopeMailbox, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	w.batchFrom(t, b, false, w.leads(t, "b1.example"))
	w.mail.smtpOf(b.Address).errs = map[int]error{0: policyBlock()}
	w.mustRun(t, "leads", "send")
	wantA := "paused mailbox system: " + bounced
	if rows := mailboxStateRows(t, w.a.ID); len(rows) != 1 || rows[0] != wantA {
		t.Errorf("A state rows %q, want [%q]", rows, wantA)
	}
	if rows := mailboxStateRows(t, b.ID); len(rows) != 1 || !strings.HasPrefix(rows[0], "paused domain system: ") {
		t.Errorf("B state rows %q", rows)
	}
	if _, err := setDomainPause(ctx, w.lq, w.clock.now(), testSenderDomain, false, ""); err != nil {
		t.Fatal(err)
	}
	a, err := mailboxStatusByID(ctx, w.lq, w.a.ID, w.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if a.State != leadstore.MailboxPaused || a.Reason != bounced || a.Scope != leadstore.ScopeMailbox {
		t.Errorf("A after the domain resume: %s %s %q, want paused by its bounces", a.State, a.Scope, a.Reason)
	}
	if st, err := mailboxStatusByID(ctx, w.lq, b.ID, w.clock.now()); err != nil || st.State != leadstore.MailboxActive {
		t.Errorf("B after the domain resume: %+v, %v", st.State, err)
	}
}

// TestSendChecksTheAddressHost: a mailbox on a subdomain has its DNS checked at its own host
// (where its records are) and its readiness reads that host's pass, while
// caps.domain_per_day still counts its registrable domain.
func TestSendChecksTheAddressHost(t *testing.T) {
	w := newMultiWorld(t, 20)
	w.editConfig(t, `domain_per_day: 50`, `domain_per_day: 1`)
	x := w.add(t, "x@"+"mail.brand.example")
	if x.Domain != "brand"+".example" || x.Host() != "mail.brand"+".example" {
		t.Fatalf("x domain %q host %q", x.Domain, x.Host())
	}
	// y on the registrable domain, with another selector, so no record of x's sits there.
	y := seedMailbox(t, w.lq, "y@"+"brand.example", withSelector("s2"), withDNSPass())
	w.watch(y.Address)
	w.batchFrom(t, x, false, w.leads(t, "x1.example", "x2.example"))
	w.batchFrom(t, y, false, w.leads(t, "y1.example"))
	out := w.mustRun(t, "leads", "send")
	if strings.Contains(out, "DNS check failed") {
		t.Errorf("a mailbox failed its DNS check:\n%s", out)
	}
	if n := len(w.sentFrom(x.Address)) + len(w.sentFrom(y.Address)); n != 1 {
		t.Errorf("%d sends from brand.example, want caps.domain_per_day 1:\n%s", n, out)
	}
	st, err := mailboxStatusByID(context.Background(), w.lq, x.ID, w.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	if !st.DNSPassedToday {
		t.Error("x's readiness does not read its host's pass")
	}
	if n := w.count(t, `SELECT count(*) FROM sender_check WHERE domain_sha256 = $1 AND selector = 'sel1' AND spf AND dkim AND dmarc`,
		leadstore.SenderDomainSHA256(x.Host())); n != 1 {
		t.Errorf("%d passes recorded for x's host, want 1", n)
	}
}

// TestSendMidRunEdit: a mailbox whose servers and password the operator changes during a run
// (sealed again, so the sync opens it) is opened again before its next send: the next
// message goes through the new server, never the sender built for the old one.
func TestSendMidRunEdit(t *testing.T) {
	w := newMultiWorld(t, 20)
	w.editConfig(t, `spacing_minutes: \[3, 9\]`, `spacing_minutes: [11, 11]`)
	w.batchFrom(t, w.a, false, w.leads(t, "a1.example", "a2.example"))
	moved := "smtp.moved" + ".invalid:465"
	key := currentTestKey(t)
	w.smtp.onSend = func(n int, _ string) {
		if n != 0 {
			return
		}
		sealed, err := secretbox.Seal(key.Public(), secretbox.Credentials{
			Address: w.a.Address, Login: w.a.Login, SMTPAddr: moved, IMAPAddr: w.a.IMAPAddr, Password: config.Secret(testPassword),
		}, nil)
		if err != nil {
			t.Error(err)
			return
		}
		if err := w.lq.UpdateMailbox(context.Background(), w.a.ID, leadstore.MailboxEdit{SMTPAddr: &moved,
			PasswordSealed: sealed, SealedBy: secretbox.Fingerprint(key.Public())}); err != nil {
			t.Error(err)
		}
	}
	out := w.mustRun(t, "leads", "send")
	if n := w.smtp.count(); n != 2 {
		t.Errorf("A sent %d, want 2:\n%s", n, out)
	}
	var addrs []string
	for _, c := range w.mail.callsOf("smtp") {
		addrs = append(addrs, c.addr)
	}
	if len(addrs) != 2 || addrs[1] != moved {
		t.Errorf("SMTP senders built for %q, want the second for the new server %q", addrs, moved)
	}
}

// TestSendKeepsTheFrozenFromName: the From name is the one frozen with the message; a change
// of leads.yaml's sender.name after approval changes nothing, and an empty frozen name sends
// with no display name.
func TestSendKeepsTheFrozenFromName(t *testing.T) {
	w := newMultiWorld(t, 20)
	b := w.add(t, "b@"+"other.example")
	w.fromName = "Frozen Name"
	w.batchFrom(t, w.a, false, w.leads(t, "a1.example"))
	w.fromName = ""
	w.batchFrom(t, b, false, w.leads(t, "b1.example"))
	w.editConfig(t, `name: "Demo Sender"`, `name: "Changed Name"`)
	w.mustRun(t, "leads", "send")
	fromLine := func(raw []byte) string {
		for l := range strings.SplitSeq(string(raw), "\r\n") {
			if strings.HasPrefix(l, "From: ") {
				return l
			}
		}
		return ""
	}
	a, bs := w.mail.smtpOf(w.a.Address).sent, w.mail.smtpOf(b.Address).sent
	if len(a) != 1 || len(bs) != 1 {
		t.Fatalf("A sent %d, B %d, want 1 each", len(a), len(bs))
	}
	if l := fromLine(a[0].raw); !strings.Contains(l, "Frozen Name") || strings.Contains(l, "Changed Name") {
		t.Errorf("A's From %q, want the frozen name", l)
	}
	if l := fromLine(bs[0].raw); strings.Contains(l, "Changed Name") || strings.Contains(l, "Demo Sender") || strings.Contains(l, "\"") {
		t.Errorf("B's From %q, want no display name", l)
	}
}
