//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The mailbox test world: a test key, sealed mailboxes and fakes keyed per mailbox, shared by
// every test that sends, syncs or checks through a sending mailbox.

var (
	testKeysMu sync.Mutex
	// testKeys holds every key testKey wrote, by its file, so seedMailbox seals with the
	// key the running test set (and never with a key file named outside the tests).
	testKeys = map[string]secretbox.PrivateKey{}
)

// testKey writes a new private key, mode 0600, under t.TempDir() and points
// secretbox.KeyFileEnv at it for the test.
func testKey(t *testing.T) secretbox.PrivateKey {
	t.Helper()
	_, priv, err := secretbox.GenerateKeyPair(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mailbox.key")
	if err := secretbox.WritePrivateKey(path, priv); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secretbox.KeyFileEnv, path)
	testKeysMu.Lock()
	testKeys[path] = priv
	testKeysMu.Unlock()
	return priv
}

// currentTestKey is the key testKey set for this test, or a new one.
func currentTestKey(t *testing.T) secretbox.PrivateKey {
	t.Helper()
	testKeysMu.Lock()
	k, ok := testKeys[getenvKeyFile()]
	testKeysMu.Unlock()
	if ok {
		return k
	}
	return testKey(t)
}

func getenvKeyFile() string { return strings.TrimSpace(os.Getenv(secretbox.KeyFileEnv)) }

// mailboxSeed is what seedMailbox's options change.
type mailboxSeed struct {
	selector, fromName string
	fromNameSet        bool
	dnsPass            bool
	unassigned         bool
}

type mailboxOpt func(*mailboxSeed)

// withDNSPass records today's full DNS pass for the mailbox's host and selector, stamped
// with leadsNow() so a test that fixes the clock sees it as today.
func withDNSPass() mailboxOpt { return func(s *mailboxSeed) { s.dnsPass = true } }

// unassigned leaves the mailbox assigned to no campaign (by default seedMailbox assigns it
// to the default campaign, so a live batch and send of it may use it).
func unassigned() mailboxOpt { return func(s *mailboxSeed) { s.unassigned = true } }

// withSelector sets the mailbox's DKIM selector (default sel1, as examples/leads.yaml).
func withSelector(sel string) mailboxOpt { return func(s *mailboxSeed) { s.selector = sel } }

// withFromName sets the mailbox's From name (default empty: leads.yaml sender.name).
func withFromName(n string) mailboxOpt {
	return func(s *mailboxSeed) { s.fromName, s.fromNameSet = n, true }
}

// seedMailbox adds a sending mailbox for address (a reserved name) with its first check
// passed, logging in as address with testPassword sealed by the test key (currentTestKey).
// Its servers are leadstoretest.SeedMailbox's, on .invalid; the fakes are keyed by login,
// so no server name is ever dialled. It records a DNS pass only with withDNSPass(), and
// assigns the mailbox to the default campaign unless unassigned().
func seedMailbox(t *testing.T, lq leadstore.Queries, address string, opts ...mailboxOpt) leadstore.Mailbox {
	t.Helper()
	s := mailboxSeed{selector: "sel1"}
	for _, o := range opts {
		o(&s)
	}
	ctx := context.Background()
	key := currentTestKey(t)
	m := leadstoretest.SeedMailbox(t, lq, address)
	sealed, err := secretbox.Seal(key.Public(), secretbox.Credentials{
		Address: m.Address, Login: m.Login, SMTPAddr: m.SMTPAddr, IMAPAddr: m.IMAPAddr,
		Password: config.Secret(testPassword),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	edit := leadstore.MailboxEdit{DKIMSelector: &s.selector, PasswordSealed: sealed, SealedBy: secretbox.Fingerprint(key.Public())}
	if s.fromNameSet {
		edit.FromName = &s.fromName
	}
	if err := lq.UpdateMailbox(ctx, m.ID, edit); err != nil {
		t.Fatalf("seed mailbox %s: %v", address, err)
	}
	if m, err = lq.MailboxByID(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if !s.unassigned {
		if _, err := lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, m.ID, true, leadstore.ActorGremy); err != nil {
			t.Fatalf("seed mailbox %s: assign: %v", address, err)
		}
	}
	if s.dnsPass {
		if err := lq.RecordSenderCheckSelector(ctx, leadstore.SenderDomainSHA256(m.Host()), m.DKIMSelector,
			mailbox.DNSResult{SPF: true, DKIM: true, DMARC: true}, leadsNow()); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// mailboxCall is one call of a per-mailbox dependency.
type mailboxCall struct {
	dep, addr, login string
	passwordOK       bool
}

// mailboxFakes are the per-mailbox outside world: one fakeSMTP and one fakeBox per login
// (a seeded mailbox logs in as its address), made on first use. A password other than
// testPassword is refused as a real server refuses it.
type mailboxFakes struct {
	mu       sync.Mutex
	smtp     map[string]*fakeSMTP
	boxes    map[string]*fakeBox
	smtpErr  map[string]error // probeSMTP's answer, by login
	imapErr  map[string]error // probeIMAP's answer, by login
	calls    []mailboxCall
	refusals int
}

func newMailboxFakes() *mailboxFakes {
	return &mailboxFakes{smtp: map[string]*fakeSMTP{}, boxes: map[string]*fakeBox{}, smtpErr: map[string]error{}, imapErr: map[string]error{}}
}

// install points d's per-mailbox dependencies at f.
func (f *mailboxFakes) install(d *mailDeps) {
	d.smtpFor = f.smtpFor
	d.readerFor = f.readerFor
	d.probeSMTP = f.probeSMTP
	d.probeIMAP = f.probeIMAP
}

// use makes login's fakes smtp and box (sendWorld hands over its own, so a test that reads
// w.smtp or w.box sees the mail of its seeded mailbox).
func (f *mailboxFakes) use(login string, smtp *fakeSMTP, box *fakeBox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.smtp[strings.ToLower(login)], f.boxes[strings.ToLower(login)] = smtp, box
}

// smtpOf is login's fake SMTP server.
func (f *mailboxFakes) smtpOf(login string) *fakeSMTP {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.smtpLocked(strings.ToLower(login))
}

func (f *mailboxFakes) smtpLocked(login string) *fakeSMTP {
	s, ok := f.smtp[login]
	if !ok {
		s = &fakeSMTP{}
		f.smtp[login] = s
	}
	return s
}

// boxOf is login's fake IMAP mailbox.
func (f *mailboxFakes) boxOf(login string) *fakeBox {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boxLocked(strings.ToLower(login))
}

func (f *mailboxFakes) boxLocked(login string) *fakeBox {
	b, ok := f.boxes[login]
	if !ok {
		b = newFakeBox()
		f.boxes[login] = b
	}
	return b
}

// failProbe makes login's next checks fail: smtpErr from probeSMTP, imapErr from probeIMAP
// (nil leaves that step passing).
func (f *mailboxFakes) failProbe(login string, smtpErr, imapErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.smtpErr[strings.ToLower(login)], f.imapErr[strings.ToLower(login)] = smtpErr, imapErr
}

// callsOf lists the calls made of dep ("smtp", "reader", "probeSMTP", "probeIMAP").
func (f *mailboxFakes) callsOf(dep string) []mailboxCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mailboxCall
	for _, c := range f.calls {
		if c.dep == dep {
			out = append(out, c)
		}
	}
	return out
}

// record notes a call and says the login and whether its password is the test one.
func (f *mailboxFakes) record(dep, addr string, user, password config.Secret) (string, bool) {
	login := strings.ToLower(user.Reveal())
	ok := password.Reveal() == testPassword
	f.calls = append(f.calls, mailboxCall{dep: dep, addr: addr, login: login, passwordOK: ok})
	if !ok {
		f.refusals++
	}
	return login, ok
}

// errLoginRefused is a refused SMTP login, as mailbox.SMTPSender reports one.
func errLoginRefused() error {
	return &mailbox.SendError{Kind: mailbox.Account, Stage: "auth", Code: 535, Text: "authentication failed"}
}

// refusingSMTP refuses every message with err.
type refusingSMTP struct{ err error }

func (r refusingSMTP) Send(context.Context, string, string, []byte) (string, error) { return "", r.err }

func (f *mailboxFakes) smtpFor(addr string, user, password config.Secret) mailSender {
	f.mu.Lock()
	defer f.mu.Unlock()
	login, ok := f.record("smtp", addr, user, password)
	if !ok {
		return refusingSMTP{errLoginRefused()}
	}
	return f.smtpLocked(login)
}

func (f *mailboxFakes) readerFor(ctx context.Context, addr string, user, password config.Secret) (mailbox.Reader, error) {
	f.mu.Lock()
	login, ok := f.record("reader", addr, user, password)
	box := f.boxLocked(login)
	f.mu.Unlock()
	if !ok {
		return nil, &mailbox.IMAPError{Stage: "login", Text: "authentication failed"}
	}
	return box.open(ctx)
}

func (f *mailboxFakes) probeSMTP(_ context.Context, addr string, user, password config.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	login, ok := f.record("probeSMTP", addr, user, password)
	if !ok {
		return errLoginRefused()
	}
	return f.smtpErr[login]
}

func (f *mailboxFakes) probeIMAP(ctx context.Context, addr string, user, password config.Secret) (mailbox.Mailboxes, error) {
	f.mu.Lock()
	login, ok := f.record("probeIMAP", addr, user, password)
	box, err := f.boxLocked(login), f.imapErr[login]
	f.mu.Unlock()
	if !ok {
		return mailbox.Mailboxes{}, &mailbox.IMAPError{Stage: "login", Text: "authentication failed"}
	}
	if err != nil {
		return mailbox.Mailboxes{}, err
	}
	r, err := box.open(ctx)
	if err != nil {
		return mailbox.Mailboxes{}, err
	}
	defer func() { _ = r.Close() }()
	return r.ListMailboxes()
}

// mailboxStateRows lists mailbox id's state history as "state scope actor: reason".
func mailboxStateRows(t *testing.T, id int64) []string {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT format('%s %s %s: %s', state, scope, actor, reason) FROM mailbox_state WHERE mailbox_id = $1 ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
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
	return out
}

// TestMailboxCreds: a seal that matches the mailbox opens to the real password and leaves
// the mailbox active; a changed server, another key or a broken seal pauses the mailbox
// (system, a plain reason) and returns a plain error that never carries the password.
func TestMailboxCreds(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		break_ func(t *testing.T, m leadstore.Mailbox) // nil: the seal matches
		other  bool                                    // open with another key
		reason string
	}{
		{name: "match"},
		{name: "changed smtp_addr", break_: func(t *testing.T, m leadstore.Mailbox) {
			ownerExec(t, `UPDATE sender_mailbox SET smtp_addr = 'smtp.evil.invalid:465' WHERE id = $1`, m.ID)
		}, reason: "re-enter the password: the mailbox's settings changed since it was saved"},
		{name: "changed login", break_: func(t *testing.T, m leadstore.Mailbox) {
			ownerExec(t, `UPDATE sender_mailbox SET login = 'someone@evil.invalid' WHERE id = $1`, m.ID)
		}, reason: "re-enter the password: the mailbox's settings changed since it was saved"},
		{name: "wrong key", other: true, reason: "re-enter the password: it was saved under a different key"},
		{name: "broken seal", break_: func(t *testing.T, m leadstore.Mailbox) {
			ownerExec(t, `UPDATE sender_mailbox SET password_sealed = '\x00010203'::bytea WHERE id = $1`, m.ID)
		}, reason: "re-enter the password: it could not be opened"},
	} {
		t.Run(c.name, func(t *testing.T) {
			leadsDB(t)
			lq := leadstoretest.Q(t)
			key := testKey(t)
			m := seedMailbox(t, lq, "creds@"+testSenderDomain)
			if c.break_ != nil {
				c.break_(t, m)
			}
			if c.other {
				_, other, err := secretbox.GenerateKeyPair(nil)
				if err != nil {
					t.Fatal(err)
				}
				key = other
			}
			m, err := lq.MailboxByID(ctx, m.ID)
			if err != nil {
				t.Fatal(err)
			}
			creds, err := mailboxCreds(ctx, lq, key, m)
			states := mailboxStateRows(t, m.ID)
			if c.reason == "" {
				if err != nil {
					t.Fatalf("mailboxCreds: %v", err)
				}
				if creds.Password.Reveal() != testPassword || creds.Login != m.Login || creds.SMTPAddr != m.SMTPAddr {
					t.Errorf("creds = %+v, want the seeded mailbox's", creds)
				}
				if len(states) != 0 {
					t.Errorf("state rows %q, want none", states)
				}
				return
			}
			if err == nil {
				t.Fatal("mailboxCreds succeeded")
			}
			if !errors.Is(err, errMailboxCreds) || !strings.Contains(err.Error(), c.reason) {
				t.Errorf("error %q, want errMailboxCreds with %q", err, c.reason)
			}
			if strings.Contains(err.Error(), testPassword) || creds.Password.Reveal() != "" {
				t.Error("the password leaked on a refusal")
			}
			if want := []string{"paused mailbox system: " + c.reason}; strings.Join(states, "|") != strings.Join(want, "|") {
				t.Errorf("state rows %q, want %q", states, want)
			}
		})
	}
}

func ownerExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// TestCheckSenderFor: the check reads the mailbox's own selector and records it, so its
// mailbox's DNSPassedToday turns true; a failing selector records a row that never counts.
func TestCheckSenderFor(t *testing.T) {
	leadsDB(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	saved := leadsMail
	t.Cleanup(func() { leadsMail = saved })
	leadsMail.resolver = passingDNS()
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	m := seedMailbox(t, lq, "dns@"+testSenderDomain)

	res, err := checkSenderFor(ctx, lq, testSenderDomain, "other", now)
	if err != nil || res.DKIM || !res.SPF || !res.DMARC {
		t.Fatalf("check with a missing selector = %+v, %v", res, err)
	}
	if ok := dnsPassedToday(t, lq, m.ID, now); ok {
		t.Error("a failing check made the mailbox's DNS pass today")
	}
	res, err = checkSenderFor(ctx, lq, testSenderDomain, "sel1", now)
	if err != nil || !res.OK() {
		t.Fatalf("check = %+v, %v", res, err)
	}
	if !dnsPassedToday(t, lq, m.ID, now) {
		t.Error("a passing check of the mailbox's selector did not count")
	}
	if n := batchCount(t, `SELECT count(*)::int FROM sender_check WHERE selector IN ('sel1', 'other')`); n != 2 {
		t.Errorf("%d sender_check rows with a selector, want 2", n)
	}
}

func dnsPassedToday(t *testing.T, lq leadstore.Queries, id int64, since time.Time) bool {
	t.Helper()
	ms, err := lq.Mailboxes(context.Background(), leadstore.DefaultAccountID, since.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			return m.DNSPassedToday
		}
	}
	t.Fatalf("mailbox %d not listed", id)
	return false
}

// TestSeedMailboxWorld: a seeded mailbox is sealed with the test key, its first check
// passed, and holds a DNS pass for today only with withDNSPass.
func TestSeedMailboxWorld(t *testing.T) {
	leadsDB(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	saved := leadsNow
	t.Cleanup(func() { leadsNow = saved })
	day := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	leadsNow = func() time.Time { return day }

	key := testKey(t)
	plain := seedMailbox(t, lq, "plain@"+testSenderDomain)
	if n := batchCount(t, `SELECT count(*)::int FROM sender_check`); n != 0 {
		t.Errorf("a seed without withDNSPass wrote %d sender_check rows", n)
	}
	ready := seedMailbox(t, lq, "ready@other.example", withDNSPass(), withSelector("s2"), withFromName("Sales Desk"))
	if again := currentTestKey(t); again != key {
		t.Error("currentTestKey is not the key testKey set")
	}
	if loaded, err := secretbox.LoadPrivateKey(os.Getenv); err != nil || loaded != key {
		t.Errorf("the key file does not hold the test key: %v", err)
	}
	ms, err := lq.Mailboxes(ctx, leadstore.DefaultAccountID, day.Add(-time.Hour))
	if err != nil || len(ms) != 2 {
		t.Fatalf("mailboxes = %d, %v", len(ms), err)
	}
	for _, m := range ms {
		if m.State != leadstore.MailboxActive || m.LastCheck == nil || m.LastCheck.Outcome != leadstore.CheckPassed {
			t.Errorf("%s: state %s, check %+v", m.Address, m.State, m.LastCheck)
		}
		if _, err := mailboxCreds(ctx, lq, key, m.Mailbox); err != nil {
			t.Errorf("%s: %v", m.Address, err)
		}
		switch m.ID {
		case plain.ID:
			if m.DNSPassedToday || m.DKIMSelector != "sel1" || m.FromName != "" {
				t.Errorf("plain: %+v", m)
			}
		case ready.ID:
			if !m.DNSPassedToday || m.DKIMSelector != "s2" || m.FromName != "Sales Desk" {
				t.Errorf("ready: %+v", m)
			}
		}
	}
}

// TestMailboxFakesPerAddress: each login reaches its own fakes; a wrong password is refused
// by every dependency; failProbe fails the named step only.
func TestMailboxFakesPerAddress(t *testing.T) {
	ctx := context.Background()
	f := newMailboxFakes()
	var d mailDeps
	f.install(&d)
	a, b := config.Secret("a@"+testSenderDomain), config.Secret("b@other.example")
	pw, bad := config.Secret(testPassword), config.Secret(testPassword+"-wrong")

	if _, err := d.smtpFor("smtp.a.invalid:465", a, pw).Send(ctx, a.Reveal(), "x@lead.example", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if f.smtpOf(a.Reveal()).count() != 1 || f.smtpOf(b.Reveal()).count() != 0 {
		t.Error("a's message did not land in a's fake alone")
	}
	if _, err := d.smtpFor("smtp.a.invalid:465", a, bad).Send(ctx, a.Reveal(), "x@lead.example", nil); !errors.As(err, new(*mailbox.SendError)) {
		t.Errorf("wrong password: %v", err)
	}
	f.boxOf(b.Reveal()).deliver("INBOX", "Subject: re\r\n\r\n", "thanks")
	r, err := d.readerFor(ctx, "imap.b.invalid:993", b, pw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	if uids, _ := r.SearchSince(0); len(uids) != 1 {
		t.Errorf("b's inbox holds %d messages, want 1", len(uids))
	}
	if _, err := d.readerFor(ctx, "imap.b.invalid:993", b, bad); err == nil {
		t.Error("reader opened with a wrong password")
	}
	if err := d.probeSMTP(ctx, "smtp.a.invalid:465", a, pw); err != nil {
		t.Errorf("probe SMTP: %v", err)
	}
	if boxes, err := d.probeIMAP(ctx, "imap.a.invalid:993", a, pw); err != nil || boxes.Sent == "" || boxes.Junk == "" {
		t.Errorf("probe IMAP = %+v, %v", boxes, err)
	}
	f.failProbe(a.Reveal(), nil, mailbox.ErrFoldersMissing)
	if err := d.probeSMTP(ctx, "smtp.a.invalid:465", a, pw); err != nil {
		t.Errorf("probe SMTP after an IMAP failure: %v", err)
	}
	if _, err := d.probeIMAP(ctx, "imap.a.invalid:993", a, pw); !errors.Is(err, mailbox.ErrFoldersMissing) {
		t.Errorf("probe IMAP: %v", err)
	}
	if err := d.probeSMTP(ctx, "smtp.b.invalid:465", b, bad); err == nil {
		t.Error("probe SMTP passed a wrong password")
	}
	if calls := f.callsOf("smtp"); len(calls) != 2 || !calls[0].passwordOK || calls[1].passwordOK || calls[0].addr != "smtp.a.invalid:465" {
		t.Errorf("smtp calls %+v", calls)
	}
	one, two := &fakeSMTP{}, newFakeBox()
	f.use("C@"+testSenderDomain, one, two)
	if f.smtpOf("c@"+testSenderDomain) != one || f.boxOf("c@"+testSenderDomain) != two {
		t.Error("use did not hand over the given fakes")
	}
}

// TestProductionMailboxDeps: the production per-mailbox dependencies are set and build the
// real clients from the mailbox's own settings.
func TestProductionMailboxDeps(t *testing.T) {
	d := leadsMail
	if d.smtpFor == nil || d.readerFor == nil || d.probeSMTP == nil || d.probeIMAP == nil {
		t.Fatal("a per-mailbox dependency is unset")
	}
	s, ok := d.smtpFor("smtp.prod.invalid:465", config.Secret("u@prod.invalid"), config.Secret(testPassword)).(*mailbox.SMTPSender)
	if !ok || s.Addr != "smtp.prod.invalid:465" || s.User.Reveal() != "u@prod.invalid" || s.Password.Reveal() != testPassword {
		t.Errorf("smtpFor = %#v", s)
	}
}
