//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
)

// coreWorld is a reset database, a data dir, the test key and the per-mailbox fakes, for
// the mailbox core.
type coreWorld struct {
	dir  string
	lq   leadstore.Queries
	key  secretbox.PrivateKey
	cfg  leadconfig.Config
	mail *mailboxFakes
	now  time.Time
}

func newCoreWorld(t *testing.T) *coreWorld {
	t.Helper()
	w := &coreWorld{dir: leadsDB(t), lq: leadstoretest.Q(t), key: testKey(t), now: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	cfg, err := leadconfig.LoadForProbe(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	w.cfg = cfg
	savedMail, savedNow := leadsMail, leadsNow
	t.Cleanup(func() { leadsMail, leadsNow = savedMail, savedNow })
	leadsNow = func() time.Time { return w.now }
	leadsMail.allowReserved = true
	leadsMail.resolver = passingDNS()
	w.mail = newMailboxFakes()
	w.mail.install(&leadsMail)
	t.Setenv(secretbox.PublicKeyEnv, w.key.Public().String())
	t.Setenv("LEADHOUND_TZ", "Europe/London")
	return w
}

func (w *coreWorld) input(address string) MailboxInput {
	return MailboxInput{Address: address, SMTPAddr: "smtp." + testSenderDomain + ":465", IMAPAddr: "imap." + testSenderDomain + ":993",
		Selector: "sel1", Password: config.Secret(testPassword)}
}

func (w *coreWorld) add(t *testing.T, in MailboxInput) int64 {
	t.Helper()
	id, err := addMailbox(context.Background(), w.lq, w.cfg, w.key.Public(), w.dir, w.now, in)
	if err != nil {
		t.Fatalf("add %s: %v", in.Address, err)
	}
	return id
}

func (w *coreWorld) status(t *testing.T, id int64) leadstore.MailboxStatus {
	t.Helper()
	ms, err := w.lq.Mailboxes(context.Background(), w.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("mailbox %d not listed", id)
	return leadstore.MailboxStatus{}
}

func (w *coreWorld) requestFile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.dir, runRequestDir, mailboxCheckAction)) // #nosec G304 -- the test's data dir
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// refusalWords is err's words when it is an ErrUserFixable refusal, else fails.
func refusalWords(t *testing.T, err error) string {
	t.Helper()
	if !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	return strings.TrimPrefix(err.Error(), console.ErrUserFixable.Error()+": ")
}

// TestAddMailboxCore: the address is normalised, its domain derived, the credentials sealed
// for exactly the stored fields with the key's fingerprint, a check requested and the
// request file written; the password is stored only sealed.
func TestAddMailboxCore(t *testing.T) {
	w := newCoreWorld(t)
	ctx := context.Background()
	in := w.input("  Sales@Sender.Example ")
	in.FromName = "Sales Desk"
	id := w.add(t, in)

	m, err := w.lq.MailboxByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Address != "sales@"+testSenderDomain || m.Domain != testSenderDomain || m.Login != m.Address ||
		m.DKIMSelector != "sel1" || m.FromName != "Sales Desk" || m.SealedBy != secretbox.Fingerprint(w.key.Public()) {
		t.Errorf("stored %+v", m)
	}
	if bytes.Contains(m.PasswordSealed, []byte(testPassword)) {
		t.Error("the password is stored in the clear")
	}
	creds, err := secretbox.Open(w.key, m.PasswordSealed)
	if err != nil || creds.Password.Reveal() != testPassword || !creds.Matches(m.Address, m.Login, m.SMTPAddr, m.IMAPAddr) {
		t.Errorf("the seal opens to %+v, %v", creds, err)
	}
	st := w.status(t, id)
	if st.State != leadstore.MailboxActive || st.LastCheck == nil || st.LastCheck.Outcome != leadstore.CheckRequested || st.LastCheck.Actor != leadstore.ActorOperator {
		t.Errorf("status %+v, check %+v", st, st.LastCheck)
	}
	if got := w.requestFile(t); got != w.now.UTC().Format(time.RFC3339Nano) {
		t.Errorf("request file = %q", got)
	}

	// A preset fills the servers and the selector; IDN becomes punycode.
	idn := w.add(t, MailboxInput{Address: "info@bücher.example", Preset: "spacemail", Password: config.Secret(testPassword)})
	m, err = w.lq.MailboxByID(ctx, idn)
	if err != nil {
		t.Fatal(err)
	}
	p := mailboxPresets[0]
	if m.Address != "info@xn--bcher-kva.example" || m.Domain != "xn--bcher-kva.example" || m.SMTPAddr != p.SMTP || m.IMAPAddr != p.IMAP || m.DKIMSelector != p.Selector {
		t.Errorf("preset mailbox %+v", m)
	}
	// No preset and no selector: leads.yaml's selector.
	other := w.input("x@other.example")
	other.Selector = ""
	m, err = w.lq.MailboxByID(ctx, w.add(t, other))
	if err != nil || m.DKIMSelector != dkimSelector(w.cfg) {
		t.Errorf("default selector = %q, %v; want %q", m.DKIMSelector, err, dkimSelector(w.cfg))
	}
}

// TestAddMailboxRefusals: each refusal is plain words and writes nothing.
func TestAddMailboxRefusals(t *testing.T) {
	w := newCoreWorld(t)
	ctx := context.Background()
	w.add(t, w.input("one@"+testSenderDomain))
	w.add(t, w.input("two@"+testSenderDomain))
	rows := func() int {
		return batchCount(t, `SELECT (SELECT count(*) FROM sender_mailbox) + (SELECT count(*) FROM mailbox_check)`)
	}
	before := rows()

	with := func(f func(*MailboxInput)) MailboxInput { in := w.input("new@other.example"); f(&in); return in }
	for _, c := range []struct {
		name     string
		in       MailboxInput
		reserved bool // refuse reserved names, as a real run does
		pub      *secretbox.PublicKey
		want     string
	}{
		{name: "bad address", in: with(func(in *MailboxInput) { in.Address = "not an address" }), want: "is not one mailbox address"},
		{name: "two at signs", in: with(func(in *MailboxInput) { in.Address = "a@b@other.example" }), want: "is not one mailbox address"},
		{name: "reserved", in: with(func(*MailboxInput) {}), reserved: true, want: "other.example is a reserved test name"},
		{name: "duplicate", in: with(func(in *MailboxInput) { in.Address = "ONE@" + testSenderDomain }), want: "one@" + testSenderDomain + " is already mailbox"},
		{name: "third on a domain", in: with(func(in *MailboxInput) { in.Address = "three@mail." + testSenderDomain }),
			want: testSenderDomain + " already has 2 mailboxes, the most leads.yaml allows (caps.mailboxes_per_domain). Mail providers judge reputation by domain, so a new mailbox is safer on another domain."},
		{name: "smtp port", in: with(func(in *MailboxInput) { in.SMTPAddr = "smtp.other.example:25" }), want: "The sending server must be host:port with port 465 or 587."},
		{name: "imap port", in: with(func(in *MailboxInput) { in.IMAPAddr = "imap.other.example:143" }), want: "The reading server must be host:port with port 993."},
		{name: "no smtp", in: with(func(in *MailboxInput) { in.SMTPAddr = "" }), want: "Give the sending server as host:port, or choose a preset."},
		{name: "from name break", in: with(func(in *MailboxInput) { in.FromName = "Sales\r\nBcc: x@other.example" }), want: "The From name cannot hold a line break."},
		{name: "empty password", in: with(func(in *MailboxInput) { in.Password = "" }), want: "The password is empty"},
		{name: "unknown preset", in: with(func(in *MailboxInput) { in.Preset = "Nope" }), want: "There is no preset Nope; the presets are Spacemail, Other."},
		{name: "no public key", in: with(func(*MailboxInput) {}), pub: &secretbox.PublicKey{}, want: secretbox.PublicKeyEnv + " is not set"},
	} {
		t.Run(c.name, func(t *testing.T) {
			leadsMail.allowReserved = !c.reserved
			defer func() { leadsMail.allowReserved = true }()
			pub := w.key.Public()
			if c.pub != nil {
				pub = *c.pub
			}
			_, err := addMailbox(ctx, w.lq, w.cfg, pub, w.dir, w.now, c.in)
			if got := refusalWords(t, err); !strings.Contains(got, c.want) {
				t.Errorf("refusal %q, want %q", got, c.want)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Error("the refusal carries the password")
			}
			if rows() != before {
				t.Error("a refused add wrote rows")
			}
		})
	}
	// A retired mailbox no longer counts toward its domain.
	if _, err := retireMailbox(ctx, w.lq, w.now, 1, true); err != nil {
		t.Fatal(err)
	}
	w.add(t, w.input("three@"+testSenderDomain))
}

// TestEditMailboxCore: a blank password keeps the seal only while login and servers stay;
// a connection change with the password re-seals and requests a check; a From name change
// alone requests none.
func TestEditMailboxCore(t *testing.T) {
	w := newCoreWorld(t)
	ctx := context.Background()
	m := seedMailbox(t, w.lq, "edit@"+testSenderDomain)
	checks := func() int {
		return batchCount(t, `SELECT count(*)::int FROM mailbox_check WHERE outcome = 'requested'`)
	}

	recheck, err := editMailbox(ctx, w.lq, w.key.Public(), w.dir, w.now, m.ID, MailboxInput{FromName: "Desk"})
	if err != nil || recheck || checks() != 0 {
		t.Fatalf("from name edit = %v, %v; %d checks requested", recheck, err, checks())
	}
	got, _ := w.lq.MailboxByID(ctx, m.ID)
	if got.FromName != "Desk" || !bytes.Equal(got.PasswordSealed, m.PasswordSealed) || got.SMTPAddr != m.SMTPAddr {
		t.Errorf("after from name edit %+v", got)
	}

	_, err = editMailbox(ctx, w.lq, w.key.Public(), w.dir, w.now, m.ID, MailboxInput{SMTPAddr: "smtp.evil.example:465", FromName: "Desk"})
	if words := refusalWords(t, err); !strings.Contains(words, "needs the password again") {
		t.Errorf("server change without password: %q", words)
	}
	if got, _ := w.lq.MailboxByID(ctx, m.ID); got.SMTPAddr != m.SMTPAddr {
		t.Error("a refused edit changed the server")
	}
	for _, bad := range []MailboxInput{
		{Address: "other@" + testSenderDomain},
		{IMAPAddr: "imap.x.example:143"},
		{FromName: "a\nb"},
	} {
		if _, err := editMailbox(ctx, w.lq, w.key.Public(), w.dir, w.now, m.ID, bad); !errors.Is(err, console.ErrUserFixable) {
			t.Errorf("edit %+v: %v, want a refusal", bad, err)
		}
	}
	if _, err := editMailbox(ctx, w.lq, w.key.Public(), w.dir, w.now, 999, MailboxInput{}); !strings.Contains(refusalWords(t, err), "No mailbox 999") {
		t.Errorf("missing mailbox: %v", err)
	}

	recheck, err = editMailbox(ctx, w.lq, w.key.Public(), w.dir, w.now, m.ID,
		MailboxInput{SMTPAddr: "smtp.new.example:587", FromName: "Desk", Password: config.Secret(testPassword)})
	if err != nil || !recheck || checks() != 1 || w.requestFile(t) == "" {
		t.Fatalf("server change with password = %v, %v; %d checks", recheck, err, checks())
	}
	got, _ = w.lq.MailboxByID(ctx, m.ID)
	if got.SMTPAddr != "smtp.new.example:587" {
		t.Errorf("smtp = %s", got.SMTPAddr)
	}
	if creds, err := mailboxCreds(ctx, w.lq, w.key, got); err != nil || creds.Password.Reveal() != testPassword {
		t.Errorf("the new seal: %v", err)
	}
	// A selector change requests a check (the DNS step reads it).
	if recheck, err := editMailbox(ctx, w.lq, w.key.Public(), w.dir, w.now, m.ID, MailboxInput{Selector: "s2", FromName: "Desk"}); err != nil || !recheck {
		t.Errorf("selector edit = %v, %v", recheck, err)
	}
}

// TestMailboxPauseCore: pause, pause again (no row), resume; a domain pause pauses only the
// active mailboxes of that domain and its resume leaves a mailbox paused on its own paused.
func TestMailboxPauseCore(t *testing.T) {
	w := newCoreWorld(t)
	ctx := context.Background()
	a := seedMailbox(t, w.lq, "a@"+testSenderDomain)
	b := seedMailbox(t, w.lq, "b@"+testSenderDomain)
	c := seedMailbox(t, w.lq, "c@other.example")

	if sw, err := setMailboxPause(ctx, w.lq, w.now, a.ID, true, " bounces "); err != nil || !sw.Changed {
		t.Fatalf("pause = %+v, %v", sw, err)
	}
	if sw, err := setMailboxPause(ctx, w.lq, w.now, a.ID, true, "again"); err != nil || sw.Changed || sw.Reason != "bounces" {
		t.Errorf("pause again = %+v, %v", sw, err)
	}
	if got := mailboxStateRows(t, a.ID); strings.Join(got, "|") != "paused mailbox operator: bounces" {
		t.Errorf("a's rows %q", got)
	}

	n, err := setDomainPause(ctx, w.lq, w.now, " Sender.Example ", true, "provider block")
	if err != nil || n.Changed != 1 {
		t.Fatalf("domain pause = %+v, %v; want 1 changed", n, err)
	}
	if got := mailboxStateRows(t, b.ID); strings.Join(got, "|") != "paused domain operator: provider block" {
		t.Errorf("b's rows %q", got)
	}
	if got := mailboxStateRows(t, c.ID); len(got) != 0 {
		t.Errorf("another domain's mailbox got %q", got)
	}
	n, err = setDomainPause(ctx, w.lq, w.now, testSenderDomain, false, "")
	if err != nil || n.Changed != 1 || n.Kept != 1 {
		t.Errorf("domain resume = %+v, %v; want 1 changed, 1 kept", n, err)
	}
	if st := w.status(t, a.ID); st.State != leadstore.MailboxPaused {
		t.Errorf("a = %s after the domain resume, want still paused", st.State)
	}
	if st := w.status(t, b.ID); st.State != leadstore.MailboxActive {
		t.Errorf("b = %s, want active", st.State)
	}
	if _, err := setDomainPause(ctx, w.lq, w.now, "nobody.example", true, ""); !strings.Contains(refusalWords(t, err), "No mailbox is on nobody.example") {
		t.Errorf("unknown domain: %v", err)
	}

	if sw, err := setMailboxPause(ctx, w.lq, w.now, a.ID, false, ""); err != nil || !sw.Changed {
		t.Errorf("resume = %+v, %v", sw, err)
	}
	if sw, err := setMailboxPause(ctx, w.lq, w.now, a.ID, false, ""); err != nil || sw.Changed {
		t.Errorf("resume again = %+v, %v", sw, err)
	}
	if _, err := retireMailbox(ctx, w.lq, w.now, c.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := setMailboxPause(ctx, w.lq, w.now, c.ID, false, ""); !strings.Contains(refusalWords(t, err), "is retired") {
		t.Errorf("resume retired: %v", err)
	}
	if _, err := setMailboxPause(ctx, w.lq, w.now, 999, true, ""); !strings.Contains(refusalWords(t, err), "No mailbox 999") {
		t.Errorf("missing: %v", err)
	}
}

// TestRetireMailboxCore: retiring needs confirm; it is final and cancels the mailbox's
// unsent messages with reason "mailbox retired".
func TestRetireMailboxCore(t *testing.T) {
	w := newSendWorld(t, true, 10)
	ctx := context.Background()
	w.batch(t, false, map[int64]string{w.lead(t, "one.example"): "one.example"})
	ownerExec(t, `UPDATE outreach_message SET mailbox_id = $1`, w.mailbox.ID)
	now := leadsNow()

	_, err := retireMailbox(ctx, w.lq, now, w.mailbox.ID, false)
	if words := refusalWords(t, err); !strings.Contains(words, "Confirm to retire it") {
		t.Errorf("without confirm: %q", words)
	}
	if n := w.count(t, `SELECT count(*)::int FROM outreach_message WHERE state = 'cancelled'`); n != 0 {
		t.Fatalf("a refused retire cancelled %d", n)
	}
	n, err := retireMailbox(ctx, w.lq, now, w.mailbox.ID, true)
	if err != nil || n != 3 {
		t.Fatalf("retire = %d, %v; want 3 cancelled", n, err)
	}
	if c := w.count(t, `SELECT count(*)::int FROM outreach_message WHERE state = 'cancelled' AND cancel_reason = 'mailbox retired'`); c != 3 {
		t.Errorf("%d cancelled with reason mailbox retired, want 3", c)
	}
	if got := mailboxStateRows(t, w.mailbox.ID); strings.Join(got, "|") != "retired mailbox operator: " {
		t.Errorf("rows %q", got)
	}
	if n, err := retireMailbox(ctx, w.lq, now, w.mailbox.ID, true); err != nil || n != 0 {
		t.Errorf("retire again = %d, %v", n, err)
	}
	if _, err := requestCheck(ctx, w.lq, w.dir, now, w.mailbox.ID); !strings.Contains(refusalWords(t, err), "is retired") {
		t.Errorf("check retired: %v", err)
	}
}

// TestRequestCheckCore: one waiting check per mailbox, and the request file.
func TestRequestCheckCore(t *testing.T) {
	w := newCoreWorld(t)
	ctx := context.Background()
	m := seedMailbox(t, w.lq, "chk@"+testSenderDomain)
	id1, err := requestCheck(ctx, w.lq, w.dir, w.now, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := requestCheck(ctx, w.lq, w.dir, w.now, m.ID)
	if err != nil || id1 != id2 || w.requestFile(t) == "" {
		t.Errorf("requests = %d, %d, %v", id1, id2, err)
	}
	if _, err := requestCheck(ctx, w.lq, w.dir, w.now, 999); !strings.Contains(refusalWords(t, err), "No mailbox 999") {
		t.Errorf("missing: %v", err)
	}
}

// TestRunChecksCore: every requested check is run with the fakes and filled with ticks and
// the first failing step's fix; a password that cannot be used is did_not_run; a request
// made mid-run is answered in the same run; nothing is sent.
func TestRunChecksCore(t *testing.T) {
	w := newCoreWorld(t)
	ctx := context.Background()
	good := seedMailbox(t, w.lq, "good@"+testSenderDomain)
	badLogin := seedMailbox(t, w.lq, "login@other.example")
	noFolders := seedMailbox(t, w.lq, "folders@third.example")
	noDKIM := seedMailbox(t, w.lq, "dkim@"+testSenderDomain, withSelector("nope"))
	wrongKey := seedMailbox(t, w.lq, "key@fourth.example")
	ownerExec(t, `UPDATE sender_mailbox SET sealed_by = 'another-key' WHERE id = $1`, wrongKey.ID)
	w.mail.failProbe(badLogin.Login, errLoginRefused(), nil)
	w.mail.failProbe(noFolders.Login, nil, mailbox.ErrFoldersMissing)
	leadsMail.resolver = fakeResolver{
		testSenderDomain:                      {"v=spf1 -all"},
		"sel1._domainkey." + testSenderDomain: {"v=DKIM1; p=MIIB"},
		"_dmarc." + testSenderDomain:          {"v=DMARC1; p=none"},
	}
	for _, m := range []leadstore.Mailbox{good, badLogin, noFolders, noDKIM, wrongKey} {
		if _, err := w.lq.RequestCheck(ctx, m.ID, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	// The second SMTP probe (good is finished by then) asks for another check of good, as the
	// console would mid-run.
	probe := leadsMail.probeSMTP
	probes := 0
	leadsMail.probeSMTP = func(ctx context.Context, addr string, user, password config.Secret) error {
		if probes++; probes == 2 {
			if _, err := w.lq.RequestCheck(ctx, good.ID, leadstore.ActorOperator); err != nil {
				t.Error(err)
			}
		}
		return probe(ctx, addr, user, password)
	}

	done, err := runChecks(ctx, w.lq, w.key, w.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 6 {
		t.Errorf("%d checks run, want 6 (5 + the mid-run request): %+v", len(done), done)
	}
	if left, _ := w.lq.RequestedChecks(ctx); len(left) != 0 {
		t.Errorf("%d checks left requested", len(left))
	}
	yes, no := true, false
	for _, c := range []struct {
		m                                    leadstore.Mailbox
		outcome                              string
		smtp, imap, folders, spf, dkim, dmrc *bool
		fix                                  string
	}{
		{good, leadstore.CheckPassed, &yes, &yes, &yes, &yes, &yes, &yes, ""},
		{badLogin, leadstore.CheckFailed, &no, &yes, &yes, &no, &no, &no, mailbox.FixFor(mailbox.StepSMTP, errLoginRefused(), "smtp.seed.invalid")},
		{noFolders, leadstore.CheckFailed, &yes, &yes, &no, &no, &no, &no, "Signed in, but the Sent or Junk folder was not found; replies in Junk would be missed."},
		{noDKIM, leadstore.CheckFailed, &yes, &yes, &yes, &yes, &no, &yes, "Add the DKIM record: a TXT record at nope._domainkey." + testSenderDomain + " starting v=DKIM1, as the mail provider gives it."},
		{wrongKey, leadstore.CheckDidNotRun, nil, nil, nil, nil, nil, nil, "Re-enter the password: it was saved under a different key."},
	} {
		lc := w.status(t, c.m.ID).LastCheck
		if lc == nil || lc.Outcome != c.outcome || lc.Fix != c.fix || lc.CheckedAt == nil {
			t.Errorf("%s: check %+v, want %s %q", c.m.Address, lc, c.outcome, c.fix)
			continue
		}
		for name, p := range map[string][2]*bool{"smtp": {lc.SMTPOK, c.smtp}, "imap": {lc.IMAPOK, c.imap}, "folders": {lc.FoldersOK, c.folders},
			"spf": {lc.SPF, c.spf}, "dkim": {lc.DKIM, c.dkim}, "dmarc": {lc.DMARC, c.dmrc}} {
			if (p[0] == nil) != (p[1] == nil) || (p[0] != nil && *p[0] != *p[1]) {
				t.Errorf("%s: %s = %v, want %v", c.m.Address, name, deref(p[0]), deref(p[1]))
			}
		}
	}
	if st := w.status(t, wrongKey.ID); st.State != leadstore.MailboxPaused {
		t.Errorf("the mailbox with an unusable password is %s, want paused", st.State)
	}
	if calls := w.mail.callsOf("smtp"); len(calls) != 0 {
		t.Errorf("a check sent mail: %+v", calls)
	}
	for _, c := range append(w.mail.callsOf("probeSMTP"), w.mail.callsOf("probeIMAP")...) {
		if !c.passwordOK {
			t.Errorf("a probe got the wrong password: %+v", c)
		}
	}
	if n := batchCount(t, `SELECT count(*)::int FROM sender_check WHERE selector <> ''`); n != 5 {
		t.Errorf("%d sender_check rows, want one per check that ran (5)", n)
	}
	if !w.status(t, good.ID).DNSPassedToday {
		t.Error("a passing check did not make its mailbox's domain pass today")
	}
}

func deref(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestLeadsMailboxesCLI: add (password on stdin, never echoed), list, check, pause, resume,
// retire; every refusal exits 2 with the core's words.
func TestLeadsMailboxesCLI(t *testing.T) {
	w := newCoreWorld(t)
	saved := mailboxPasswordInput
	t.Cleanup(func() { mailboxPasswordInput = saved })
	var outs []string
	cli := func(stdin string, args ...string) (int, string) {
		t.Helper()
		mailboxPasswordInput = stdinFile(t, stdin)
		code, out, errOut := leadsRun(t, append([]string{"leads", "mailboxes"}, args...)...)
		outs = append(outs, out, errOut)
		return code, out + errOut
	}
	must := func(stdin string, args ...string) string {
		t.Helper()
		code, out := cli(stdin, args...)
		if code != 0 {
			t.Fatalf("%q exited %d: %s", args, code, out)
		}
		return out
	}

	out := must(testPassword+"\n", "add", "--address", "Cli@"+testSenderDomain, "--smtp", "smtp."+testSenderDomain+":465",
		"--imap", "imap."+testSenderDomain+":993", "--selector", "sel1", "--from-name", "Desk")
	if !strings.Contains(out, "Mailbox 1 added: cli@"+testSenderDomain) {
		t.Errorf("add said %q", out)
	}
	must(testPassword+"\n", "add", "--address", "two@"+testSenderDomain, "--preset", "Other", "--smtp", "smtp."+testSenderDomain+":587",
		"--imap", "imap."+testSenderDomain+":993")
	code, out := cli(testPassword+"\n", "add", "--address", "three@"+testSenderDomain, "--smtp", "smtp."+testSenderDomain+":465",
		"--imap", "imap."+testSenderDomain+":993")
	if code != 2 || !strings.Contains(out, testSenderDomain+" already has 2 mailboxes, the most leads.yaml allows (caps.mailboxes_per_domain).") {
		t.Errorf("third on the domain = %d %q", code, out)
	}
	if code, out := cli("\n", "add", "--address", "x@other.example", "--preset", "Spacemail"); code != 2 || !strings.Contains(out, "The password is empty") {
		t.Errorf("empty password = %d %q", code, out)
	}

	out = must("", "list")
	for _, want := range []string{"1  cli@" + testSenderDomain, "active", "check requested", "2  two@" + testSenderDomain} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	out = must("", "check", "--requested")
	if !strings.Contains(out, "Mailbox 1: check passed.") || !strings.Contains(out, "Mailbox 2: check passed.") {
		t.Errorf("check --requested said %q", out)
	}
	if out := must("", "check", "--requested"); !strings.Contains(out, "No mailbox check is waiting.") {
		t.Errorf("an empty check said %q", out)
	}
	w.mail.failProbe("two@"+testSenderDomain, errLoginRefused(), nil)
	if out := must("", "check", "2"); !strings.Contains(out, "Mailbox 2: check failed: The server refused the login.") {
		t.Errorf("check 2 said %q", out)
	}
	if out := must("", "list"); !strings.Contains(out, "check failed: The server refused the login.") {
		t.Errorf("list after a failed check:\n%s", out)
	}

	if out := must("", "pause", "1", "--reason", "testing"); !strings.Contains(out, "Mailbox 1 is paused") {
		t.Errorf("pause said %q", out)
	}
	if out := must("", "pause", "1"); !strings.Contains(out, "Mailbox 1 is already paused") {
		t.Errorf("pause again said %q", out)
	}
	if out := must("", "resume", "1"); !strings.Contains(out, "Mailbox 1 is resumed") {
		t.Errorf("resume said %q", out)
	}
	if out := must("", "pause", "--domain", testSenderDomain, "--reason", "block"); !strings.Contains(out, "2 mailboxes on "+testSenderDomain+" are paused") {
		t.Errorf("domain pause said %q", out)
	}
	if out := must("", "resume", "--domain", testSenderDomain); !strings.Contains(out, "2 mailboxes on "+testSenderDomain+" are resumed") {
		t.Errorf("domain resume said %q", out)
	}
	if code, out := cli("", "retire", "2"); code != 2 || !strings.Contains(out, "Confirm to retire it") {
		t.Errorf("retire without confirm = %d %q", code, out)
	}
	if out := must("", "retire", "2", "--confirm"); !strings.Contains(out, "Mailbox 2 is retired") {
		t.Errorf("retire said %q", out)
	}
	if code, out := cli("", "pause", "9"); code != 2 || !strings.Contains(out, "No mailbox 9.") {
		t.Errorf("pause 9 = %d %q", code, out)
	}
	if out := must("", "list"); !strings.Contains(out, "retired") {
		t.Errorf("list after retire:\n%s", out)
	}

	for _, o := range outs {
		if strings.Contains(o, testPassword) {
			t.Fatalf("a command printed the password:\n%s", o)
		}
	}
}

// TestMailboxesCheckNeedsKey: check without a key exits 2 with the key's plain words and
// leaves the request waiting.
func TestMailboxesCheckNeedsKey(t *testing.T) {
	w := newCoreWorld(t)
	m := seedMailbox(t, w.lq, "k@"+testSenderDomain)
	if _, err := w.lq.RequestCheck(context.Background(), m.ID, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secretbox.KeyFileEnv, "")
	code, _, errOut := leadsRun(t, "leads", "mailboxes", "check", "--requested")
	if code != 2 || !strings.Contains(errOut, "needs the mailbox key") {
		t.Errorf("check without a key = %d %q", code, errOut)
	}
	if left, _ := w.lq.RequestedChecks(context.Background()); len(left) != 1 {
		t.Errorf("%d requested, want 1 left waiting", len(left))
	}
}
