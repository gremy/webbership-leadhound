//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The batch through mailboxes (Task 2.1): every draft is assigned a sending mailbox at batch
// creation, its From frozen with it and covered by the batch hash.

// TestMailboxSizingDriftOneMailbox (MB-R7): with exactly one ready mailbox, the per-mailbox
// sizing is the old single-mailbox batchSizing, field for field and word for word, over ramp
// days, weekdays and weekends, today's window open or over, sends today and mail waiting.
func TestMailboxSizingDriftOneMailbox(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	var nows []time.Time
	for d := 26; d <= 32; d++ { // Monday 26 Oct to Sunday 1 Nov
		for _, h := range []int{7, 11, 18} {
			nows = append(nows, time.Date(2026, 10, d, h, 0, 0, 0, loc))
		}
	}
	day := func(y, m, d int) *time.Time { t := time.Date(y, time.Month(m), d, 10, 0, 0, 0, loc); return &t }
	n := 0
	for _, start := range []string{"2026-10-27", "2026-10-15", "2026-12-01", "first-send"} {
		for _, perDay := range []int{0, 1, 4, 40} {
			cfg := sizingConfig(t, start, perDay)
			for _, first := range []*time.Time{nil, day(2026, 10, 20), day(2026, 9, 1)} {
				if first != nil && !cfg.Caps.StartDate.IsFirstSend() {
					continue // liveSizing reads the first send only with first-send
				}
				for _, now := range nows {
					for _, ld := range []sizingLoad{{}, {sentToday: 2}, {approved: 5}, {approved: 7, toCheck: 2, sentToday: 1}, {approved: 200}} {
						for _, size := range []int{1, 10} {
							ld.firstSend = first
							old := batchSizing(cfg, ld, now, size)
							mb := outreach.MailboxLoad{ID: 1, Domain: "sender.example", FirstSend: first, SentToday: ld.sentToday,
								Waiting: ld.approved + ld.toCheck, Ready: true}
							got, rooms := mailboxSizing(cfg, []outreach.MailboxLoad{mb}, ld.toCheck, now, size, 0)
							want := old
							want.Mailboxes = 1
							if got != want {
								t.Fatalf("start %s, %d a day, first %v, now %s, load %+v, size %d:\n got %+v\nwant %+v",
									start, perDay, first, now, ld, size, got, want)
							}
							if got.Words() != old.Words() {
								t.Fatalf("words %q, want %q", got.Words(), old.Words())
							}
							if len(rooms) != 1 || rooms[1] != old.Room {
								t.Fatalf("room %v, want %d", rooms, old.Room)
							}
							n++
						}
					}
				}
			}
		}
	}
	if n < 1000 {
		t.Fatalf("only %d cases compared", n)
	}
}

// TestMailboxSizingSpread: two ready mailboxes on one domain share caps.domain_per_day; a
// mailbox that is not ready gets no room; on two domains each has its own ramp.
func TestMailboxSizingSpread(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	tue := time.Date(2026, 10, 27, 11, 0, 0, 0, loc)
	cfg := sizingConfig(t, "2026-10-27", 10)
	cfg.Caps.DomainPerDay = 4
	one := []outreach.MailboxLoad{{ID: 1, Domain: "sender.example", Ready: true}, {ID: 2, Domain: "sender.example", Ready: true}}
	z, rooms := mailboxSizing(cfg, one, 0, tue, 10, 0)
	if z.Room != 12 || z.Leads != 4 || z.Mailboxes != 2 || rooms[1]+rooms[2] != 12 {
		t.Errorf("one domain: %+v, rooms %v", z, rooms)
	}
	if !strings.Contains(z.Words(), "room for 12 emails over the next 3 days on 2 mailboxes") {
		t.Errorf("words %q", z.Words())
	}
	cfg.Caps.DomainPerDay = 0 // the default, 50
	two := []outreach.MailboxLoad{{ID: 1, Domain: "sender.example", Ready: true}, {ID: 2, Domain: "other.example", Ready: true}, {ID: 3, Domain: "third.example"}}
	z, rooms = mailboxSizing(cfg, two, 0, tue, 20, 0)
	if z.Room != 60 || z.Leads != 20 || z.Mailboxes != 2 || rooms[1] != 30 || rooms[2] != 30 || rooms[3] != 0 {
		t.Errorf("two domains: %+v, rooms %v", z, rooms)
	}
}

// setDomainCap rewrites leads.yaml's caps.domain_per_day.
func setDomainCap(t *testing.T, dir string, n int) {
	t.Helper()
	path := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	out := regexp.MustCompile(`(?m)^  domain_per_day: .*$`).ReplaceAll(raw, fmt.Appendf(nil, "  domain_per_day: %d", n))
	if string(out) == string(raw) {
		t.Fatal("leads.yaml domain_per_day not rewritten")
	}
	if err := os.WriteFile(path, out, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

// countingResolver fails the test on any DNS lookup made while it is installed, and counts them.
type countingResolver struct {
	mu sync.Mutex
	n  int
	t  *testing.T
}

func (r *countingResolver) LookupTXT(context.Context, string) ([]string, error) {
	r.mu.Lock()
	r.n++
	r.mu.Unlock()
	r.t.Error("a DNS lookup was made")
	return nil, errors.New("no network in this test")
}

func (r *countingResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// mailboxOf is the address and From name of each lead's messages in batch id, as
// "domain: address|from" per lead; a lead whose messages disagree is a test failure.
func mailboxOf(t *testing.T, id int64) map[string]string {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT o.domain, coalesce(mb.address, ''), m.from_name, m.message_id, m.channel
		 FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 LEFT JOIN sender_mailbox mb ON mb.id = m.mailbox_id
		 WHERE m.batch_id = $1 ORDER BY m.lead_id, m.step`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var dom, addr, from, msgID, channel string
		if err := rows.Scan(&dom, &addr, &from, &msgID, &channel); err != nil {
			t.Fatal(err)
		}
		if addr == "" {
			t.Errorf("%s: a message with no mailbox", dom)
		}
		if channel == "email" && !strings.HasSuffix(msgID, "@"+addr[strings.LastIndexByte(addr, '@')+1:]) {
			t.Errorf("%s: Message-ID %q is not on the mailbox's domain (%s)", dom, msgID, addr)
		}
		v := addr + "|" + from
		if prev, ok := out[dom]; ok && prev != v {
			t.Errorf("%s: messages on %q and %q: a follow-up did not inherit its first email's mailbox", dom, prev, v)
		}
		out[dom] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBatchNeedsAMailbox: with no active mailbox no batch is drafted, shadow or live, CLI or
// console, and the words say to add one; a paused mailbox is not active.
func TestBatchNeedsAMailbox(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	lq := leadstoretest.Q(t)
	if err := lq.SetMailboxState(ctx, 1, leadstore.MailboxPaused, "by hand", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	const words = "Add a mailbox first: Mailboxes in the console, or leads mailboxes add."
	code, out, errOut := leadsRun(t, "leads", "batch", "new")
	if code != 2 || !strings.Contains(out+errOut, words) {
		t.Errorf("shadow batch new exited %d:\n%s%s", code, out, errOut)
	}
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	code, out, errOut = leadsRun(t, "leads", "batch", "new")
	if code != 2 || !strings.Contains(out+errOut, words) {
		t.Errorf("live batch new exited %d:\n%s%s", code, out, errOut)
	}
	ce := openCoreEnv(t)
	_, err := makeBatch(ctx, ce.s, newBatchSetup(ce.setup, ce.loc), leadsNow(), NewBatchRequest{LiveOnly: true})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), words) {
		t.Errorf("console: %v", err)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 0 {
		t.Errorf("%d batches written", n)
	}
}

// TestShadowBatchAssignsMailboxes: a shadow batch spreads its leads over the active
// mailboxes (a paused one gets none), each lead's follow-ups on its first email's mailbox,
// Message-IDs on the mailbox's domain, From frozen (the mailbox's name, or sender.name); and
// it never reads a DNS record nor opens a password. batch show prints each email's From.
func TestShadowBatchAssignsMailboxes(t *testing.T) {
	ctx := context.Background()
	leadsScene(t)
	lq := leadstoretest.Q(t)
	desk := seedMailbox(t, lq, "desk@other.example", withSelector("s2"), withFromName("Sales Desk"))
	paused := seedMailbox(t, lq, "paused@third.example", withDNSPass())
	if err := lq.SetMailboxState(ctx, paused.ID, leadstore.MailboxPaused, "by hand", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	saved := leadsMail
	t.Cleanup(func() { leadsMail = saved })
	res := &countingResolver{t: t}
	leadsMail.resolver = res
	fakes := newMailboxFakes()
	fakes.install(&leadsMail)

	wantLines(t, "shadow", mustRun(t, "leads", "batch", "new"), "Batch 1 (shadow): ")
	by := mailboxOf(t, 1)
	used := map[string]int{}
	for _, v := range by {
		used[v]++
	}
	if used[testUser+"|Demo Sender"] == 0 || used[desk.Address+"|Sales Desk"] == 0 || len(used) != 2 {
		t.Errorf("leads by mailbox %v, want both active mailboxes used and no other", used)
	}
	if res.count() != 0 || len(fakes.calls) != 0 {
		t.Errorf("a shadow batch made %d DNS lookups and %d mailbox calls", res.count(), len(fakes.calls))
	}
	out := mustRun(t, "leads", "batch", "show", "1")
	wantLines(t, "show", out, "From: Demo Sender <"+testUser+">", "From: Sales Desk <desk@other.example>")
	if strings.Count(out, "From: ") != batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE batch_id = 1 AND channel = 'email'`) {
		t.Errorf("not one From line per email:\n%s", out)
	}
}

// TestBatchHashCoversFrom: a fresh two-mailbox batch approves; a batch whose stored From
// name or mailbox was changed after drafting is refused; the hash of a draft changes with
// its mailbox and From name and, for a draft with neither, is what it was before mailboxes.
func TestBatchHashCoversFrom(t *testing.T) {
	ctx := context.Background()
	leadsScene(t)
	lq := leadstoretest.Q(t)
	seedMailbox(t, lq, "desk@other.example", withFromName("Front Desk"))

	mustRun(t, "leads", "batch", "new")
	wantLines(t, "fresh", mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1)), "Batch 1")
	for _, c := range []struct{ name, sql string }{
		{"from_name", `UPDATE outreach_message SET from_name = 'Someone Else' WHERE id = (SELECT min(id) FROM outreach_message WHERE batch_id = $1)`},
		{"mailbox_id", `UPDATE outreach_message SET mailbox_id = (SELECT mb.id FROM sender_mailbox mb WHERE mb.id <> outreach_message.mailbox_id ORDER BY mb.id LIMIT 1)
			WHERE id = (SELECT min(id) FROM outreach_message WHERE batch_id = $1)`},
	} {
		t.Run(c.name, func(t *testing.T) {
			mustRun(t, "leads", "batch", "new")
			id := int64(batchCount(t, `SELECT max(id)::int FROM outreach_batch`))
			hash := batchHash(t, id)
			ownerExec(t, c.sql, id)
			code, out, errOut := leadsRun(t, "leads", "batch", "approve", fmt.Sprint(id), "--hash", hash)
			if code == 0 || !strings.Contains(out+errOut, "changed") {
				t.Errorf("approve of a changed batch exited %d:\n%s%s", code, out, errOut)
			}
			if n := batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM outreach_message WHERE batch_id = %d AND state <> 'draft'`, id)); n != 0 {
				t.Errorf("%d messages left draft", n)
			}
			mustRun(t, "leads", "batch", "discard", fmt.Sprint(id))
		})
	}

	base := leadstore.DraftRow{LeadID: 1, Channel: "email", To: "a@lead.example", Subject: "s", Body: "b", MessageID: "x@sender.example"}
	h0, err := leadstore.ContentSHA256([]leadstore.DraftRow{base})
	if err != nil {
		t.Fatal(err)
	}
	withMB, withName := base, base
	withMB.MailboxID, withName.FromName = 2, "Sales Desk"
	for _, d := range []leadstore.DraftRow{withMB, withName} {
		h, err := leadstore.ContentSHA256([]leadstore.DraftRow{d})
		if err != nil || string(h) == string(h0) {
			t.Errorf("%+v: the hash does not cover it (%v)", d, err)
		}
	}
	// The hash of a draft from before mailboxes is unchanged: neither field is in its JSON.
	if b, err := json.Marshal(base); err != nil || strings.Contains(string(b), "mailbox_id") || strings.Contains(string(b), "from_name") {
		t.Errorf("a draft with no mailbox carries the new fields: %s (%v)", b, err)
	}
	// Approval recomputed the hash from the stored rows, mailbox and From included.
	ms, err := lq.BatchMessages(ctx, 1)
	if err != nil || len(ms) == 0 {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.MailboxID == 0 || m.FromName == "" || m.FromAddress == "" {
			t.Errorf("message %d: mailbox %d, From %q <%s>", m.ID, m.MailboxID, m.FromName, m.FromAddress)
		}
	}
}

// TestLiveBatchMailboxes: a live batch is sized per mailbox and spread over the ready ones;
// two mailboxes on one domain together stay within caps.domain_per_day.
func TestLiveBatchMailboxes(t *testing.T) {
	dir := leadsScene(t)
	lq := leadstoretest.Q(t)
	goLive(t)
	setCaps(t, dir, "2026-10-27", "[{from_day: 0, per_day: 10}]")
	second := seedMailbox(t, lq, "second@"+testSenderDomain, withDNSPass())

	setDomainCap(t, dir, 4)
	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "one domain", out, "Batch 1 (live): ",
		"Sizing: 4 leads: room for 12 emails over the next 3 days on 2 mailboxes, and each lead has 3 steps.")
	for dom, v := range mailboxOf(t, 1) {
		if !strings.HasPrefix(v, testUser+"|") {
			t.Errorf("%s on %s: the domain's room is the first mailbox's", dom, v)
		}
	}
	mustRun(t, "leads", "batch", "discard", "1")

	setDomainCap(t, dir, 50)
	desk := seedMailbox(t, lq, "desk@other.example", withDNSPass(), withSelector("s2"), withFromName("Sales Desk"))
	wantLines(t, "two domains", mustRun(t, "leads", "batch", "new"), "Batch 2 (live): ", "on 3 mailboxes")
	used := map[string]bool{}
	for _, v := range mailboxOf(t, 2) {
		used[strings.SplitN(v, "|", 2)[0]] = true
	}
	if !used[testUser] || !used[desk.Address] {
		t.Errorf("mailboxes used %v, want %s and %s", used, testUser, desk.Address)
	}
	if n := batchCount(t, fmt.Sprintf(`SELECT count(*)::int FROM outreach_message WHERE batch_id = 1 AND mailbox_id = %d`, second.ID)); n != 0 {
		t.Errorf("the second mailbox on the domain got %d messages, with no domain room left for it", n)
	}
}

// TestLiveBatchReadiness (decision 5): the console's batch reads only today's DNS pass and
// never reaches the network, refusing in plain words when today's check has not run or
// failed; leads batch new (the CLI and the morning timer) runs the check first.
func TestLiveBatchReadiness(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	ownerExec(t, `DELETE FROM sender_check`)
	res := &countingResolver{t: t}
	leadsMail.resolver = res
	ce := openCoreEnv(t)
	console := func() error {
		_, err := makeBatch(ctx, ce.s, newBatchSetup(ce.setup, ce.loc), leadsNow(), NewBatchRequest{LiveOnly: true})
		return err
	}
	notRun := "Today's domain check has not run for " + testSenderDomain + " yet: press Check now on its mailbox page, or wait for the morning batch."
	if err := console(); err == nil || !strings.Contains(err.Error(), notRun) {
		t.Errorf("console, no check today: %v", err)
	}
	if res.count() != 0 {
		t.Fatal("the console's batch reached DNS")
	}

	// The CLI runs the check: with DKIM missing it records the failure and says what is missing.
	failing := passingDNS()
	delete(failing, "sel1._domainkey."+testSenderDomain)
	leadsMail.resolver = failing
	failed := "Today's domain check failed for " + testSenderDomain + ": missing DKIM (selector sel1)."
	code, out, errOut := leadsRun(t, "leads", "batch", "new")
	if code != 2 || !strings.Contains(out+errOut, failed) {
		t.Errorf("CLI with DKIM missing exited %d:\n%s%s", code, out, errOut)
	}
	leadsMail.resolver = res
	if err := console(); err == nil || !strings.Contains(err.Error(), failed) {
		t.Errorf("console after a failed check: %v", err)
	}
	if n := batchCount(t, `SELECT count(*) FROM outreach_batch`); n != 0 {
		t.Errorf("%d batches written", n)
	}

	// A passing check by the CLI makes the mailbox ready, and the batch is drafted.
	leadsMail.resolver = passingDNS()
	wantLines(t, "passing", mustRun(t, "leads", "batch", "new"), "Batch 1 (live): ")
	if n := batchCount(t, `SELECT count(*) FROM sender_check WHERE selector = 'sel1'`); n != 2 {
		t.Errorf("%d sender_check rows, want the failed and the passing one", n)
	}
	// A pass today is not checked again.
	mustRun(t, "leads", "batch", "discard", "1")
	leadsMail.resolver = res
	wantLines(t, "again", mustRun(t, "leads", "batch", "new"), "Batch 2 (live): ")
	if res.count() != 0 {
		t.Error("a mailbox with today's pass was checked again")
	}
}
