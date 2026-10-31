//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// noSystemd makes systemctl fail, as on a machine without it, and every /proc read empty.
func noSystemd(t *testing.T) {
	t.Helper()
	stubSystemctl(t, "", errors.New("no systemd here"))
	saved := procFile
	t.Cleanup(func() { procFile = saved })
	procFile = func(string) string { return "" }
}

// statusSystemctl answers systemctl show for the units asked: the send timer enabled with
// its next run at next, the backup timer loaded and disabled, every other unit unknown.
func statusSystemctl(t *testing.T, next time.Time) {
	t.Helper()
	saved := systemctlShow
	t.Cleanup(func() { systemctlShow = saved })
	systemctlShow = func(_ context.Context, units []string) ([]byte, error) {
		var b strings.Builder
		for _, u := range units {
			switch u {
			case unitSend:
				fmt.Fprintf(&b, "NextElapseUSecRealtime=@%d\nId=%s\nLoadState=loaded\nActiveState=active\nUnitFileState=enabled\n\n", next.Unix(), u)
			case "leadhound-backup.timer":
				fmt.Fprintf(&b, "NextElapseUSecRealtime=\nId=%s\nLoadState=loaded\nActiveState=inactive\nUnitFileState=disabled\n\n", u)
			default:
				fmt.Fprintf(&b, "NextElapseUSecRealtime=\nId=%s\nLoadState=not-found\nActiveState=inactive\nUnitFileState=\n\n", u)
			}
		}
		return []byte(b.String()), nil
	}
}

// coldStatus checks a Status from an empty database: every area never read.
func coldStatus(t *testing.T, st console.Status) {
	t.Helper()
	if st.Mailboxes.UpdatedAt != nil || len(st.Mailboxes.Rows) != 0 {
		t.Errorf("mailboxes %+v", st.Mailboxes)
	}
	if st.Server.UpdatedAt != nil {
		t.Errorf("server read at %v", st.Server.UpdatedAt)
	}
	if len(st.Server.Units) != len(statusUnits) {
		t.Errorf("%d units; want %d", len(st.Server.Units), len(statusUnits))
	}
	for _, u := range st.Server.Units {
		if u.LastRun != nil || u.Result != console.UnitNever || u.Next != nil || u.Switch != console.SwitchUnknown {
			t.Errorf("unit %+v", u)
		}
	}
	if st.Server.Load != nil || st.Server.MemoryPct != nil {
		t.Errorf("load %v memory %v without /proc", st.Server.Load, st.Server.MemoryPct)
	}
	c := st.Claude
	if c.UpdatedAt != nil || c.TodayMicros != 0 || c.MonthMicros != 0 || c.Topup != nil || c.LastErrorAt != nil || c.DailyCapMicros <= 0 {
		t.Errorf("claude %+v", c)
	}
	r := st.Reoon
	if r.FetchedAt != nil || r.LastErrorAt != nil || r.RefreshAskedAt != nil || r.ChecksToday.Total() != 0 {
		t.Errorf("reoon %+v", r)
	}
	if st.Jev.UpdatedAt != nil || st.Jev.JobsCalls != 0 || st.Jev.LeadsCalls != 0 {
		t.Errorf("jev %+v", st.Jev)
	}
}

// TestStatusColdStart: an empty database, with leads set up here and without, gives a Status
// with every area never read, not an error; so does a machine without systemd or /proc.
func TestStatusColdStart(t *testing.T) {
	ctx := context.Background()
	t.Run("no leads", func(t *testing.T) {
		leadstoretest.Reset(t)
		noSystemd(t)
		st, err := testBackend(t).Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		coldStatus(t, st)
	})
	t.Run("leads set up", func(t *testing.T) {
		leadsDB(t)
		t.Setenv("LEADHOUND_TZ", "Europe/London")
		noSystemd(t)
		st, err := leadsBackend(t).Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		coldStatus(t, st)
	})
}

// statusWorld is the send world (one checked mailbox, 10 a day, Tue 27 Oct 2026 10:00
// London) with Claude and Jev spend, a top-up, email checks, a Reoon reading and runs.
func statusWorld(t *testing.T) (*sendWorld, *backend) {
	t.Helper()
	ctx := context.Background()
	w := newSendWorld(t, true, 10)
	b := leadsBackend(t)
	owner := storetest.OwnerConn(t)
	for _, q := range []string{
		// Claude: today 0.30 drafting + 0.10 research; 0.05 earlier this month (after the
		// top-up); 0.007 last month.
		`INSERT INTO ai_usage (provider, model, day, calls, input_tokens, purpose, cost_micros, created_at) VALUES
		   ('anthropic', 'claude-test', '2026-10-27', 1, 10, 'drafting', 300000, '2026-10-27 09:00Z'),
		   ('anthropic', 'claude-test', '2026-10-27', 1, 10, 'research', 100000, '2026-10-27 09:10Z'),
		   ('anthropic', 'claude-test', '2026-10-02', 1, 10, 'drafting', 50000, '2026-10-02 09:00Z'),
		   ('anthropic', 'claude-test', '2026-09-30', 1, 10, 'drafting', 7000, '2026-09-30 09:00Z')`,
		// Jev: today 3 jobs calls and 4 leads calls.
		`INSERT INTO ai_usage (provider, model, day, calls, input_tokens, purpose, created_at) VALUES
		   ('jev', 'jev-test', '2026-10-27', 3, 100, 'jobs', '2026-10-27 08:00Z'),
		   ('jev', 'jev-test', '2026-10-27', 4, 100, 'leads', '2026-10-27 08:30Z')`,
		// Email checks: today two valid, one invalid, one catch-all; one yesterday.
		`INSERT INTO email_check (address, provider, status, checked_at) VALUES
		   ('a@alpha.example', 'reoon', 'safe', '2026-10-27 08:00Z'),
		   ('b@alpha.example', 'reoon', 'role_account', '2026-10-27 08:01Z'),
		   ('c@alpha.example', 'reoon', 'invalid', '2026-10-27 08:02Z'),
		   ('d@alpha.example', 'reoon', 'catch_all', '2026-10-27 08:03Z'),
		   ('e@alpha.example', 'reoon', 'safe', '2026-10-26 08:00Z')`,
		// Runs: an hourly send, a Send now and a failed LinkedIn collect.
		`INSERT INTO run (command, source, started_at, finished_at, note) VALUES
		   ('leads-send', 'outreach', '2026-10-27 08:00Z', '2026-10-27 08:00:41Z', '2 sent'),
		   ('leads-send', 'outreach', '2026-10-27 09:00Z', '2026-10-27 09:02Z', '1 sent · notes send now')`,
		`INSERT INTO run (command, source, started_at, finished_at, stop_reason) VALUES
		   ('collect', 'linkedin', '2026-10-27 07:00Z', '2026-10-27 07:05Z', 'blocked')`,
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	lq := w.lq
	if err := lq.RecordServiceError(ctx, leadstore.ServiceReoon, "verify: Reoon answered 500\nmore", time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := lq.RecordServiceOK(ctx, leadstore.ServiceReoon, []byte(`{"api_status":"active","daily":150,"instant":20}`), time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := lq.RecordServiceError(ctx, leadstore.ServiceClaude, "rate limited", time.Date(2026, 10, 27, 9, 20, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddClaudeTopup(ctx, 5000, time.Date(2026, 10, 2, 0, 0, 0, 0, b.loc)); err != nil {
		t.Fatal(err)
	}
	return w, b
}

// TestStatusFigures: every area's figures over the real store.
func TestStatusFigures(t *testing.T) {
	ctx := context.Background()
	w, b := statusWorld(t)
	next := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	statusSystemctl(t, next)
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := w.clock.now()

	c := st.Claude
	if c.TodayMicros != 400000 || c.MonthMicros != 450000 || c.SinceTopupMicros != 450000 || c.DailyCapMicros != b.cfg.ClaudeDailyCapMicros {
		t.Errorf("claude spend %+v", c)
	}
	if c.Topup == nil || c.Topup.AmountCents != 5000 {
		t.Errorf("top-up %+v", c.Topup)
	}
	if c.UpdatedAt == nil || !c.UpdatedAt.Equal(time.Date(2026, 10, 27, 9, 10, 0, 0, time.UTC)) {
		t.Errorf("claude updated %v", c.UpdatedAt)
	}
	if c.LastError != "rate limited" || c.LastErrorAt == nil {
		t.Errorf("claude error %q %v", c.LastError, c.LastErrorAt)
	}
	if fmt.Sprint(c.ByPurpose) != "[{drafting 300000 3000000} {research 100000 1500000} {drafts_eval 0 2000000}]" {
		t.Errorf("by purpose %v", c.ByPurpose)
	}

	r := st.Reoon
	if r.FetchedAt == nil || r.APIStatus != "active" || r.DailyCredits != 150 || r.InstantCredits != 20 {
		t.Errorf("reoon reading %+v", r)
	}
	if r.LastError != "verify: Reoon answered 500" || r.LastErrorAt == nil {
		t.Errorf("reoon error %q", r.LastError)
	}
	if r.ChecksToday != (console.ReoonChecks{Valid: 2, Invalid: 1, Other: 1}) {
		t.Errorf("checks %+v", r.ChecksToday)
	}

	if st.Jev.JobsCalls != 3 || st.Jev.LeadsCalls != 4 || st.Jev.UpdatedAt == nil || st.Jev.JobsCap != b.cfg.JevDailyCallCap {
		t.Errorf("jev %+v", st.Jev)
	}

	if len(st.Mailboxes.Rows) != 1 {
		t.Fatalf("mailboxes %+v", st.Mailboxes.Rows)
	}
	m := st.Mailboxes.Rows[0]
	if m.ID != w.mailbox.ID || m.Address != w.mailbox.Address || m.State != console.StatusMailboxActive || m.RampToday != 10 {
		t.Errorf("mailbox %+v", m)
	}

	if st.Server.UpdatedAt == nil || !st.Server.UpdatedAt.Equal(now) || st.Server.DBBytes <= 0 || st.Server.DBMaxBytes != b.cfg.DBMaxBytes {
		t.Errorf("server %+v", st.Server)
	}
	units := map[string]console.UnitStatus{}
	for _, u := range st.Server.Units {
		units[u.Name] = u
	}
	if u := units["leads-send"]; u.Result != console.UnitOK || u.Note != "41 s" || u.Switch != console.SwitchOn ||
		u.Next == nil || !u.Next.Equal(next) || u.LastRun == nil || !u.LastRun.Equal(time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("leads-send %+v", u)
	}
	if u := units["leads-send-now"]; u.Result != console.UnitOK || u.LastRun == nil || !u.LastRun.Equal(time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("leads-send-now %+v", u)
	}
	if u := units["collect"]; u.Result != console.UnitFailed || !strings.Contains(u.Note, "blocked") {
		t.Errorf("collect %+v", u)
	}
	if u := units["leads-verify-balance"]; u.Result != console.UnitOK || u.LastRun == nil || !u.LastRun.Equal(*r.FetchedAt) {
		t.Errorf("balance %+v", u)
	}
	if u := units["backup"]; u.Switch != console.SwitchOff || u.Result != console.UnitNever {
		t.Errorf("backup %+v", u)
	}
	if u := units["ats"]; u.Switch != console.SwitchUnknown || u.Result != console.UnitNever {
		t.Errorf("ats %+v", u)
	}
}

// TestStatusMatchesDashboard: the dashboard's Claude, Reoon and Jev figures are the Status
// page's, from the same state.
func TestStatusMatchesDashboard(t *testing.T) {
	ctx := context.Background()
	_, b := statusWorld(t)
	noSystemd(t)
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d, err := b.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.ClaudeMicros != st.Claude.TodayMicros || d.ClaudeMonthMicros != st.Claude.MonthMicros || d.ClaudeCapMicros != st.Claude.DailyCapMicros {
		t.Errorf("claude: dashboard %d/%d/%d, status %d/%d/%d", d.ClaudeMicros, d.ClaudeMonthMicros, d.ClaudeCapMicros,
			st.Claude.TodayMicros, st.Claude.MonthMicros, st.Claude.DailyCapMicros)
	}
	for _, p := range d.ClaudeByPurpose {
		i := slices.IndexFunc(st.Claude.ByPurpose, func(c console.PurposeCap) bool { return c.Purpose == p.Purpose })
		if i < 0 || st.Claude.ByPurpose[i].Micros != p.Micros {
			t.Errorf("purpose %s: dashboard %d, status %+v", p.Purpose, p.Micros, st.Claude.ByPurpose)
		}
	}
	if d.ReoonChecksToday != st.Reoon.ChecksToday.Total() || d.ReoonChecksToday != 4 || d.ReoonDailyLimit != defaultReoonDailyCap {
		t.Errorf("reoon: dashboard %d of %d, status %d", d.ReoonChecksToday, d.ReoonDailyLimit, st.Reoon.ChecksToday.Total())
	}
	if d.JobsCalls != st.Jev.JobsCalls || d.LeadsCalls != st.Jev.LeadsCalls || d.JobsCallCap != st.Jev.JobsCap || d.LeadsCallCap != st.Jev.LeadsCap {
		t.Errorf("jev: dashboard %d/%d %d/%d, status %+v", d.JobsCalls, d.JobsCallCap, d.LeadsCalls, d.LeadsCallCap, st.Jev)
	}
	// The leads card's sends and room are the Leads page's.
	lp, err := b.Leads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Leads == nil || d.Leads.CapToday != lp.Sending.CapToday || d.Leads.SentToday != lp.Sending.SentToday {
		t.Errorf("leads card %+v; Leads page sending %+v", d.Leads, lp.Sending)
	}
	if d.NeedsMeTotal != 0 || len(d.NeedsMe) != 0 {
		t.Errorf("needs me %d %+v", d.NeedsMeTotal, d.NeedsMe)
	}
}

// TestDashboardLeadsWidgets: a replied lead with nothing scheduled is waiting on the
// operator, and a campaign's sends show in its funnel.
func TestDashboardLeadsWidgets(t *testing.T) {
	ctx := context.Background()
	w, b := statusWorld(t)
	noSystemd(t)
	id := w.lead(t, "alpha.example")
	if _, err := storetest.OwnerConn(t).Exec(ctx,
		`UPDATE lead SET state = 'replied', state_at = '2026-10-27 09:00Z' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	d, err := b.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.NeedsMeTotal != 1 || len(d.NeedsMe) != 1 || d.NeedsMe[0].LeadID != id || d.NeedsMe[0].What != "Replied · nothing scheduled" {
		t.Errorf("needs me %d %+v", d.NeedsMeTotal, d.NeedsMe)
	}
}

// TestRefreshStatus: a request is written where the balance unit's watcher looks, refused
// while one waits, taken again once answered; refused without leads set up.
func TestRefreshStatus(t *testing.T) {
	ctx := context.Background()
	t.Run("not set up", func(t *testing.T) {
		leadstoretest.Reset(t)
		_, err := testBackend(t).RefreshStatus(ctx)
		if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Leads are not set up") {
			t.Fatalf("%v", err)
		}
	})
	w, b := statusWorld(t)
	noSystemd(t)
	out, err := b.RefreshStatus(ctx)
	if err != nil || out.Message != "Asked for a fresh Reoon balance. It shows here within a minute." {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(w.dir, runRequestDir, reoonBalanceRequest)); err != nil {
		t.Fatal(err)
	}
	st, err := b.Status(ctx)
	if err != nil || st.Reoon.RefreshAskedAt == nil || !st.Reoon.RefreshAskedAt.Equal(w.clock.now()) {
		t.Fatalf("asked %v, %v", st.Reoon.RefreshAskedAt, err)
	}
	if _, err := b.RefreshStatus(ctx); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "already asked") {
		t.Fatalf("second ask: %v", err)
	}
	// Answered: the balance unit stores a reading after the request.
	if err := w.lq.RecordServiceOK(ctx, leadstore.ServiceReoon, []byte(`{"api_status":"active","daily":149,"instant":20}`),
		w.clock.now().Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if st, err := b.Status(ctx); err != nil || st.Reoon.RefreshAskedAt != nil || st.Reoon.DailyCredits != 149 {
		t.Fatalf("answered: %+v %v", st.Reoon, err)
	}
	if _, err := b.RefreshStatus(ctx); err != nil {
		t.Fatalf("after the answer: %v", err)
	}
}

// TestAddClaudeTopup: a top-up is recorded and said back in dollars and the day.
func TestAddClaudeTopup(t *testing.T) {
	ctx := context.Background()
	leadstoretest.Reset(t)
	b := testBackend(t)
	at := time.Date(2026, 10, 31, 0, 0, 0, 0, b.loc)
	out, err := b.AddClaudeTopup(ctx, 5000, at)
	if err != nil || out.Message != "Recorded a $50.00 top-up on 31 Oct." {
		t.Fatalf("%+v %v", out, err)
	}
	got, err := leadstoretest.Q(t).LatestClaudeTopup(ctx)
	if err != nil || got.AmountCents != 5000 || !got.At.Equal(at) {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := b.AddClaudeTopup(ctx, 0, at); !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("zero: %v", err)
	}
}
