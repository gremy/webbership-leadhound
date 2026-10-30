//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// sendTimerOn is systemctlSample with the send timer enabled, next at 11:00 London on the
// send world's Tuesday.
var sendTimerOn = strings.Replace(systemctlSample, `Id=leadhound-leads-send.timer
LoadState=not-found
ActiveState=inactive
UnitFileState=`, `NextElapseUSecRealtime=@1793098800
Id=leadhound-leads-send.timer
LoadState=loaded
ActiveState=active
UnitFileState=enabled`, 1)

// controlsWorld is a live send world (newSendWorld, 10 a day) with the send timer on, two
// leads approved in one batch from its mailbox, and the console backend on its clock.
func controlsWorld(t *testing.T, capPerDay int) (*sendWorld, *backend) {
	t.Helper()
	w := newSendWorld(t, true, capPerDay)
	stubSystemctl(t, sendTimerOn, nil)
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example", w.lead(t, "beta.example"): "beta.example"})
	return w, leadsBackend(t)
}

func sentence(t *testing.T, b *backend) string {
	t.Helper()
	w, err := b.whyNotSending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := b.SendingWhy(context.Background()); err != nil || v.Sentence != w.Sentence {
		t.Fatalf("SendingWhy %+v, %v; whyNotSending %q", v, err, w.Sentence)
	}
	return w.Sentence
}

// TestWhyNotSendingStore: the sentence over the real store, one cell per reason that reads
// stored rows, each from the same start: two approved emails due now on the seeded mailbox.
func TestWhyNotSendingStore(t *testing.T) {
	ctx := context.Background()
	t.Run("goes", func(t *testing.T) {
		_, b := controlsWorld(t, 10)
		if got := sentence(t, b); got != "2 approved emails go out at the next run, Tue 27 Oct 11:00." {
			t.Errorf("%q", got)
		}
	})
	t.Run("shadow", func(t *testing.T) {
		_, b := controlsWorld(t, 10)
		t.Setenv("LEADHOUND_OUTREACH", "")
		if got := sentence(t, b); !strings.HasPrefix(got, "Shadow mode:") {
			t.Errorf("%q", got)
		}
	})
	t.Run("sending off", func(t *testing.T) {
		w, b := controlsWorld(t, 10)
		if _, err := setSendPause(ctx, w.lq, true, "café first", leadstore.ActorGremy); err != nil {
			t.Fatal(err)
		}
		if got := sentence(t, b); !strings.HasPrefix(got, "Sending is off since ") || !strings.HasSuffix(got, ": café first.") {
			t.Errorf("%q", got)
		}
	})
	t.Run("no send timer", func(t *testing.T) {
		_, b := controlsWorld(t, 10)
		stubSystemctl(t, systemctlSample, nil)
		if got := sentence(t, b); got != "The send timer isn't enabled on the server." {
			t.Errorf("%q", got)
		}
	})
	t.Run("no send timer, inferred", func(t *testing.T) {
		_, b := controlsWorld(t, 10)
		stubSystemctl(t, "", errors.New("no systemd"))
		t.Setenv("LEADHOUND_LEADS_TIMERS", "")
		if got := sentence(t, b); got != "The send timer isn't enabled on the server." {
			t.Errorf("%q", got)
		}
		t.Setenv("LEADHOUND_LEADS_TIMERS", "on")
		if got := sentence(t, b); got != "2 approved emails go out at the next send run." {
			t.Errorf("inferred on: %q", got)
		}
	})
	t.Run("nothing approved", func(t *testing.T) {
		newSendWorld(t, true, 10)
		stubSystemctl(t, sendTimerOn, nil)
		if got := sentence(t, leadsBackend(t)); got != "Nothing approved is waiting to go out." {
			t.Errorf("%q", got)
		}
	})
	t.Run("mailbox retired: its emails are cancelled, not waiting", func(t *testing.T) {
		w, b := controlsWorld(t, 10)
		if err := w.lq.SetMailboxState(ctx, w.mailbox.ID, leadstore.MailboxRetired, "gone", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
		if got := sentence(t, b); got != "Nothing approved is waiting to go out." {
			t.Errorf("%q", got)
		}
	})
	t.Run("a lead that may no longer be written to", func(t *testing.T) {
		_, b := controlsWorld(t, 10)
		if _, err := storetest.OwnerConn(t).Exec(ctx,
			`UPDATE lead SET state = 'replied' WHERE org_id = (SELECT id FROM org WHERE domain = 'beta.example')`); err != nil {
			t.Fatal(err)
		}
		if got := sentence(t, b); got != "1 approved email goes out at the next run, Tue 27 Oct 11:00." {
			t.Errorf("%q", got)
		}
	})
	t.Run("mailbox paused", func(t *testing.T) {
		w, b := controlsWorld(t, 10)
		if err := w.lq.SetMailboxState(ctx, w.mailbox.ID, leadstore.MailboxPaused, "two bounces", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
		if got := sentence(t, b); got != "2 approved emails wait on "+testUser+": it is paused (two bounces)." {
			t.Errorf("%q", got)
		}
	})
	t.Run("not assigned", func(t *testing.T) {
		w, b := controlsWorld(t, 10)
		if _, err := w.lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, w.mailbox.ID, false, leadstore.ActorGremy); err != nil {
			t.Fatal(err)
		}
		name := campaignName(t, w.lq, leadstore.DefaultCampaignID)
		if got := sentence(t, b); got != "2 approved emails wait: "+testUser+" is not assigned to "+name+"." {
			t.Errorf("%q", got)
		}
	})
	t.Run("warm-up limit used", func(t *testing.T) {
		w, b := controlsWorld(t, 1)
		w.mustRun(t, "leads", "send")
		if got := sentence(t, b); got != "Today's warm-up limit is used (1 of 1); the next send is Wed 28 Oct 09:30." {
			t.Errorf("%q", got)
		}
	})
	t.Run("outside the sending window", func(t *testing.T) {
		w, b := controlsWorld(t, 10)
		w.clock.set(time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC))
		if got := sentence(t, b); got != "2 approved emails go out in the next sending window, Wed 28 Oct 09:30." {
			t.Errorf("%q", got)
		}
	})
	t.Run("one mailbox's emails", func(t *testing.T) {
		w, b := controlsWorld(t, 10)
		other := seedMailbox(t, w.lq, "hello@other.example")
		v, err := b.MailboxSending(ctx, w.mailbox.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Waiting != 2 || v.Why.Blocked || v.Why.Sentence != "2 approved emails go out at the next run, Tue 27 Oct 11:00." ||
			len(v.Campaigns) != 1 || v.Campaigns[0].Value != slugOfCampaign(t, w.lq, leadstore.DefaultCampaignID) {
			t.Errorf("mailbox %d: %+v", w.mailbox.ID, v)
		}
		v, err = b.MailboxSending(ctx, other.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Waiting != 0 || !v.Why.Blocked || v.Why.Sentence != "No approved email uses this mailbox." {
			t.Errorf("mailbox %d: %+v", other.ID, v)
		}
		if _, err := b.MailboxSending(ctx, 999); !errors.Is(err, console.ErrNotFound) {
			t.Errorf("no such mailbox: %v", err)
		}
	})
}

func campaignName(t *testing.T, lq leadstore.Queries, id int64) string {
	t.Helper()
	cs, err := lq.Campaigns(context.Background(), leadstore.DefaultAccountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.ID == id {
			return c.Name
		}
	}
	t.Fatalf("no campaign %d", id)
	return ""
}

func slugOfCampaign(t *testing.T, lq leadstore.Queries, id int64) string {
	t.Helper()
	cs, err := lq.Campaigns(context.Background(), leadstore.DefaultAccountID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.ID == id {
			return c.Slug
		}
	}
	t.Fatalf("no campaign %d", id)
	return ""
}

// TestControlsPageStore: the Controls page over the real store: every job on with no row,
// a switch set from the console shows off with its reason and time, a switched-off run reads
// "Switched off", the sending switch set by the conversion reads as the server's, the timers
// come from systemd, and the guards are the environment's.
func TestControlsPageStore(t *testing.T) {
	ctx := context.Background()
	w, b := controlsWorld(t, 10)
	p, err := b.Controls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Jobs) != len(console.ControlJobs) || !p.TimersRead || p.TimersNote != "" || p.Why.Blocked ||
		!p.Sending.On || p.Sending.Since != nil || !p.Sending.Timer.Enabled || p.Sending.Timer.Next == nil {
		t.Fatalf("cold start: %+v", p)
	}
	for i, j := range p.Jobs {
		if j.Job != console.ControlJobs[i] || !j.On || j.Since != nil || j.Name == "" {
			t.Errorf("cold start row %d: %+v", i, j)
		}
	}
	if p.Jobs[2].Timer.Unit != unitProbe || !p.Jobs[2].Timer.Enabled || p.Jobs[2].Timer.Next == nil || p.Jobs[0].Timer.Enabled {
		t.Errorf("timers: probe %+v, batch %+v", p.Jobs[2].Timer, p.Jobs[0].Timer)
	}
	if len(p.Guards) != 3 || p.Guards[0].Value != "live" || p.RatingPath != console.RatingSettingsPath {
		t.Errorf("guards %+v, rating %q", p.Guards, p.RatingPath)
	}

	out, err := b.ControlAct(ctx, console.ControlAction{Do: console.ControlSwitchOff, Job: leadstore.JobSync, Reason: "  mailbox moving  "})
	if err != nil || out.Message != "Inbox sync is switched off: its runs do nothing until you switch it on." {
		t.Fatalf("switch off: %+v, %v", out, err)
	}
	if out, err := b.ControlAct(ctx, console.ControlAction{Do: console.ControlSwitchOff, Job: leadstore.JobSync}); err != nil ||
		!strings.HasPrefix(out.Message, "Inbox sync is already off (since ") {
		t.Errorf("again: %+v, %v", out, err)
	}
	w.mustRun(t, "leads", "sync")
	if _, err := setSendPause(ctx, w.lq, true, convertSendReason, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	p, err = b.Controls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sync := p.Jobs[1]
	if sync.On || sync.Since == nil || sync.Reason != "mailbox moving" || sync.BySystem ||
		sync.LastRun == nil || sync.LastRun.Result != "Switched off" {
		t.Errorf("sync off: %+v (last run %+v)", sync, sync.LastRun)
	}
	if s := p.Sending; s.On || s.Since == nil || s.Reason != convertSendReason || !s.BySystem || !p.Why.Blocked {
		t.Errorf("sending paused by the conversion: %+v, why %+v", s, p.Why)
	}

	for _, bad := range []console.ControlAction{
		{Do: console.ControlSwitchOff, Job: "send"},
		{Do: console.ControlSwitchOff, Job: leadstore.JobBatch, Reason: "two\nlines"},
		{Do: console.ControlSwitchOff, Job: leadstore.JobBatch, Reason: strings.Repeat("é", console.ControlReasonMax+1)},
		{Do: "pause", Job: leadstore.JobBatch},
	} {
		if _, err := b.ControlAct(ctx, bad); !errors.Is(err, console.ErrUserFixable) {
			t.Errorf("%+v: %v, want refused", bad, err)
		}
	}
	if sw, err := w.lq.JobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobBatch); err != nil || !sw.On {
		t.Errorf("a refused switch changed the batch: %+v, %v", sw, err)
	}
}

// requestFile is the research request file's content under dir, "" when there is none.
func requestFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, runRequestDir, researchAction)) // #nosec G304 -- the test's own temp dir
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSwitchResearchOnRequests: switching research back on, from the console or the command
// line, writes its request file (its only trigger), so what a star queued while it was off
// runs then; switching it off, switching it on when it is on, and switching another job on
// write none.
func TestSwitchResearchOnRequests(t *testing.T) {
	ctx := context.Background()
	t.Run("console", func(t *testing.T) {
		w, b := controlsWorld(t, 10)
		act := func(do, job string) string {
			t.Helper()
			out, err := b.ControlAct(ctx, console.ControlAction{Do: do, Job: job})
			if err != nil {
				t.Fatal(err)
			}
			return out.Message
		}
		act(console.ControlSwitchOff, leadstore.JobResearch)
		act(console.ControlSwitchOff, leadstore.JobBatch)
		if got := act(console.ControlSwitchOn, leadstore.JobBatch); got != "Morning batch is switched on: its next run goes ahead." || requestFile(t, w.dir) != "" {
			t.Errorf("batch on: %q, request %q", got, requestFile(t, w.dir))
		}
		got := act(console.ControlSwitchOn, leadstore.JobResearch)
		if got != "Research is switched on: its next run goes ahead. What was queued while it was off starts now." || requestFile(t, w.dir) == "" {
			t.Errorf("research on: %q, request %q", got, requestFile(t, w.dir))
		}
		if err := os.Remove(filepath.Join(w.dir, runRequestDir, researchAction)); err != nil {
			t.Fatal(err)
		}
		if got := act(console.ControlSwitchOn, leadstore.JobResearch); got != "Research is already on." || requestFile(t, w.dir) != "" {
			t.Errorf("research on again: %q, request %q", got, requestFile(t, w.dir))
		}
	})
	t.Run("command line", func(t *testing.T) {
		w := newSendWorld(t, true, 10)
		w.mustRun(t, "leads", "switches", "set", "research", "off")
		if requestFile(t, w.dir) != "" {
			t.Fatal("switching off wrote a request")
		}
		out := w.mustRun(t, "leads", "switches", "set", "research", "on")
		wantLines(t, "on", out, "Research is switched on: its next run goes ahead. What was queued while it was off starts now.")
		if requestFile(t, w.dir) == "" {
			t.Error("switching research on wrote no request")
		}
	})
}

// TestControlsCampaignMailboxesStore: each campaign lists every mailbox not retired with its
// assignment; assign and unassign go through ControlAct with the store's refusals worded; the
// landing shows found (off the campaign's site when its host differs), didn't answer, or not
// checked for a check older than the landing URL.
func TestControlsCampaignMailboxesStore(t *testing.T) {
	ctx := context.Background()
	w, b := controlsWorld(t, 10)
	// The landing URL's cutoff is migration 24's goose tstamp: stamp it a minute ago, as a
	// fresh migration does, in the database's own zone (a timestamp with none), so a check
	// from just now must count on a database whose zone is not UTC.
	restampLandingCutoff(t)
	other := seedMailbox(t, w.lq, "hello@other.example", unassigned())
	gone := seedMailbox(t, w.lq, "gone@third.example", unassigned())
	if err := w.lq.SetMailboxState(ctx, gone.ID, leadstore.MailboxRetired, "old", leadstore.ScopeMailbox, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	slug := slugOfCampaign(t, w.lq, leadstore.DefaultCampaignID)
	find := func(t *testing.T) console.CampaignMailboxes {
		t.Helper()
		cs, err := b.CampaignMailboxes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cs {
			if c.Slug == slug {
				return c
			}
		}
		t.Fatalf("no campaign %s in %+v", slug, cs)
		return console.CampaignMailboxes{}
	}
	// seedMailbox's check passed with no landing URL: a check since the landing URL exists,
	// so the website didn't answer.
	c := find(t)
	if len(c.Rows) != 2 || c.Assigned != 1 || !c.Rows[0].Assigned || c.Rows[1].Assigned || c.Rows[1].MailboxID != other.ID ||
		c.Rows[1].Host != "other.example" || c.Rows[0].Landing.State != console.LandingNoAnswer {
		t.Fatalf("before: %+v", c)
	}

	out, err := b.ControlAct(ctx, console.ControlAction{Do: console.ControlAssign, Campaign: slug, MailboxID: other.ID})
	if err != nil || !strings.HasPrefix(out.Message, "hello@other.example is assigned to ") {
		t.Fatalf("assign: %+v, %v", out, err)
	}
	for _, bad := range []console.ControlAction{
		{Do: console.ControlAssign, Campaign: "no-such", MailboxID: other.ID},
		{Do: console.ControlAssign, Campaign: slug, MailboxID: 999},
		{Do: console.ControlAssign, Campaign: slug, MailboxID: gone.ID},
	} {
		if _, err := b.ControlAct(ctx, bad); !errors.Is(err, console.ErrUserFixable) {
			t.Errorf("%+v: %v, want refused", bad, err)
		}
	}

	// Landings: the seeded mailbox lands off-site, the other did not answer, and a check
	// finished before the landing URL existed reads as not checked.
	// finish stamps the check with the real clock, as the check job does: the landing URL's
	// cutoff is when its migration ran, on the real clock too.
	finish := func(id int64, landing *string) int64 {
		t.Helper()
		cid, err := w.lq.RequestCheck(ctx, id, leadstore.ActorOperator)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.lq.FinishCheck(ctx, cid, leadstore.MailboxCheckResult{Outcome: leadstore.CheckPassed, LandingURL: landing}, time.Now()); err != nil {
			t.Fatal(err)
		}
		return cid
	}
	elsewhere := "https://www.elsewhere.example/landing"
	finish(w.mailbox.ID, &elsewhere)
	cid := finish(other.ID, nil)
	c = find(t)
	if l := c.Rows[0].Landing; l.State != console.LandingFound || l.URL != elsewhere || l.Host != "www.elsewhere.example" ||
		l.At == nil || l.OffSite != (c.SiteHost != "" && c.SiteHost != "elsewhere.example") {
		t.Errorf("found: %+v (site %q)", l, c.SiteHost)
	}
	if l := c.Rows[1].Landing; l.State != console.LandingNoAnswer || !c.Rows[1].Assigned || c.Assigned != 2 {
		t.Errorf("no answer: %+v", c.Rows[1])
	}
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE mailbox_check SET checked_at = '2000-01-01' WHERE id = $1`, cid); err != nil {
		t.Fatal(err)
	}
	if l := find(t).Rows[1].Landing; l.State != console.LandingNotChecked {
		t.Errorf("a check from before the landing URL: %+v", l)
	}

	out, err = b.ControlAct(ctx, console.ControlAction{Do: console.ControlUnassign, Campaign: slug, MailboxID: other.ID})
	if err != nil || !strings.Contains(out.Message, "is no longer assigned to") {
		t.Errorf("unassign: %+v, %v", out, err)
	}
	if out, err := b.ControlAct(ctx, console.ControlAction{Do: console.ControlUnassign, Campaign: slug, MailboxID: other.ID}); err != nil ||
		!strings.HasPrefix(out.Message, "hello@other.example is not assigned to ") {
		t.Errorf("unassign again: %+v, %v", out, err)
	}
}

// restampLandingCutoff sets migration 24's goose tstamp to a minute ago, as now() writes it
// (local time, no zone), and puts the old stamps back after the test.
func restampLandingCutoff(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	rows, err := owner.Query(ctx, `SELECT id, tstamp FROM goose_db_version_leads WHERE version_id = 24`)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		saved[id] = at
	}
	if rows.Close(); rows.Err() != nil || len(saved) == 0 {
		t.Fatalf("migration 24's stamp: %v (%d rows)", rows.Err(), len(saved))
	}
	t.Cleanup(func() {
		for id, at := range saved {
			if _, err := owner.Exec(context.Background(), `UPDATE goose_db_version_leads SET tstamp = $2 WHERE id = $1`, id, at); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := owner.Exec(ctx, `UPDATE goose_db_version_leads SET tstamp = localtimestamp - interval '1 minute' WHERE version_id = 24`); err != nil {
		t.Fatal(err)
	}
}

// TestControlsSwitchWithBrokenSettings: a job switch from the console works while leads.yaml
// cannot be read: switching a job off is how a broken run is stopped, and research switched
// back on still writes its request file.
func TestControlsSwitchWithBrokenSettings(t *testing.T) {
	ctx := context.Background()
	w, b := controlsWorld(t, 10)
	if err := os.WriteFile(filepath.Join(w.dir, "leads.yaml"), []byte("not: [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Controls(ctx); !errors.Is(err, console.ErrLeadsNotSetUp) {
		t.Fatalf("the settings are not broken: %v", err)
	}
	for _, do := range []string{console.ControlSwitchOff, console.ControlSwitchOn} {
		if _, err := b.ControlAct(ctx, console.ControlAction{Do: do, Job: leadstore.JobResearch}); err != nil {
			t.Fatalf("research %s: %v", do, err)
		}
	}
	if requestFile(t, w.dir) == "" {
		t.Error("research switched on wrote no request")
	}
	if sw, err := w.lq.JobSwitch(ctx, leadstore.DefaultAccountID, leadstore.JobResearch); err != nil || !sw.On || sw.At.IsZero() {
		t.Errorf("research switch %+v, %v", sw, err)
	}
}
