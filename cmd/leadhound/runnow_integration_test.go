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
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// runNowBackend is the console backend over a reset test database, with its own data dir.
func runNowBackend(t *testing.T) *backend {
	t.Helper()
	storetest.ResetDB(t)
	b := testBackend(t)
	b.cfg.DataDir = t.TempDir()
	return b
}

// TestRunsSince: only runs started at or after since, oldest first.
func TestRunsSince(t *testing.T) {
	b := runNowBackend(t)
	ctx := context.Background()
	base := time.Now().Add(-10 * time.Hour).Truncate(time.Second)
	for i, off := range []time.Duration{3 * time.Hour, 0, time.Hour, 2 * time.Hour} {
		ownerExecCmd(t, `INSERT INTO run (command, source, started_at, note) VALUES ('collect', 'linkedin', $1, $2)`,
			base.Add(off), string(rune('a'+i)))
	}
	runs, err := b.s.Q().RunsSince(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, r := range runs {
		notes = append(notes, r.Note)
	}
	if strings.Join(notes, "") != "cda" {
		t.Fatalf("runs since = %v, want c d a (oldest first, from since on)", notes)
	}
	if runs, err := b.s.Q().RunsSince(ctx, time.Now().Add(time.Hour)); err != nil || len(runs) != 0 {
		t.Fatalf("future since = %v, %v", runs, err)
	}
}

// TestRunsSinceKeepsTheNewest: past the cap the oldest go, never the run that runs now.
func TestRunsSinceKeepsTheNewest(t *testing.T) {
	b := runNowBackend(t)
	ownerExecCmd(t, `INSERT INTO run (command, started_at) SELECT 'collect', now() - interval '1 hour' - g * interval '1 second' FROM generate_series(1, $1) g`, store.MaxRunsSince+5)
	ownerExecCmd(t, `INSERT INTO run (command, source, note) VALUES ('preview', '', 'newest')`)
	runs, err := b.s.Q().RunsSince(context.Background(), time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != store.MaxRunsSince || runs[len(runs)-1].Note != "newest" {
		t.Fatalf("got %d runs, last %+v", len(runs), runs[len(runs)-1])
	}
}

// TestRunNowBriefRefusedOnceSent: the send button is refused once today's brief_log row has
// sent_at, and the card says when it went; nothing is written then.
func TestRunNowBriefRefusedOnceSent(t *testing.T) {
	b := runNowBackend(t)
	ctx := context.Background()
	if _, err := b.RunNowCard(ctx); err != nil {
		t.Fatal(err)
	}
	ownerExecCmd(t, `INSERT INTO brief_log (day, subject, body_text, job_ids, send_state) VALUES ($1::date, 's', 'b', '{}', 'pending')`, today())
	if _, err := b.RunNow(ctx, console.RunRequest{Action: console.RunBrief}); err != nil {
		t.Fatalf("unsent brief: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.DataDir, "run", "brief")); err != nil {
		t.Fatalf("request file: %v", err)
	}
	ownerExecCmd(t, `UPDATE brief_log SET sent_at = now()`)
	if err := os.Remove(filepath.Join(b.cfg.DataDir, "run", "brief")); err != nil {
		t.Fatal(err)
	}
	_, err := b.RunNow(ctx, console.RunRequest{Action: console.RunBrief})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Today's email already went at") {
		t.Fatalf("sent brief: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.DataDir, "run", "brief")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused press wrote its request: %v", err)
	}
	card, err := b.RunNowCard(ctx)
	if err != nil || card.BriefSentAt == nil {
		t.Fatalf("card = %+v, %v", card, err)
	}
}

// TestRunNowFetchCooldown: a fetch is refused within 60 minutes of a finished LinkedIn
// collect and allowed while one runs; a second press is refused as already asked.
func TestRunNowFetchCooldown(t *testing.T) {
	b := runNowBackend(t)
	ctx := context.Background()
	ownerExecCmd(t, `INSERT INTO run (command, source, started_at, finished_at) VALUES ('collect', 'linkedin', now() - interval '40 minutes', now() - interval '30 minutes')`)
	_, err := b.RunNow(ctx, console.RunRequest{Action: console.RunFetch})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "LinkedIn was read at") {
		t.Fatalf("fetch after a collect: err = %v", err)
	}
	ownerExecCmd(t, `INSERT INTO run (command, source, started_at) VALUES ('collect', 'linkedin', now() - interval '2 minutes')`)
	out, err := b.RunNow(ctx, console.RunRequest{Action: console.RunFetch})
	if err != nil || !strings.Contains(out.Message, "Joining the collect") {
		t.Fatalf("fetch while a collect runs: %+v, %v", out, err)
	}
	_, err = b.RunNow(ctx, console.RunRequest{Action: console.RunFetch})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Already asked at") {
		t.Fatalf("second press: err = %v", err)
	}
	card, err := b.RunNowCard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range card.Actions {
		if a.Action == console.RunFetch && (a.State != "waiting" || !strings.Contains(a.Line, "waiting for LinkedIn")) {
			t.Fatalf("fetch on the card = %+v", a)
		}
	}
}

// TestRunNowFetchPausedToday: a throttled LinkedIn run today refuses the fetch; the
// preview still goes ahead and says the recheck is skipped.
func TestRunNowFetchPausedToday(t *testing.T) {
	b := runNowBackend(t)
	ctx := context.Background()
	ownerExecCmd(t, `INSERT INTO run (command, source, started_at, finished_at, stop_reason) VALUES ('recheck', 'linkedin', now() - interval '3 hours', now() - interval '3 hours', 'throttled')`)
	_, err := b.RunNow(ctx, console.RunRequest{Action: console.RunFetch})
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "LinkedIn is paused today (throttled at") {
		t.Fatalf("fetch paused: err = %v", err)
	}
	out, err := b.RunNow(ctx, console.RunRequest{Action: console.RunPreview})
	if err != nil || !strings.Contains(out.Message, "nothing is rechecked") {
		t.Fatalf("preview paused: %+v, %v", out, err)
	}
}
