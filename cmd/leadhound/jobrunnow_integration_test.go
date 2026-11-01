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
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// jobRunSystemd answers systemctl show for any units asked: timers and watchers loaded,
// enabled and active (a timer's next run an hour ahead of the send world's clock), services
// loaded and inactive, except where states names "ActiveState/UnitFileState" for a unit, or
// "not-found".
func jobRunSystemd(t *testing.T, states map[string]string) {
	t.Helper()
	saved := systemctlShow
	t.Cleanup(func() { systemctlShow = saved })
	systemctlShow = func(_ context.Context, units []string) ([]byte, error) {
		var b strings.Builder
		for _, u := range units {
			active, file := "active", "enabled"
			if strings.HasSuffix(u, ".service") {
				active, file = "inactive", "static"
			}
			load := "loaded"
			switch s := states[u]; {
			case s == "not-found":
				load, active, file = "not-found", "inactive", ""
			case s != "":
				active, file, _ = strings.Cut(s, "/")
			}
			next := ""
			if strings.HasSuffix(u, ".timer") && file == "enabled" {
				next = fmt.Sprintf("@%d", time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC).Unix())
			}
			fmt.Fprintf(&b, "NextElapseUSecRealtime=%s\nId=%s\nLoadState=%s\nActiveState=%s\nUnitFileState=%s\n\n", next, u, load, active, file)
		}
		return []byte(b.String()), nil
	}
}

// jobRunWorld is a live send world (Tue 27 Oct 2026, 10:00 UTC; London) with every timer and
// watcher armed, and the console backend on its clock.
func jobRunWorld(t *testing.T, states map[string]string) (*sendWorld, *backend) {
	t.Helper()
	w := newSendWorld(t, true, 10)
	jobRunSystemd(t, states)
	return w, leadsBackend(t)
}

// runFiles are the request files under dir's run/ folder, by name.
func runFiles(t *testing.T, dir string) map[string]os.FileInfo {
	t.Helper()
	out := map[string]os.FileInfo{}
	entries, err := os.ReadDir(filepath.Join(dir, runRequestDir))
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = fi
	}
	return out
}

// execRun runs one statement on the test database as the app.
func execRun(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := storetest.AppConn(t).Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// TestJobRunNowPress: from a cold start (no run/ folder), each job's confirmed press writes
// run/<its action> (the folder 0700, the file 0600) with the press time; a second press is
// refused as already asked and leaves the file as it was.
func TestJobRunNowPress(t *testing.T) {
	ctx := context.Background()
	w, b := jobRunWorld(t, nil)
	if len(runFiles(t, w.dir)) != 0 {
		t.Fatal("the world starts with request files")
	}
	for _, job := range console.RunNowJobs {
		u := jobRunUnits[job]
		out, err := b.JobRunNow(ctx, job, true)
		if err != nil || out.Message != fmt.Sprintf("Asked. %s is starting; this row follows it, and the next automatic run moves to 24 hours after it.", jobName(job)) {
			t.Fatalf("%s: %+v, %v", job, out, err)
		}
		fi := runFiles(t, w.dir)[u.action]
		if fi == nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: request file %v", job, fi)
		}
		at, err := readRunRequest(w.dir, u.action)
		if err != nil || at == nil || !at.Equal(w.clock.now()) {
			t.Errorf("%s: request time %v, %v", job, at, err)
		}
		if _, err := b.JobRunNow(ctx, job, true); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Already asked at ") {
			t.Errorf("%s: second press %v", job, err)
		}
		if again := runFiles(t, w.dir)[u.action]; !again.ModTime().Equal(fi.ModTime()) {
			t.Errorf("%s: the refused press rewrote the file", job)
		}
	}
	dir, err := os.Stat(filepath.Join(w.dir, runRequestDir))
	if err != nil || dir.Mode().Perm() != 0o700 {
		t.Errorf("run/: %v, %v", dir, err)
	}
	if got := slices.Sorted(func(yield func(string) bool) {
		for n := range runFiles(t, w.dir) {
			if !yield(n) {
				return
			}
		}
	}); !slices.Equal(got, []string{"leads-batch", "leads-contacts", "leads-probe"}) {
		t.Errorf("request files %v", got)
	}
	p, err := b.Controls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range p.Jobs {
		if row.RunNow != nil && (row.RunNow.State != "waiting" || !row.RunNow.Pending || row.RunNow.CanAsk) {
			t.Errorf("%s after the press: %+v", row.Job, row.RunNow)
		}
	}
}

// TestJobRunNowRefusalsWriteNothing: every refusal leaves no request file.
func TestJobRunNowRefusalsWriteNothing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, job string
		states    map[string]string
		confirmed bool
		prep      func(t *testing.T, w *sendWorld, b *backend)
		want      string
	}{
		{name: "not confirmed", job: console.JobProbe, want: "Tick the box to confirm"},
		{name: "systemd not read", job: console.JobProbe, confirmed: true, want: "The server's systemd could not be asked",
			prep: func(t *testing.T, _ *sendWorld, _ *backend) { stubSystemctl(t, "", errors.New("no systemd here")) }},
		{name: "switched off", job: console.JobContacts, confirmed: true, want: "Contacts is switched off on this page",
			prep: func(t *testing.T, _ *sendWorld, b *backend) {
				if _, err := b.ControlAct(ctx, console.ControlAction{Do: console.ControlSwitchOff, Job: leadstore.JobContacts}); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "watcher not armed", job: console.JobBatch, confirmed: true, states: map[string]string{watcherBatch: "inactive/disabled"},
			want: "The Morning batch Run now watcher is not armed"},
		{name: "watcher stopped", job: console.JobProbe, confirmed: true, states: map[string]string{watcherProbe: "failed/enabled"},
			want: "The Site probe Run now watcher has stopped"},
		{name: "already running", job: console.JobProbe, confirmed: true, states: map[string]string{serviceProbe: "activating/static"},
			want: "Already running since ",
			prep: func(t *testing.T, _ *sendWorld, _ *backend) {
				execRun(t, `INSERT INTO run (command, source, started_at, note) VALUES ($1, 'leads-probe', '2026-10-27 09:30Z', 'planned 2026-10-27T11:30:00+02:00')`, commandScheduled)
			}},
		{name: "the run's end being recorded", job: console.JobProbe, confirmed: true, states: map[string]string{serviceProbe: "deactivating/static"},
			want: "Already running since ",
			prep: func(t *testing.T, _ *sendWorld, _ *backend) {
				execRun(t, `INSERT INTO run (command, source, started_at, note) VALUES ($1, 'leads-probe', '2026-10-27 09:30Z', 'planned 2026-10-27T11:30:00+02:00')`, commandScheduled)
			}},
		{name: "contacts while the probe holds the lock", job: console.JobContacts, confirmed: true,
			want: "The site probe is running since ",
			prep: func(t *testing.T, _ *sendWorld, _ *backend) {
				execRun(t, `INSERT INTO run (command, source, started_at, note) VALUES ($1, 'leads-probe', '2026-10-27 09:57Z', 'asked at 2026-10-27T11:57:00+02:00')`, commandRunNow)
			}},
		{name: "the probe while contacts holds the lock", job: console.JobProbe, confirmed: true,
			want: "The contacts job is running since ",
			prep: func(t *testing.T, _ *sendWorld, _ *backend) {
				execRun(t, `INSERT INTO run (command, source, started_at) VALUES ($1, '', '2026-10-27 09:40Z')`, commandLeadsEnrich)
			}},
		{name: "batch waiting", job: console.JobBatch, confirmed: true, want: "A batch is already waiting for you to approve",
			prep: func(t *testing.T, w *sendWorld, _ *backend) {
				id := w.lead(t, "alpha.example")
				d := leadstore.DraftRow{LeadID: id, Step: 0, Channel: "email", To: "info@alpha.example", Subject: "Your Drupal site",
					Body: "Hello.\n", MessageID: "waiting@" + testSenderDomain, MailboxID: w.mailbox.ID, FromName: w.fromName}
				if _, _, err := leadstore.NewQueries(storetest.AppConn(t)).CreateBatch(ctx, false, []leadstore.DraftRow{d}, nil); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, b := jobRunWorld(t, tc.states)
			if tc.prep != nil {
				tc.prep(t, w, b)
			}
			out, err := b.JobRunNow(ctx, tc.job, tc.confirmed)
			if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%+v, %v; want refused with %q", out, err, tc.want)
			}
			if files := runFiles(t, w.dir); len(files) != 0 {
				t.Errorf("a refusal wrote %v", files)
			}
		})
	}
	t.Run("not a Run now job", func(t *testing.T) {
		w, b := jobRunWorld(t, nil)
		if _, err := b.JobRunNow(ctx, console.JobSync, true); !errors.Is(err, console.ErrUserFixable) || len(runFiles(t, w.dir)) != 0 {
			t.Errorf("sync: %v", err)
		}
	})
}

// TestJobRunSwitchOnWritesNoRequest: switching each Run now job off and on again, from the
// console and from the command line, never writes its request file: a switch-on must not start
// a long job unasked.
func TestJobRunSwitchOnWritesNoRequest(t *testing.T) {
	ctx := context.Background()
	w, b := jobRunWorld(t, nil)
	for _, job := range console.RunNowJobs {
		for _, do := range []string{console.ControlSwitchOff, console.ControlSwitchOn} {
			if _, err := b.ControlAct(ctx, console.ControlAction{Do: do, Job: job}); err != nil {
				t.Fatalf("%s %s: %v", job, do, err)
			}
		}
		w.mustRun(t, "leads", "switches", "set", job, "off")
		w.mustRun(t, "leads", "switches", "set", job, "on")
		if files := runFiles(t, w.dir); len(files) != 0 {
			t.Errorf("switching %s on wrote %v", job, files)
		}
	}
}

// TestJobRunControlsPage: the Run now block goes on exactly the RunNowJobs rows, the plan on
// those and the Email check row (all on guarded timers); a press at 15:02 Bucharest yesterday
// plans 15:00 today on Controls and Status alike, and 31 minutes past that plan with no start
// both say overdue. A disabled timer gets no plan.
func TestJobRunControlsPage(t *testing.T) {
	ctx := context.Background()
	w, b := jobRunWorld(t, map[string]string{unitSync: "inactive/disabled"})
	// 15:02 Bucharest on Mon 26 Oct (EET, UTC+2) is 13:02 UTC.
	execRun(t, `INSERT INTO run (command, source, started_at, finished_at, note)
		VALUES ($1, 'leads-contacts', '2026-10-26 13:02Z', '2026-10-26 18:00Z', 'asked at 2026-10-26T15:02:00+02:00 · ended success')`, commandRunNow)
	plan := time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)

	check := func(overdue bool) {
		t.Helper()
		p, err := b.Controls(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range p.Jobs {
			wantRunNow := slices.Contains(console.RunNowJobs, row.Job)
			wantPlanned := wantRunNow || row.Job == leadstore.JobVerify
			if (row.RunNow != nil) != wantRunNow || (row.Planned != nil) != wantPlanned {
				t.Errorf("%s: RunNow %+v, Planned %+v", row.Job, row.RunNow, row.Planned)
			}
			if row.RunNow != nil && (row.RunNow.State != "idle" || !row.RunNow.CanAsk || row.RunNow.Confirm == "") {
				t.Errorf("%s: %+v", row.Job, row.RunNow)
			}
			if row.Job == leadstore.JobContacts && (row.Planned == nil || !row.Planned.At.Equal(plan) || row.Planned.Overdue != overdue) {
				t.Errorf("contacts planned %+v, want %s overdue %v", row.Planned, plan, overdue)
			}
		}
		st, err := b.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range st.Server.Units {
			key, guarded := guardKeyOfTimer(map[string]string{"leads-verify": unitBatch}[u.Name])
			if !guarded {
				key, guarded = guardKeyOfTimer(guardTimer(u.Name))
			}
			if (u.Planned != nil) != guarded {
				t.Errorf("status %s (%s): Planned %+v", u.Name, key, u.Planned)
			}
			if u.Name == "leads-contacts" && (u.Planned == nil || !u.Planned.At.Equal(plan) || u.Planned.Overdue != overdue) {
				t.Errorf("status contacts planned %+v, want %s overdue %v", u.Planned, plan, overdue)
			}
			if u.Planned != nil && u.Planned.Overdue && u.Name != "leads-contacts" {
				t.Errorf("status %s overdue: %+v", u.Name, u.Planned)
			}
		}
	}
	check(false)
	w.clock.set(plan.Add(31 * time.Minute))
	check(true)

	// Every timer disabled: no plan anywhere.
	off := map[string]string{}
	for key := range guardJobs {
		off[guardTimer(key)] = "inactive/disabled"
	}
	jobRunSystemd(t, off)
	p, err := b.Controls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range p.Jobs {
		if row.Planned != nil {
			t.Errorf("%s: planned with its timer disabled: %+v", row.Job, row.Planned)
		}
	}
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range st.Server.Units {
		if u.Planned != nil {
			t.Errorf("status %s: planned with its timer disabled", u.Name)
		}
	}
}

// TestJobRunNowCardPlanned: the Jobs home card plans the ATS and fetch (collect) actions when
// their timers are enabled, and no other action.
func TestJobRunNowCardPlanned(t *testing.T) {
	ctx := context.Background()
	_, b := jobRunWorld(t, nil)
	card, err := b.RunNowCard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range card.Actions {
		want := a.Action == console.RunATS || a.Action == console.RunFetch
		if (a.Planned != nil) != want {
			t.Errorf("%s: Planned %+v", a.Action, a.Planned)
		}
		if a.Planned != nil && (a.Planned.Overdue || !strings.Contains(a.Planned.Line, "its usual time")) {
			t.Errorf("%s cold start: %+v", a.Action, a.Planned)
		}
	}
	stubSystemctl(t, "", errors.New("no systemd here"))
	if card, err = b.RunNowCard(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range card.Actions {
		if a.Planned != nil {
			t.Errorf("%s planned without systemd", a.Action)
		}
	}
}
