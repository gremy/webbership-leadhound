//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The outreach mailbox the tests send from, and its secrets; built at runtime.
var (
	testSenderDomain = "sender" + ".example"
	testUser         = "outreach" + "@" + testSenderDomain
	testPassword     = "pw-" + "Zq81-not-a-real-one"
	testOperator     = "operator" + "@" + testSenderDomain
)

// sendClock is the fake clock: leadsNow reads it and the spacing sleep moves it on.
type sendClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *sendClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *sendClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }
func (c *sendClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return nil
}

// fakeBox is the outreach mailbox: INBOX and Junk, each with its UIDVALIDITY and messages.
type fakeBox struct {
	mu       sync.Mutex
	dialErr  error
	validity map[string]uint32
	msgs     map[string][]mailbox.Fetched
	byDate   int
	appended int
	opens    int
}

func newFakeBox() *fakeBox {
	return &fakeBox{validity: map[string]uint32{"INBOX": 7, "Junk": 3}, msgs: map[string][]mailbox.Fetched{}}
}

func (b *fakeBox) open(context.Context, leadconfig.Env) (mailbox.Reader, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens++
	if b.dialErr != nil {
		return nil, b.dialErr
	}
	return &fakeReader{b: b}, nil
}

// deliver puts a message in name with the next UID.
func (b *fakeBox) deliver(name, header, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	uid := uint32(len(b.msgs[name]) + 1)
	b.msgs[name] = append(b.msgs[name], mailbox.Fetched{UID: uid, Header: []byte(header), FirstText: []byte(text)})
}

type fakeReader struct {
	b   *fakeBox
	box string
}

func (r *fakeReader) ListMailboxes() (mailbox.Mailboxes, error) {
	return mailbox.Mailboxes{Inbox: "INBOX", Sent: "Sent", Junk: "Junk"}, nil
}

func (r *fakeReader) Select(name string) (uint32, error) {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	r.box = name
	return r.b.validity[name], nil
}

func (r *fakeReader) SearchSince(after uint32) ([]uint32, error) {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	var out []uint32
	for _, m := range r.b.msgs[r.box] {
		if m.UID > after {
			out = append(out, m.UID)
		}
	}
	return out, nil
}

func (r *fakeReader) SearchSinceDate(time.Time) ([]uint32, error) {
	r.b.mu.Lock()
	r.b.byDate++
	r.b.mu.Unlock()
	return r.SearchSince(0)
}

func (r *fakeReader) FetchHeaders(uids []uint32) ([]mailbox.Fetched, error) {
	r.b.mu.Lock()
	defer r.b.mu.Unlock()
	var out []mailbox.Fetched
	for _, m := range r.b.msgs[r.box] {
		if slices.Contains(uids, m.UID) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r *fakeReader) AppendSent([]byte) error {
	r.b.mu.Lock()
	r.b.appended++
	r.b.mu.Unlock()
	return nil
}

func (r *fakeReader) Close() error { return nil }

type fakeMail struct {
	from, to string
	raw      []byte
}

// fakeSMTP records every message; errs fails the n-th call (from 0); onSend runs after a
// call is recorded; block, when set, holds every call until it is closed.
type fakeSMTP struct {
	mu      sync.Mutex
	sent    []fakeMail
	errs    map[int]error
	onSend  func(n int, to string)
	block   chan struct{}
	entered chan struct{}
}

func (f *fakeSMTP) Send(_ context.Context, from, to string, raw []byte) (string, error) {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	n := len(f.sent)
	f.sent = append(f.sent, fakeMail{from, to, raw})
	err := f.errs[n]
	hook := f.onSend
	f.mu.Unlock()
	if hook != nil {
		hook(n, to)
	}
	if err != nil {
		return "", err
	}
	return "250 2.0.0 queued", nil
}

func (f *fakeSMTP) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

func (f *fakeSMTP) recipients() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.sent {
		out = append(out, m.to)
	}
	return out
}

// fakeResolver answers TXT lookups from a map; a missing name is not found.
type fakeResolver map[string][]string

func (r fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := r[name]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("lookup %s: not found", name)
}

func passingDNS() fakeResolver {
	return fakeResolver{
		testSenderDomain: {"v=spf1 include:_spf." + testSenderDomain + " -all"},
		"spacemail._domainkey." + testSenderDomain: {"v=DKIM1; k=rsa; p=MIIB"},
		"_dmarc." + testSenderDomain:               {"v=DMARC1; p=none"},
	}
}

// sendWorld is a reset database, a scratch data dir and every fake the send commands use.
type sendWorld struct {
	dir   string
	clock *sendClock
	box   *fakeBox
	smtp  *fakeSMTP
	dns   fakeResolver
	lq    leadstore.Queries
	outs  []string // every command's output, for the secrets sweep
}

// newSendWorld sets the outreach env (live or shadow), a cap of capPerDay from 1 Oct 2026,
// and the fakes; the clock starts on Tuesday 27 Oct 2026, 10:00 London.
func newSendWorld(t *testing.T, live bool, capPerDay int) *sendWorld {
	t.Helper()
	w := &sendWorld{dir: leadsDB(t), clock: &sendClock{t: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)},
		box: newFakeBox(), smtp: &fakeSMTP{}, dns: passingDNS(), lq: leadstoretest.Q(t)}
	if live {
		t.Setenv("LEADHOUND_OUTREACH", "live")
	} else {
		t.Setenv("LEADHOUND_OUTREACH", "")
	}
	t.Setenv("LEADHOUND_OUTREACH_USER", testUser)
	t.Setenv("LEADHOUND_OUTREACH_PASSWORD", testPassword)
	t.Setenv("LEADHOUND_SMTP_ADDR", "smtp."+testSenderDomain+":465")
	t.Setenv("LEADHOUND_IMAP_ADDR", "imap."+testSenderDomain+":993")
	t.Setenv("LEADHOUND_TZ", "Europe/London")
	w.editConfig(t, `start_date: 2026-11-16`, `start_date: 2026-10-01`)
	w.editConfig(t, `ramp: \[.*\]`, fmt.Sprintf("ramp: [{from_day: 0, per_day: %d}]", capPerDay))

	savedMail, savedNow := leadsMail, leadsNow
	t.Cleanup(func() { leadsMail, leadsNow = savedMail, savedNow })
	leadsNow = w.clock.now
	leadsMail = mailDeps{
		resolver:      w.dns,
		openReader:    w.box.open,
		smtp:          func(leadconfig.Env) mailSender { return w.smtp },
		sleep:         w.clock.sleep,
		rand:          savedMail.rand,
		syncWait:      200 * time.Millisecond,
		lockPoll:      20 * time.Millisecond,
		allowReserved: true,
	}
	return w
}

func (w *sendWorld) editConfig(t *testing.T, re, repl string) {
	t.Helper()
	p := filepath.Join(w.dir, "leads.yaml")
	b, err := os.ReadFile(p) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	out := regexp.MustCompile(re).ReplaceAll(b, []byte(repl))
	if bytes.Equal(out, b) {
		t.Fatalf("leads.yaml has no %q", re)
	}
	if err := os.WriteFile(p, out, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

// run runs one command, keeping its output for the sweep.
func (w *sendWorld) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	code, out, errOut := leadsRun(t, args...)
	w.outs = append(w.outs, out, errOut)
	return code, out + errOut
}

func (w *sendWorld) mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out := w.run(t, args...)
	if code != 0 {
		t.Fatalf("%q exited %d:\n%s", args, code, out)
	}
	return out
}

// lead writes a GB site on domain with a Drupal 7 probe, its org, an info@ email route and
// its lead; it returns the lead id.
func (w *sendWorld) lead(t *testing.T, domain string) int64 {
	t.Helper()
	return w.leadIn(t, domain, "GB")
}

// leadIn is lead in another country.
func (w *sendWorld) leadIn(t *testing.T, domain, country string) int64 {
	t.Helper()
	ctx := context.Background()
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-sites-probe", "sites-fixture")
	if err != nil {
		t.Fatal(err)
	}
	fid, err := core.InsertFetch(ctx, runID, "sites-fixture", fetch.Record{Kind: fetch.KindSite, URL: "https://" + domain + "/", Status: 200, Body: []byte("<html></html>"), ContentType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{{Domain: domain, Country: country}}); err != nil {
		t.Fatal(err)
	}
	siteID, err := w.lq.SiteIDByDomain(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	probeID, err := w.lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: 7,
		Signals: []leadstore.Signal{{Signal: "meta-generator", Value: "7", FetchID: fid}}, FetchIDs: []int64{fid}, DerivedBy: "drupal@1"})
	if err != nil {
		t.Fatal(err)
	}
	orgID, _, err := w.lq.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteEmail, Value: "info@" + domain, FetchID: fid, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
	routes, err := w.lq.RoutesFor(ctx, orgID)
	if err != nil || len(routes) == 0 {
		t.Fatalf("routes: %v", err)
	}
	id, _, err := w.lq.UpsertLead(ctx, leadstore.LeadRow{OrgID: orgID, Kind: leadstore.LeadUnsupported, ProbeID: &probeID, Channel: leadstore.ChannelEmail, RouteID: &routes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// batch drafts the three-email sequence for each lead (to info@<domain>) in one batch and
// approves it: first emails due now, follow-ups unscheduled, as batch approve leaves them.
func (w *sendWorld) batch(t *testing.T, shadow bool, leads map[int64]string) int64 {
	t.Helper()
	ctx := context.Background()
	var drafts []leadstore.DraftRow
	for id, domain := range leads {
		first := ""
		for step := range 3 {
			d := leadstore.DraftRow{LeadID: id, Step: step, Channel: "email", To: "info@" + domain,
				Subject: fmt.Sprintf("Your Drupal site, note %d", step+1), Body: fmt.Sprintf("Hello %s, note %d.\n", domain, step+1),
				MessageID: fmt.Sprintf("b%dl%ds%d@%s", len(drafts), id, step, testSenderDomain)}
			if step == 0 {
				first = d.MessageID
			} else {
				d.InReplyTo = first
			}
			drafts = append(drafts, d)
		}
	}
	var batchID int64
	pool := storetest.AppConn(t)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	q := leadstore.NewQueries(tx)
	batchID, sum, err := q.CreateBatch(ctx, shadow, drafts, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := w.clock.now()
	if _, err := q.ApproveBatch(ctx, batchID, hex.EncodeToString(sum), nil, func(m leadstore.Message) time.Time {
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
	return batchID
}

func (w *sendWorld) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (w *sendWorld) str(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func (w *sendWorld) leadState(t *testing.T, domain string) string {
	t.Helper()
	return w.str(t, `SELECT l.state FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = $1`, domain)
}

func (w *sendWorld) msgState(t *testing.T, domain string, step int) string {
	t.Helper()
	return w.str(t, `SELECT m.state FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = $1 AND m.step = $2 ORDER BY m.id DESC LIMIT 1`, domain, step)
}

func (w *sendWorld) lastNote(t *testing.T, command string) string {
	t.Helper()
	return w.str(t, `SELECT coalesce(note, '') FROM run WHERE command = $1 ORDER BY id DESC LIMIT 1`, command)
}

// sweep fails on any secret or recipient address in the outputs and run notes.
func (w *sendWorld) sweep(t *testing.T, extra ...string) {
	t.Helper()
	notes := ""
	rows, err := storetest.OwnerConn(t).Query(context.Background(), `SELECT coalesce(note, '') FROM run`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		notes += n + "\n"
	}
	rows.Close()
	all := strings.Join(w.outs, "\n")
	for _, secret := range append([]string{testUser, testPassword, testOperator}, extra...) {
		if strings.Contains(notes, secret) {
			t.Errorf("a run note holds %q", secret)
		}
		if strings.Contains(all, secret) {
			t.Errorf("command output holds %q", secret)
		}
	}
	if strings.Contains(notes, "@") {
		t.Errorf("a run note holds an address:\n%s", notes)
	}
}

func replyHeader(fromDomain, inReplyTo, subject string) string {
	h := "From: Office <office@" + fromDomain + ">\r\nTo: " + testUser + "\r\nSubject: " + subject + "\r\nMessage-ID: <r" +
		fmt.Sprint(time.Now().UnixNano()) + "@" + fromDomain + ">\r\n"
	if inReplyTo != "" {
		h += "In-Reply-To: <" + inReplyTo + ">\r\nReferences: <" + inReplyTo + ">\r\n"
	}
	return h + "\r\n"
}

// firstMessageID is the Message-ID of domain's first email.
func (w *sendWorld) firstMessageID(t *testing.T, domain string) string {
	t.Helper()
	return w.str(t, `SELECT m.message_id FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = $1 AND m.step = 0`, domain)
}

// The live loop end to end: two first emails, their follow-ups scheduled, and a reply that
// arrives between two sends of the next run is honoured (the 10-minute re-sync rule).
func TestSendLiveResyncHonoursReply(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.editConfig(t, `spacing_minutes: \[3, 9\]`, `spacing_minutes: [11, 11]`)
	a, b := "alpha.example", "bravo.example"
	w.batch(t, false, map[int64]string{w.lead(t, a): a, w.lead(t, b): b})

	out := w.mustRun(t, "leads", "send")
	wantLines(t, "first send", out, "Mailbox synced: nothing new.", "Sent 2: ")
	if w.smtp.count() != 2 || w.box.appended != 2 {
		t.Fatalf("first run: %d sent, %d appended to Sent", w.smtp.count(), w.box.appended)
	}
	for _, d := range []string{a, b} {
		if s := w.leadState(t, d); s != leadstore.LeadContacted {
			t.Errorf("%s lead %s, want contacted", d, s)
		}
		if s := w.msgState(t, d, 0); s != leadstore.MsgSent {
			t.Errorf("%s step 0 %s", d, s)
		}
	}
	// Step 1 four business days on (Mon 2 Nov), step 2 waits for step 1.
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 1 AND due_at::date = '2026-11-02'`); n != 2 {
		t.Errorf("%d follow-ups due Mon 2 Nov", n)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 2 AND due_at IS NOT NULL`); n != 0 {
		t.Errorf("%d step 2 scheduled before step 1 went", n)
	}
	if n := w.count(t, `SELECT count(*) FROM sender_check WHERE spf AND dkim AND dmarc`); n != 1 {
		t.Errorf("%d passing sender checks", n)
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.HasPrefix(note, "live · sent 2 · refused 0") {
		t.Errorf("run note %q", note)
	}

	// Next Monday: the first follow-up goes, then bravo replies, then the re-sync (11 min
	// later) stops bravo's follow-up.
	w.clock.set(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	bravoFirst := w.firstMessageID(t, b)
	w.smtp.onSend = func(n int, to string) {
		if n == 2 {
			w.box.deliver("INBOX", replyHeader(b, bravoFirst, "Re: Your Drupal site"), "Yes, let us talk next week.")
		}
	}
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "second send", out, "1 reply to read in the outreach mailbox: "+b, "Sent 1: "+a)
	if w.smtp.count() != 3 {
		t.Errorf("%d messages in all, want 3: %v", w.smtp.count(), w.smtp.recipients())
	}
	if s := w.leadState(t, b); s != leadstore.LeadReplied {
		t.Errorf("bravo %s, want replied", s)
	}
	if s := w.msgState(t, b, 1); s != leadstore.MsgCancelled {
		t.Errorf("bravo's follow-up %s, want cancelled", s)
	}
	if n := w.count(t, `SELECT count(*) FROM mailbox_event WHERE kind = 'reply' AND from_domain = $1 AND matched_message_id IS NOT NULL`, b); n != 1 {
		t.Errorf("%d reply events", n)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = $1 AND m.step = 2 AND m.due_at IS NOT NULL`, a); n != 1 {
		t.Error("alpha's step 2 was not scheduled after step 1")
	}
	// The DNS check passed today already: no second row.
	if n := w.count(t, `SELECT count(*) FROM sender_check`); n != 2 {
		t.Errorf("%d sender checks (one per day)", n)
	}
	w.sweep(t, "info@"+a, "info@"+b)
}

func TestSendStopsBeforeSMTP(t *testing.T) {
	t.Run("dns", func(t *testing.T) {
		w := newSendWorld(t, true, 20)
		delete(w.dns, "_dmarc."+testSenderDomain)
		w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "dns", out, "No sends: the sending domain's DNS check failed (SPF pass · DKIM pass · DMARC fail)")
		if w.smtp.count() != 0 || w.box.opens != 0 {
			t.Errorf("sent %d, opened IMAP %d times after a DNS failure", w.smtp.count(), w.box.opens)
		}
		if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "notes dns") {
			t.Errorf("note %q", note)
		}
		if strings.Contains(out, testSenderDomain) {
			t.Errorf("output names the sending domain:\n%s", out)
		}
		w.sweep(t)
	})
	t.Run("imap", func(t *testing.T) {
		w := newSendWorld(t, true, 20)
		w.box.dialErr = errors.New("mailbox: imap connect: connection refused")
		w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "imap", out, "No further sends: the reply sync failed")
		if w.smtp.count() != 0 {
			t.Errorf("sent %d with IMAP down", w.smtp.count())
		}
		if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "notes sync") {
			t.Errorf("note %q", note)
		}
		if s := w.msgState(t, "alpha.example", 0); s != leadstore.MsgApproved {
			t.Errorf("message %s, want approved", s)
		}
	})
	t.Run("sync lock", func(t *testing.T) {
		w := newSendWorld(t, true, 20)
		w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})
		pool := storetestPool(t)
		l, err := leadstore.TryLock(context.Background(), pool, leadstore.SyncLockKey)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Release()
		start := time.Now()
		out := w.mustRun(t, "leads", "send")
		wantLines(t, "send under a sync", out, "No further sends: a sync is still running")
		if time.Since(start) < leadsMail.syncWait {
			t.Errorf("send gave up before waiting %s", leadsMail.syncWait)
		}
		if w.smtp.count() != 0 {
			t.Errorf("sent %d while a sync ran", w.smtp.count())
		}
		code, out := w.run(t, "leads", "sync")
		if code != 0 || !strings.Contains(out, "A sync is running") {
			t.Errorf("standalone sync under the lock exited %d:\n%s", code, out)
		}
	})
	t.Run("live needs a mailbox address", func(t *testing.T) {
		w := newSendWorld(t, true, 20)
		w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})
		for _, bad := range []string{"outreach", "Outreach <" + testUser + ">", "outreach@"} {
			t.Setenv("LEADHOUND_OUTREACH_USER", bad)
			code, out := w.run(t, "leads", "send")
			if code != 2 || !strings.Contains(out, "LEADHOUND_OUTREACH_USER") {
				t.Errorf("user %q exited %d:\n%s", bad, code, out)
			}
		}
		if w.smtp.count() != 0 || w.count(t, `SELECT count(*) FROM run WHERE command = 'leads-send'`) != 0 || w.count(t, `SELECT count(*) FROM sender_check`) != 0 {
			t.Error("a live send without a valid mailbox address started")
		}
	})
	t.Run("live needs its settings", func(t *testing.T) {
		w := newSendWorld(t, true, 20)
		t.Setenv("LEADHOUND_SMTP_ADDR", "")
		code, out := w.run(t, "leads", "send")
		if code != 2 || !strings.Contains(out, "LEADHOUND_SMTP_ADDR") || strings.Contains(out, testPassword) {
			t.Errorf("live without SMTP exited %d:\n%s", code, out)
		}
	})
}

func storetestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s.Pool()
}

// Cap, --max, and every SMTP outcome: maybe_sent counts toward the cap and is never
// planned again; a retryable failure waits an hour; a 550 refuses and suppresses.
func TestSendCapAndOutcomes(t *testing.T) {
	w := newSendWorld(t, true, 20)
	domains := []string{"alpha.example", "bravo.example", "charlie.example", "delta.example", "echo.example"}
	leads := map[int64]string{}
	for _, d := range domains {
		leads[w.lead(t, d)] = d
	}
	w.batch(t, false, leads)
	// Plan order is by lead: alpha first. alpha: connection lost after DATA; bravo: 550;
	// charlie: 421.
	w.smtp.errs = map[int]error{
		0: &mailbox.SendError{Kind: mailbox.Uncertain, Stage: "data", Text: "connection reset"},
		1: &mailbox.SendError{Kind: mailbox.Refused, Stage: "rcpt", Code: 550, Bounce: true, Text: "no such user"},
		2: &mailbox.SendError{Kind: mailbox.Retryable, Stage: "mail", Code: 421, Text: "try later"},
	}
	out := w.mustRun(t, "leads", "send", "--max", "4")
	wantLines(t, "outcomes", out, "Check by hand: 1 message may have gone out", "Refused 1: message", "1 message failed for now and will be retried in an hour.",
		"Held for a later run: --max 1.")
	if s := w.msgState(t, "alpha.example", 0); s != leadstore.MsgMaybeSent {
		t.Errorf("alpha %s, want maybe_sent", s)
	}
	if s := w.msgState(t, "bravo.example", 0); s != leadstore.MsgRefused {
		t.Errorf("bravo %s", s)
	}
	if n := w.count(t, `SELECT count(*) FROM suppression WHERE kind = 'address' AND value = $1 AND reason = 'bounce'`, "info@bravo.example"); n != 1 {
		t.Error("a 550 did not suppress the address")
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		WHERE o.domain = 'charlie.example' AND m.step = 0 AND m.state = 'approved'
		AND m.due_at BETWEEN '2026-10-27 11:00:00+00' AND '2026-10-27 11:30:00+00'`); n != 1 {
		t.Error("a 421 did not wait an hour")
	}
	if s := w.msgState(t, "delta.example", 0); s != leadstore.MsgSent {
		t.Errorf("delta %s, want sent", s)
	}

	// The same day: sent 1 + maybe_sent 1 of a cap of 3 leaves one place.
	w.editConfig(t, `per_day: 20`, `per_day: 3`)
	w.clock.set(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	w.smtp.errs = nil
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "second run", out, "Sent 1: ", "Held for a later run: today's cap 1.")
	if got := w.smtp.recipients(); len(got) != 5 || slices.Contains(got[4:], "info@alpha.example") {
		t.Errorf("recipients %v: maybe_sent was planned again", got)
	}
	if s := w.msgState(t, "alpha.example", 0); s != leadstore.MsgMaybeSent {
		t.Errorf("alpha %s after a second run", s)
	}
	w.sweep(t, "info@alpha.example", "info@bravo.example")
}

// Shadow writes to the outbox, never counts toward the cap or contacts a lead, and the two
// kinds of batch never cross (RA1).
func TestSendShadowAndLiveApart(t *testing.T) {
	w := newSendWorld(t, false, 1)
	t.Setenv("LEADHOUND_IMAP_ADDR", "")
	shadowLeads := map[int64]string{w.lead(t, "alpha.example"): "alpha.example", w.lead(t, "bravo.example"): "bravo.example"}
	w.batch(t, true, shadowLeads)
	live := w.lead(t, "charlie.example")
	w.batch(t, false, map[int64]string{live: "charlie.example"})

	out := w.mustRun(t, "leads", "send")
	wantLines(t, "shadow", out, "No IMAP settings: nothing synced; shadow sends go ahead", "Shadow: wrote 1 to the outbox")
	// A cap of 1 holds the second shadow message; the live batch is not touched.
	if s := w.msgState(t, "charlie.example", 0); s != leadstore.MsgApproved {
		t.Errorf("a shadow send touched the live batch: %s", s)
	}
	entries, err := os.ReadDir(filepath.Join(w.dir, outboxDir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("outbox: %v %v", entries, err)
	}
	if w.smtp.count() != 0 {
		t.Error("shadow reached SMTP")
	}
	w.clock.set(time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC))
	w.mustRun(t, "leads", "send")
	for _, d := range []string{"alpha.example", "bravo.example"} {
		if s := w.leadState(t, d); s != leadstore.LeadNew {
			t.Errorf("shadow lead %s is %s", d, s)
		}
		if s := w.msgState(t, d, 0); s != leadstore.MsgShadowSent {
			t.Errorf("shadow message %s is %s", d, s)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id WHERE b.shadow AND m.step = 1 AND m.due_at IS NOT NULL`); n != 2 {
		t.Errorf("%d shadow follow-ups scheduled", n)
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.HasPrefix(note, "shadow · sent 1") {
		t.Errorf("note %q", note)
	}
	if n := w.count(t, `SELECT count(*) FROM run WHERE command = 'leads-send' AND source = 'outreach-shadow'`); n != 2 {
		t.Errorf("%d shadow run rows", n)
	}

	// Live: the two shadow_sent today leave the cap of 1 free, and the shadow batch is left alone.
	t.Setenv("LEADHOUND_OUTREACH", "live")
	t.Setenv("LEADHOUND_IMAP_ADDR", "imap."+testSenderDomain+":993")
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "live after shadow", out, "Sent 1: charlie.example")
	if s := w.leadState(t, "charlie.example"); s != leadstore.LeadContacted {
		t.Errorf("live lead %s", s)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id WHERE b.shadow AND m.state = 'sent'`); n != 0 {
		t.Error("a live send sent a shadow message")
	}
	if got := w.smtp.recipients(); len(got) != 1 {
		t.Errorf("SMTP got %v", got)
	}
	if n := w.count(t, `SELECT count(*) FROM run WHERE command = 'leads-send' AND source = 'outreach'`); n != 1 {
		t.Errorf("%d live run rows", n)
	}
}

// The grid's send cells: a lead that moved on is refused (its messages cancelled); only a
// batched or contacted lead sends. Plus a tampered row, a reserved recipient and a
// suppression added mid-run.
func TestSendRefusals(t *testing.T) {
	w := newSendWorld(t, true, 20)
	ctx := context.Background()
	states := []string{leadstore.LeadReplied, leadstore.LeadBounced, leadstore.LeadOptedOut, leadstore.LeadSuppressed, leadstore.LeadSkipped, leadstore.LeadClosed}
	leads := map[int64]string{}
	ids := map[string]int64{}
	for _, d := range []string{"ok.example", "late.example", "tampered.example", "replied.example", "bounced.example", "optedout.example", "suppressed.example", "skipped.example", "closed.example"} {
		id := w.lead(t, d)
		leads[id], ids[d] = d, id
	}
	w.batch(t, false, leads)
	for i, d := range []string{"replied.example", "bounced.example", "optedout.example", "suppressed.example", "skipped.example", "closed.example"} {
		if err := w.lq.SetLeadState(ctx, ids[d], states[i], "test", leadstore.ActorSystem); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE outreach_message SET body_text = body_text || 'P.S. changed'
		WHERE step = 0 AND lead_id = $1`, ids["tampered.example"]); err != nil {
		t.Fatal(err)
	}
	// The first send suppresses late.example's domain: its message must not go.
	w.smtp.onSend = func(n int, to string) {
		if n == 0 {
			if _, err := leadstoretest.Q(t).Suppress(ctx, leadstore.SuppressDomain, "late.example", leadstore.ReasonManual, "test"); err != nil {
				t.Error(err)
			}
		}
	}
	out := w.mustRun(t, "leads", "send")
	if got := w.smtp.recipients(); len(got) != 1 || got[0] != "info@ok.example" {
		t.Errorf("SMTP got %v, want ok.example only:\n%s", got, out)
	}
	wantLines(t, "refusals", out, "Refused 1: message", "(tampered)")
	for _, d := range []string{"late.example", "replied.example", "bounced.example", "optedout.example", "suppressed.example", "skipped.example", "closed.example"} {
		if s := w.msgState(t, d, 0); s != leadstore.MsgCancelled {
			t.Errorf("%s step 0 %s, want cancelled", d, s)
		}
	}
	if s := w.msgState(t, "tampered.example", 0); s != leadstore.MsgRefused {
		t.Errorf("tampered %s", s)
	}
	if r := w.str(t, `SELECT cancel_reason FROM outreach_message WHERE step = 0 AND lead_id = $1`, ids["replied.example"]); r != "lead replied" {
		t.Errorf("replied lead's cancel reason %q", r)
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "notes tampered") {
		t.Errorf("note %q", note)
	}

	// A follow-up of a contacted lead sends; a reserved recipient is refused live (RA25).
	w.clock.set(time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC))
	leadsMail.allowReserved = false
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "reserved", out, "(reserved)")
	if s := w.msgState(t, "ok.example", 1); s != leadstore.MsgRefused {
		t.Errorf("a reserved follow-up %s, want refused", s)
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "notes reserved") {
		t.Errorf("note %q", note)
	}
	if w.smtp.count() != 1 {
		t.Errorf("a reserved recipient reached SMTP")
	}
}

// Two sends at once over one due message send it once (RA3).
func TestSendConcurrentOnce(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})
	w.smtp.block, w.smtp.entered = make(chan struct{}), make(chan struct{}, 1)
	var first bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- leadsSendRun(context.Background(), &first, sendOptions{}) }()
	<-w.smtp.entered
	var second bytes.Buffer
	if err := leadsSendRun(context.Background(), &second, sendOptions{}); err != nil {
		t.Fatal(err)
	}
	close(w.smtp.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.String(), "Another send is running; this one did nothing.") {
		t.Errorf("second run:\n%s", second.String())
	}
	if w.smtp.count() != 1 {
		t.Errorf("sent %d times", w.smtp.count())
	}
	// And once the first is done, a third finds nothing due.
	if out := w.mustRun(t, "leads", "send"); !strings.Contains(out, "Nothing due to send.") || w.smtp.count() != 1 {
		t.Errorf("third run:\n%s", out)
	}
}

func TestSyncCommand(t *testing.T) {
	w := newSendWorld(t, true, 20)
	t.Setenv("LEADHOUND_IMAP_ADDR", "")
	if out := w.mustRun(t, "leads", "sync"); !strings.Contains(out, "No IMAP settings") || w.box.opens != 0 {
		t.Errorf("sync without IMAP:\n%s", out)
	}
	t.Setenv("LEADHOUND_IMAP_ADDR", "imap."+testSenderDomain+":993")

	leads := map[int64]string{}
	for _, d := range []string{"reply.example", "optout.example", "bounce.example", "auto.example", "colleague.example"} {
		leads[w.lead(t, d)] = d
	}
	w.batch(t, false, leads)
	w.mustRun(t, "leads", "send")
	if w.smtp.count() != 5 {
		t.Fatalf("sent %d", w.smtp.count())
	}
	w.box.deliver("INBOX", replyHeader("reply.example", w.firstMessageID(t, "reply.example"), "Re: hello"), "Sounds interesting.")
	w.box.deliver("INBOX", replyHeader("optout.example", w.firstMessageID(t, "optout.example"), "Re: hello"), "Please unsubscribe us.")
	w.box.deliver("INBOX", "From: Mail Delivery System <mailer-daemon@mx.bounce.example>\r\nReturn-Path: <>\r\nSubject: Undelivered\r\n"+
		"Content-Type: text/plain\r\n\r\n", "")
	w.box.deliver("INBOX", "From: Office <office@auto.example>\r\nSubject: Out of office\r\nAuto-Submitted: auto-replied\r\nIn-Reply-To: <"+w.firstMessageID(t, "auto.example")+">\r\n\r\n", "Back Monday.")
	// A colleague at the same domain, no thread: matched by domain.
	w.box.deliver("Junk", "From: Someone <team@colleague.example>\r\nSubject: Your note\r\n\r\n", "We might be interested.")
	w.box.deliver("Junk", "From: Shop <news@unrelated.example>\r\nSubject: Sale\r\n\r\n", "Big sale.")

	out := w.mustRun(t, "leads", "sync")
	wantLines(t, "sync", out,
		"3 replies to read in the outreach mailbox: reply.example, optout.example, colleague.example",
		"Of those, 1 is an opt-out, now suppressed.",
		"1 automatic reply (recorded only).",
		"2 other messages in the outreach mailbox.")
	if !strings.HasPrefix(out, "3 replies to read") {
		t.Errorf("sync output does not lead with what to read:\n%s", out)
	}
	for d, want := range map[string]string{"reply.example": "replied", "optout.example": "opted_out", "auto.example": "contacted", "colleague.example": "replied", "bounce.example": "contacted"} {
		if s := w.leadState(t, d); s != want {
			t.Errorf("%s lead %s, want %s", d, s, want)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM suppression WHERE value IN ('info@optout.example', 'optout.example') AND reason = 'opt_out'`); n != 2 {
		t.Errorf("%d opt-out suppressions", n)
	}
	if n := w.count(t, `SELECT count(*) FROM mailbox_event`); n != 6 {
		t.Errorf("%d events", n)
	}
	if n := w.count(t, `SELECT count(*) FROM mailbox_event WHERE mailbox = 'Junk'`); n != 2 {
		t.Errorf("%d Junk events", n)
	}
	if n := w.count(t, `SELECT count(*) FROM mailbox_event WHERE from_domain LIKE '%@%'`); n != 0 {
		t.Error("an event holds an address")
	}
	if c, _, _ := w.lq.MailboxCursor(context.Background(), "INBOX"); c.UIDValidity != 7 || c.LastUID != 4 {
		t.Errorf("INBOX cursor %+v", c)
	}
	byDate := w.box.byDate

	// Again: nothing new, read from the cursor.
	if out := w.mustRun(t, "leads", "sync"); !strings.Contains(out, "Mailbox synced: nothing new.") {
		t.Errorf("second sync:\n%s", out)
	}
	if w.box.byDate != byDate {
		t.Errorf("a second sync rescanned by date")
	}
	// A new UIDVALIDITY: read again by date; nothing is recorded twice under the old one, and
	// the effects stay as they were.
	w.box.mu.Lock()
	w.box.validity["INBOX"] = 8
	w.box.mu.Unlock()
	w.mustRun(t, "leads", "sync")
	if w.box.byDate != byDate+1 {
		t.Errorf("a UIDVALIDITY change did not rescan by date (%d → %d)", byDate, w.box.byDate)
	}
	if c, _, _ := w.lq.MailboxCursor(context.Background(), "INBOX"); c.UIDValidity != 8 || c.LastUID != 4 {
		t.Errorf("INBOX cursor after the change %+v", c)
	}
	if s := w.leadState(t, "optout.example"); s != "opted_out" {
		t.Errorf("rescan changed opted_out to %s", s)
	}
	w.sweep(t, "info@reply.example", "office@reply.example", "team@colleague.example")
}

func TestSelftest(t *testing.T) {
	w := newSendWorld(t, true, 1)
	t.Setenv("LEADHOUND_OUTREACH", "")
	if code, out := w.run(t, "leads", "selftest", "--to", testOperator); code != 2 || !strings.Contains(out, "LEADHOUND_OUTREACH=live") {
		t.Errorf("shadow selftest exited %d:\n%s", code, out)
	}
	t.Setenv("LEADHOUND_OUTREACH", "live")
	other := "someone" + "@" + "elsewhere.example"
	if code, out := w.run(t, "leads", "selftest", "--to", other); code != 2 || strings.Contains(out, other) || !strings.Contains(out, "operator_addresses") {
		t.Errorf("selftest to a stranger exited %d:\n%s", code, out)
	}
	if code, _ := w.run(t, "leads", "selftest"); code != 2 {
		t.Errorf("selftest without --to exited %d", code)
	}
	if w.smtp.count() != 0 {
		t.Fatal("a refused selftest sent")
	}

	// Today's cap of 1 is used already; the selftest still goes, and counts.
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})
	w.mustRun(t, "leads", "send")
	w.batch(t, false, map[int64]string{w.lead(t, "bravo.example"): "bravo.example"})
	out := w.mustRun(t, "leads", "selftest", "--to", strings.ToUpper(testOperator))
	wantLines(t, "selftest", out, "Selftest 1: message", "Sent 1: your selftest address")
	if got := w.smtp.recipients(); len(got) != 2 || got[1] != testOperator {
		t.Errorf("SMTP got %v", got)
	}
	if s := w.msgState(t, "bravo.example", 0); s != leadstore.MsgApproved {
		t.Errorf("selftest sent another lead's message: %s", s)
	}
	if n := w.count(t, `SELECT count(*) FROM org o JOIN lead l ON l.org_id = o.id WHERE o.domain = 'selftest-1' AND o.source = 'selftest' AND l.kind = 'selftest' AND l.state = 'contacted'`); n != 1 {
		t.Error("selftest rows missing")
	}
	if n, err := w.lq.SentSince(context.Background(), time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)); err != nil || n != 2 {
		t.Errorf("sent today %d (%v): the selftest does not count", n, err)
	}
	if note := w.lastNote(t, commandLeadsSelftest); !strings.HasPrefix(note, "live · sent 1") {
		t.Errorf("selftest note %q", note)
	}

	// RA24: a mail from the operator's domain with no thread is not a selftest reply; a
	// threaded opt-out suppresses the address only.
	w.box.deliver("INBOX", "From: Me <other@"+testSenderDomain+">\r\nSubject: hi\r\n\r\n", "unrelated")
	selfID := w.str(t, `SELECT m.message_id FROM outreach_message m JOIN lead l ON l.id = m.lead_id WHERE l.kind = 'selftest'`)
	w.box.deliver("INBOX", "From: Me <"+testOperator+">\r\nSubject: Re: Leadhound selftest\r\nIn-Reply-To: <"+selfID+">\r\n\r\n", "unsubscribe")
	out = w.mustRun(t, "leads", "sync")
	wantLines(t, "selftest sync", out, "1 reply to read in the outreach mailbox: your selftest", "1 other message")
	if n := w.count(t, `SELECT count(*) FROM suppression WHERE kind = 'domain'`); n != 0 {
		t.Error("a selftest opt-out suppressed a domain")
	}
	if n := w.count(t, `SELECT count(*) FROM suppression WHERE kind = 'address' AND reason = 'opt_out'`); n != 1 {
		t.Error("a selftest opt-out did not suppress the address")
	}
	w.sweep(t, "info@alpha.example")
}

func TestSendersCheck(t *testing.T) {
	w := newSendWorld(t, true, 20)
	t.Setenv("LEADHOUND_OUTREACH_USER", "")
	if code, out := w.run(t, "leads", "senders", "check"); code != 2 || !strings.Contains(out, "LEADHOUND_OUTREACH_USER") {
		t.Errorf("no user exited %d:\n%s", code, out)
	}
	t.Setenv("LEADHOUND_OUTREACH_USER", testUser)
	w.dns["spacemail._domainkey."+testSenderDomain] = []string{"v=spf1 wrong record"}
	out := w.mustRun(t, "leads", "senders", "check")
	wantLines(t, "check", out, "SPF pass\nDKIM fail\nDMARC pass\n", "a record fails")
	if strings.Contains(out, testSenderDomain) {
		t.Errorf("output names the domain:\n%s", out)
	}
	if n := w.count(t, `SELECT count(*) FROM sender_check WHERE spf AND NOT dkim AND dmarc AND domain_sha256 = $1`, leadstore.SenderDomainSHA256(testSenderDomain)); n != 1 {
		t.Errorf("%d sender_check rows", n)
	}
	if code, _ := w.run(t, "leads", "senders"); code != 2 {
		t.Errorf("senders without check exited %d", code)
	}
	w.sweep(t)
}

// An account-level SMTP failure stops the run: the message stays approved, nothing is
// suppressed, and the command fails so the operator sees it.
func TestSendAccountFailureStops(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example", w.lead(t, "bravo.example"): "bravo.example"})
	w.smtp.errs = map[int]error{0: &mailbox.SendError{Kind: mailbox.Account, Stage: "auth", Code: 535, Text: "authentication failed"}}
	code, out := w.run(t, "leads", "send")
	if code != 1 || !strings.Contains(out, "the outreach mailbox account was refused at auth (535)") {
		t.Errorf("exited %d:\n%s", code, out)
	}
	if w.smtp.count() != 1 {
		t.Errorf("%d SMTP attempts, want the run to stop after 1", w.smtp.count())
	}
	for _, d := range []string{"alpha.example", "bravo.example"} {
		if s := w.msgState(t, d, 0); s != leadstore.MsgApproved {
			t.Errorf("%s %s, want approved", d, s)
		}
	}
	if n := w.count(t, `SELECT count(*) FROM suppression`); n != 0 {
		t.Error("an account failure suppressed something")
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE sent_at IS NOT NULL`); n != 0 {
		t.Error("a requeued message keeps an attempt time")
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "notes smtp-account") || !strings.HasSuffix(note, "error") {
		t.Errorf("note %q", note)
	}
	w.sweep(t, "info@alpha.example")
}

// Business hours are checked again at the moment of sending: the spacing carries London past
// 16:30, so its second message waits while New York, still open, goes.
func TestSendHoursAtSendTime(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.editConfig(t, `(?m)^  GB: `, "  US: {channel: email, lang: en, tz: America/New_York, footer: footer-en}\n  GB: ")
	w.editConfig(t, `spacing_minutes: \[3, 9\]`, `spacing_minutes: [11, 11]`)
	w.clock.set(time.Date(2026, 10, 27, 16, 25, 0, 0, time.UTC))
	london1, london2 := w.lead(t, "london1.example"), w.lead(t, "london2.example")
	ny := w.leadIn(t, "newyork.example", "US")
	w.batch(t, false, map[int64]string{london1: "london1.example", london2: "london2.example", ny: "newyork.example"})
	out := w.mustRun(t, "leads", "send")
	if got := w.smtp.recipients(); len(got) != 2 || got[0] != "info@london1.example" || got[1] != "info@newyork.example" {
		t.Errorf("SMTP got %v:\n%s", got, out)
	}
	if s := w.msgState(t, "london2.example", 0); s != leadstore.MsgApproved {
		t.Errorf("london2 %s, want approved and due", s)
	}
	wantLines(t, "hours", out, "Held for a later run: outside business hours 1.")

	// Nothing left in its hours after the pause: the run stops without sleeping.
	w.clock.set(time.Date(2026, 10, 28, 16, 25, 0, 0, time.UTC))
	w.batch(t, false, map[int64]string{w.lead(t, "london3.example"): "london3.example", w.lead(t, "london4.example"): "london4.example"})
	before := w.clock.now()
	out = w.mustRun(t, "leads", "send")
	if !w.clock.now().Equal(before) {
		t.Errorf("slept %s with nothing left to send", w.clock.now().Sub(before))
	}
	wantLines(t, "stop", out, "outside business hours")
}

// A row left in sending by a run that broke off counts toward the cap and is for checking by
// hand; a follow-up scheduling failure never undoes a send.
func TestSendStuckAndFollowupFailure(t *testing.T) {
	w := newSendWorld(t, true, 2)
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example", w.lead(t, "bravo.example"): "bravo.example",
		w.lead(t, "charlie.example"): "charlie.example"})
	var stuck int64
	if err := owner.QueryRow(ctx, `UPDATE outreach_message SET state = 'sending', sent_at = '2026-10-27 09:40:00+00'
		WHERE id = (SELECT min(id) FROM outreach_message WHERE step = 0) RETURNING id`).Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `CREATE FUNCTION test_no_followups() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'no follow-ups today'; END $$;
		CREATE TRIGGER test_no_followups BEFORE UPDATE ON outreach_message
		FOR EACH ROW WHEN (OLD.step > 0 AND NEW.due_at IS DISTINCT FROM OLD.due_at) EXECUTE FUNCTION test_no_followups()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = storetest.OwnerConn(t).Exec(context.Background(), `DROP TRIGGER IF EXISTS test_no_followups ON outreach_message; DROP FUNCTION IF EXISTS test_no_followups()`)
	})
	out := w.mustRun(t, "leads", "send")
	wantLines(t, "stuck", out, fmt.Sprintf("left mid-send by an earlier run may have gone out (never resent; counted toward today's cap): message %d.", stuck),
		"Sent 1: ", "Held for a later run: today's cap 1.")
	if w.smtp.count() != 1 {
		t.Errorf("sent %d with one place left under the cap", w.smtp.count())
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 0 AND state = 'sent'`); n != 1 {
		t.Errorf("%d sent rows: a follow-up failure rolled the send back", n)
	}
	if n := w.count(t, `SELECT count(*) FROM lead WHERE state = 'contacted'`); n != 1 {
		t.Errorf("%d contacted leads", n)
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "followup") || !strings.Contains(note, "stuck-sending") {
		t.Errorf("note %q", note)
	}
}

// A selftest goes to the operator whatever the hour, and one that cannot go is cancelled,
// never left for an ordinary send.
func TestSelftestHoursAndUnsent(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.clock.set(time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)) // Saturday night
	out := w.mustRun(t, "leads", "selftest", "--to", testOperator)
	if got := w.smtp.recipients(); len(got) != 1 || got[0] != testOperator {
		t.Errorf("SMTP got %v:\n%s", got, out)
	}
	w.smtp.errs = map[int]error{1: &mailbox.SendError{Kind: mailbox.Retryable, Stage: "mail", Code: 451, Text: "busy"}}
	out = w.mustRun(t, "leads", "selftest", "--to", testOperator)
	wantLines(t, "unsent selftest", out, "was not sent; it is cancelled (selftest-not-sent)")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE state = 'cancelled' AND cancel_reason = 'selftest-not-sent'`); n != 1 {
		t.Errorf("%d cancelled selftests", n)
	}
	// An ordinary send never picks a selftest up.
	w.clock.set(time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC))
	w.smtp.errs = nil
	if out := w.mustRun(t, "leads", "send"); !strings.Contains(out, "Nothing due to send.") || w.smtp.count() != 2 {
		t.Errorf("ordinary send after selftests:\n%s", out)
	}
	w.sweep(t)
}
