//go:build integration

package main

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
	"github.com/gremy/webbership-leadhound/internal/verify"
)

// fakeReoon answers each address from status (an address not listed fails the check) and the
// balance from bal; it counts every call. No test reaches Reoon.
type fakeReoon struct {
	mu       sync.Mutex
	status   map[string]string
	bal      verify.Balance
	checks   []string
	balances int
}

func (f *fakeReoon) Check(_ context.Context, address string) (verify.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, address)
	st, ok := f.status[address]
	if !ok {
		return verify.Result{}, errors.New("verify: reoon /api/v1/verify: HTTP 500")
	}
	return verify.Result{Status: st}, nil
}

func (f *fakeReoon) Balance(context.Context) (verify.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances++
	return f.bal, nil
}

func (f *fakeReoon) checked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.checks)
}

// useReoon installs f as the Reoon client for the test, with LEADHOUND_LIVE on.
func useReoon(t *testing.T, f *fakeReoon) {
	t.Helper()
	t.Setenv("LEADHOUND_LIVE", "1")
	saved := newReoonClient
	newReoonClient = func(bool) (reoonClient, error) { return f, nil }
	t.Cleanup(func() { newReoonClient = saved })
}

// setVerify switches the Email check job on or off.
func setVerify(t *testing.T, on bool) {
	t.Helper()
	if _, err := leadstoretest.Q(t).SetJobSwitch(context.Background(), 0, leadstore.JobVerify, on, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
}

// draftedRoutes are batch id's leads as drafted: each lead's domain with its first message's
// channel and recipient ("email info@x", "form").
func draftedRoutes(t *testing.T, id int64) map[string]string {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT o.domain, m.channel, m.to_address FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id
		 WHERE m.batch_id = $1 AND m.step = 0`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var d, ch, to string
		if err := rows.Scan(&d, &ch, &to); err != nil {
			t.Fatal(err)
		}
		out[d] = strings.TrimSpace(ch + " " + to)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sweepSendable fails the test when any email message of live batch id goes to an address
// whose latest fresh check (as of leadsTuesday) is not sendable: the gate's invariant.
func sweepSendable(t *testing.T, id int, catchAll string) {
	t.Helper()
	ctx := context.Background()
	rows, err := storetest.OwnerConn(t).Query(ctx,
		`SELECT DISTINCT m.to_address FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE b.id = $1 AND NOT b.shadow AND m.channel = 'email' AND m.to_address <> ''`, id)
	if err != nil {
		t.Fatal(err)
	}
	var addrs []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		addrs = append(addrs, a)
	}
	rows.Close()
	got, err := leadstoretest.Q(t).EmailChecks(ctx, 0, addrs, leadsTuesday.Add(-verify.FreshFor))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		var hs []verify.Check
		for _, r := range got[strings.ToLower(a)] {
			hs = append(hs, verify.Check{Status: r.Status, CheckedAt: r.CheckedAt})
		}
		if v := verify.Decide(hs, catchAll, leadsTuesday, startOfDay(leadsTuesday, time.UTC)); v.Decision != verify.Send {
			t.Errorf("a live batch emails %s, whose check is %+v", a, v)
		}
	}
}

// discardLatest discards the newest batch.
func discardLatest(t *testing.T) {
	t.Helper()
	id := batchCount(t, `SELECT coalesce(max(id), 0) FROM outreach_batch`)
	mustRun(t, "leads", "batch", "discard", strconv.Itoa(id))
}

// TestVerifyBatchGate: with the Email check on, a cold start drafts no unchecked address;
// leads verify checks the addresses the batch would draft, stores each answer and is not asked
// again for a fresh one; a live batch then emails only sendable addresses (swept over every
// drafted email), skips a blocked address with its reason, falls to a form when the lead has
// one, leaves a first unknown waiting, and obeys verify.catch_all; switched off, the batch is
// exactly the batch drafted before any check.
func TestVerifyBatchGate(t *testing.T) {
	dir := leadsScene(t)
	goLive(t)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 40}]")
	ctx := context.Background()

	// Off: the batch as it was before the check existed (the baseline).
	setVerify(t, false)
	out := mustRun(t, "leads", "batch", "new")
	if strings.Contains(out, "email check") {
		t.Errorf("switched off, the batch speaks of the check:\n%s", out)
	}
	baseline := draftedRoutes(t, 1)
	want := map[string]string{"northwind.example": "email info@northwind.example", "tailspin.example": "email info@tailspin.example",
		"litware.example": "email office@litware.example", "wingtip.example": "email hello@wingtip.example", "contoso.example": "form"}
	for d, r := range want {
		if baseline[d] != r {
			t.Fatalf("baseline %s = %q, want %q (all %v)", d, baseline[d], r, baseline)
		}
	}
	discardLatest(t)

	// Cold start, on: no email_check rows, so no email lead is drafted; the form lead is.
	setVerify(t, true)
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "cold start", out, "4 leads wait for their email check.")
	if got := draftedRoutes(t, 2); len(got) != 1 || got["contoso.example"] != "form" {
		t.Errorf("cold start drafted %v, want the form lead only", got)
	}
	discardLatest(t)

	// tailspin gets a form route to fall to.
	lq := leadstoretest.Q(t)
	var tailOrg int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT id FROM org WHERE domain = 'tailspin.example'`).Scan(&tailOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: tailOrg, Kind: leadstore.RouteForm, Value: "https://tailspin.example/contact",
		FetchID: 1, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}

	f := &fakeReoon{bal: verify.Balance{Daily: 20}, status: map[string]string{
		"info@northwind.example": verify.StatusSafe, "info@tailspin.example": verify.StatusInvalid,
		"office@litware.example": verify.StatusCatchAll, "hello@wingtip.example": verify.StatusUnknown,
	}}
	useReoon(t, f)
	out = mustRun(t, "leads", "verify")
	wantLines(t, "verify", out, "leads verify: checked 4 · sendable 2 · blocked 1 · waiting 1 · credits left 20 today, 0 one-off")
	if got := f.checked(); len(got) != 4 {
		t.Errorf("checked %v, want the 4 email addresses", got)
	}
	if n := batchCount(t, `SELECT count(*) FROM email_check`); n != 4 {
		t.Errorf("%d email_check rows, want 4", n)
	}
	if n := batchCount(t, `SELECT count(*) FROM run WHERE command = 'leads-verify' AND note LIKE 'checked 4 %' AND finished_at IS NOT NULL`); n != 1 {
		t.Errorf("verify run rows: %d", n)
	}
	// Re-run: every answer is fresh (the unknown is today's), so nothing is asked again.
	mustRun(t, "leads", "verify")
	if got := f.checked(); len(got) != 4 {
		t.Errorf("re-run checked again: %v", got)
	}

	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "gated", out, "1 lead waits for its email check.")
	if strings.Contains(out, "skipped tailspin") {
		t.Errorf("tailspin has a form, so it is not skipped:\n%s", out)
	}
	got := draftedRoutes(t, 3)
	wantGated := map[string]string{"northwind.example": "email info@northwind.example", "litware.example": "email office@litware.example",
		"tailspin.example": "form", "contoso.example": "form"}
	if len(got) != len(wantGated) {
		t.Errorf("gated batch %v, want %v", got, wantGated)
	}
	for d, r := range wantGated {
		if got[d] != r {
			t.Errorf("gated %s = %q, want %q", d, got[d], r)
		}
	}
	var ch string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT channel FROM lead WHERE org_id = $1`, tailOrg).Scan(&ch); err != nil || ch != leadstore.ChannelForm {
		t.Errorf("tailspin's lead channel %q (%v), want form: it was drafted on its form", ch, err)
	}
	sweepSendable(t, 3, verify.CatchAllSend)
	discardLatest(t)

	// Without the form, a blocked address is skipped with its reason. The next build puts the
	// lead back on its email route (the first email wins at build), then the form goes.
	mustRun(t, "leads", "build")
	if _, err := storetest.OwnerConn(t).Exec(ctx, `DELETE FROM contact_route WHERE org_id = $1 AND kind = 'form'`, tailOrg); err != nil {
		t.Fatal(err)
	}
	// catch_all blocked by the setting.
	cfgPath := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	blocked := strings.Replace(string(raw), "  catch_all: send", "  catch_all: block", 1)
	if blocked == string(raw) {
		t.Fatal("catch_all not rewritten")
	}
	if err := os.WriteFile(cfgPath, []byte(blocked), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	out = mustRun(t, "leads", "batch", "new")
	wantLines(t, "blocked", out, "skipped tailspin.example (unsupported, email): email check: invalid",
		"skipped litware.example (unsupported, email): email check: catch-all")
	got = draftedRoutes(t, 4)
	if len(got) != 2 || got["northwind.example"] != "email info@northwind.example" || got["contoso.example"] != "form" {
		t.Errorf("blocked batch %v", got)
	}
	sweepSendable(t, 4, verify.CatchAllBlock)
	discardLatest(t)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}

	// Off again: exactly the baseline, checks or not.
	setVerify(t, false)
	mustRun(t, "leads", "batch", "new")
	if got := draftedRoutes(t, 5); !maps.Equal(got, baseline) {
		t.Errorf("switched off: %v, want the baseline %v", got, baseline)
	}
}

// TestVerifyRunLimits: no credits left says so and checks nothing; --max and the daily cap
// (counted from today's checks) bound a run; a failed check leaves its address unchecked;
// switched off, live off and no key each make no call and exit 0.
func TestVerifyRunLimits(t *testing.T) {
	leadsScene(t)
	goLive(t)
	setVerify(t, true)
	all := map[string]string{
		"info@northwind.example": verify.StatusSafe, "info@tailspin.example": verify.StatusSafe,
		"office@litware.example": verify.StatusSafe, "hello@wingtip.example": verify.StatusSafe,
	}

	empty := &fakeReoon{status: all}
	useReoon(t, empty)
	if out := mustRun(t, "leads", "verify"); out != noCreditsLine+"\n" {
		t.Errorf("no credits: %q", out)
	}
	if len(empty.checked()) != 0 || empty.balances != 1 {
		t.Errorf("no credits: checks %v, balance reads %d", empty.checked(), empty.balances)
	}
	if n := batchCount(t, `SELECT count(*) FROM run WHERE command = 'leads-verify' AND note = 'No Reoon credits left today.'`); n != 1 {
		t.Errorf("no-credit run rows: %d", n)
	}

	f := &fakeReoon{status: all, bal: verify.Balance{Instant: 50}}
	useReoon(t, f)
	mustRun(t, "leads", "verify", "--max", "1")
	if len(f.checked()) != 1 {
		t.Errorf("--max 1 checked %v", f.checked())
	}
	t.Setenv(reoonDailyCapEnv, "2") // one checked today already: room for one more
	mustRun(t, "leads", "verify")
	if len(f.checked()) != 2 {
		t.Errorf("daily cap 2 checked %v in all", f.checked())
	}
	mustRun(t, "leads", "verify")
	if len(f.checked()) != 2 {
		t.Errorf("cap used, yet checked %v", f.checked())
	}
	t.Setenv(reoonDailyCapEnv, "")
	delete(f.status, "hello@wingtip.example") // its check fails
	out := mustRun(t, "leads", "verify")
	wantLines(t, "failure", out, "hello@wingtip.example not checked: verify: reoon /api/v1/verify: HTTP 500", "checked 1 · sendable 1 · blocked 0 · waiting 0 · 1 failed")
	if n := batchCount(t, `SELECT count(*) FROM email_check WHERE address = 'hello@wingtip.example'`); n != 0 {
		t.Errorf("a failed check stored %d rows", n)
	}
	before := len(f.checked())

	t.Setenv("LEADHOUND_LIVE", "")
	wantLines(t, "live off", mustRun(t, "leads", "verify"), "no Reoon call made")
	t.Setenv("LEADHOUND_LIVE", "1")
	setVerify(t, false)
	wantLines(t, "switched off", mustRun(t, "leads", "verify"), "Email check is switched off in the console")
	setVerify(t, true)
	saved := newReoonClient
	newReoonClient = func(bool) (reoonClient, error) { return nil, verify.ErrNoKey }
	wantLines(t, "no key", mustRun(t, "leads", "verify"), "leads verify: nothing was checked, because the Reoon key is not at /etc/leadhound/reoon.key")
	newReoonClient = saved
	if len(f.checked()) != before {
		t.Errorf("calls made while off, live off or keyless: %v", f.checked()[before:])
	}
}

// TestVerifyConsole: the Email check row on Controls says when it is off, counts today's
// checks and shows the credits the last run left (or that it found no key); each email route
// on the lead page shows its latest check; the catch-all choice is saved on Settings, checked
// with leads.yaml's own words when wrong, and reset to the file.
func TestVerifyConsole(t *testing.T) {
	leadsScene(t)
	goLive(t)
	setVerify(t, true)
	stubSystemctl(t, systemctlSample, nil)
	b := leadsBackend(t)
	ctx := context.Background()
	row := func() console.ControlRow {
		t.Helper()
		p, err := b.Controls(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Jobs {
			if r.Job == leadstore.JobVerify {
				return r
			}
		}
		t.Fatal("no Email check row")
		return console.ControlRow{}
	}

	r := row()
	if !r.On || r.Timer.Unit != unitBatch || !slices.Equal(r.Lines, []string{"No address checked today."}) || r.LastRun != nil {
		t.Errorf("cold start: %+v", r)
	}
	setVerify(t, false)
	if r = row(); r.On || len(r.Lines) == 0 || r.Lines[0] != emailCheckOffLine {
		t.Errorf("off: %+v", r)
	}
	setVerify(t, true)

	t.Setenv("LEADHOUND_LIVE", "1")
	saved := newReoonClient
	newReoonClient = func(bool) (reoonClient, error) { return nil, verify.ErrNoKey }
	mustRun(t, "leads", "verify")
	newReoonClient = saved
	if r = row(); r.LastRun == nil || r.LastRun.Result != "No Reoon key" || !r.LastRun.Failed ||
		!strings.Contains(strings.Join(r.Lines, "\n"), "the Reoon key is not on the server yet") {
		t.Errorf("no key: %+v %+v", r, r.LastRun)
	}

	useReoon(t, &fakeReoon{bal: verify.Balance{Daily: 18, Instant: 3}, status: map[string]string{
		"info@northwind.example": verify.StatusSafe, "info@tailspin.example": verify.StatusInvalid,
		"office@litware.example": verify.StatusCatchAll, "hello@wingtip.example": verify.StatusUnknown,
	}})
	mustRun(t, "leads", "verify")
	r = row()
	lines := strings.Join(r.Lines, "\n")
	// The run's start is the real clock's (a run row's started_at is the database's now()).
	for _, want := range []string{"Today: 4 addresses checked, 2 may be sent, 1 blocked.", "Credits left at the last run (", "): 18 today, 3 one-off."} {
		if !strings.Contains(lines, want) {
			t.Errorf("Controls lines lack %q:\n%s", want, lines)
		}
	}
	if r.LastRun == nil || r.LastRun.Result != "Finished" {
		t.Errorf("last run %+v", r.LastRun)
	}

	var leadID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = 'tailspin.example'`).Scan(&leadID); err != nil {
		t.Fatal(err)
	}
	d, err := b.LeadDetail(ctx, leadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Routes) != 1 || d.Routes[0].Check != "Email check: invalid, not sent" || d.Routes[0].CheckedAt == nil {
		t.Errorf("lead page routes %+v", d.Routes)
	}

	if err := b.SaveLeadSettings(ctx, console.SettingsEmailCheck, console.LeadSettingsValues{CatchAll: "maybe"}, false); err == nil ||
		!strings.Contains(err.Error(), `verify.catch_all: "maybe" is not one of send, block`) {
		t.Errorf("a wrong choice: %v", err)
	}
	if err := b.SaveLeadSettings(ctx, console.SettingsEmailCheck, console.LeadSettingsValues{CatchAll: console.CatchAllBlock}, false); err != nil {
		t.Fatal(err)
	}
	p, err := b.LeadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Values.CatchAll != console.CatchAllBlock || !p.Sources.CatchAll.Console {
		t.Errorf("saved: %q %+v", p.Values.CatchAll, p.Sources.CatchAll)
	}
	if r = row(); !strings.Contains(strings.Join(r.Lines, "\n"), "Today: 4 addresses checked, 1 may be sent, 2 blocked.") {
		t.Errorf("Controls after catch-all block: %q", r.Lines)
	}
	if err := b.SaveLeadSettings(ctx, console.SettingsEmailCheck, console.LeadSettingsValues{}, true); err != nil {
		t.Fatal(err)
	}
	if p, err = b.LeadSettings(ctx); err != nil || p.Values.CatchAll != console.CatchAllSend || p.Sources.CatchAll.Console {
		t.Errorf("reset: %q %+v %v", p.Values.CatchAll, p.Sources.CatchAll, err)
	}
}

// TestVerifyFallbackRoutes: a blocked address falls to a second address on the same domain,
// which is drafted; never to an address on another domain or to a suppressed one (those leads
// are skipped with the answer); two unknowns on two days block the address.
func TestVerifyFallbackRoutes(t *testing.T) {
	dir := leadsScene(t)
	goLive(t)
	setVerify(t, true)
	setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 40}]")
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	org := func(domain string) int64 {
		t.Helper()
		var id int64
		if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT id FROM org WHERE domain = $1`, domain).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	route := func(domain, email string) {
		t.Helper()
		if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: org(domain), Kind: leadstore.RouteEmail, Value: email,
			FetchID: 1, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(addr, status string, at time.Time) {
		t.Helper()
		if _, err := lq.AddEmailCheck(ctx, leadstore.EmailCheck{Address: addr, Provider: verify.Provider, Status: status, CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	at := leadsTuesday.Add(-time.Hour)
	// northwind: its address is invalid, a second one on its own domain is safe.
	route("northwind.example", "sales@northwind.example")
	check("info@northwind.example", verify.StatusInvalid, at)
	check("sales@northwind.example", verify.StatusSafe, at)
	// litware: the second address is on another domain.
	route("litware.example", "office@elsewhere.example")
	check("office@litware.example", verify.StatusInvalid, at)
	check("office@elsewhere.example", verify.StatusSafe, at)
	// wingtip: the second address is suppressed.
	route("wingtip.example", "team@wingtip.example")
	if _, err := lq.Suppress(ctx, 0, leadstore.SuppressAddress, "team@wingtip.example", leadstore.ReasonOptOut, "test"); err != nil {
		t.Fatal(err)
	}
	check("hello@wingtip.example", verify.StatusDisabled, at)
	check("team@wingtip.example", verify.StatusSafe, at)
	// tailspin: unknown yesterday and again today.
	check("info@tailspin.example", verify.StatusUnknown, leadsTuesday.Add(-30*time.Hour))
	check("info@tailspin.example", verify.StatusUnknown, at)

	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "fallbacks", out,
		"skipped litware.example (unsupported, email): email check: invalid",
		"skipped wingtip.example (agency_partner, email): email check: disabled",
		"skipped tailspin.example (unsupported, email): email check: unknown")
	if strings.Contains(out, "wait for") || strings.Contains(out, "waits for") {
		t.Errorf("nothing should wait:\n%s", out)
	}
	got := draftedRoutes(t, 1)
	if len(got) != 2 || got["northwind.example"] != "email sales@northwind.example" || got["contoso.example"] != "form" {
		t.Errorf("drafted %v, want northwind on its second address and the form lead", got)
	}
	sweepSendable(t, 1, verify.CatchAllSend)
	var ch, to string
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT l.channel, r.value FROM lead l JOIN contact_route r ON r.id = l.route_id WHERE l.org_id = $1`, org("northwind.example")).Scan(&ch, &to); err != nil ||
		ch != leadstore.ChannelEmail || to != "sales@northwind.example" {
		t.Errorf("northwind's lead is on %s %s (%v), want its second address", ch, to, err)
	}
}

// TestVerifyOnlyCampaignsWithMailboxes: a campaign with no mailbox assigned drafts no live
// batch, so leads verify spends no credit on its addresses.
func TestVerifyOnlyCampaignsWithMailboxes(t *testing.T) {
	leadsScene(t)
	goLive(t)
	setVerify(t, true)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	f := &fakeReoon{bal: verify.Balance{Daily: 20}, status: map[string]string{
		"info@northwind.example": verify.StatusSafe, "info@tailspin.example": verify.StatusSafe,
		"office@litware.example": verify.StatusSafe, "hello@wingtip.example": verify.StatusSafe,
	}}
	useReoon(t, f)
	cms, err := lq.CampaignMailboxes(ctx, leadstore.DefaultCampaignID)
	if err != nil || len(cms) == 0 {
		t.Fatalf("the scene's mailbox is not assigned: %v %v", cms, err)
	}
	for _, cm := range cms {
		if _, err := lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, cm.MailboxID, false, leadstore.ActorGremy); err != nil {
			t.Fatal(err)
		}
	}
	mustRun(t, "leads", "verify")
	if got := f.checked(); len(got) != 0 {
		t.Errorf("no mailbox assigned, yet checked %v", got)
	}
	if _, err := lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, cms[0].MailboxID, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "verify")
	if got := f.checked(); len(got) != 4 {
		t.Errorf("assigned again, checked %v, want the 4 addresses", got)
	}
}
