//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/preview"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// previewSetup is a scored fixture database and a data dir of its own holding the demo
// profile, with live off.
func previewSetup(t *testing.T) string {
	t.Helper()
	scoredFixtures(t)
	data := demoDataDir(t)
	t.Setenv("LEADHOUND_DATA_DIR", data)
	t.Setenv("LEADHOUND_LIVE", "")
	captureNotes(t)
	return data
}

// previewIDs is the file's job ids in order: Apply, then Your call, then the runner-up.
func previewIDs(p preview.Preview) []int64 {
	var ids []int64
	for _, j := range slices.Concat(p.Apply, p.YourCall) {
		ids = append(ids, j.ID)
	}
	if p.RunnerUp != nil {
		ids = append(ids, p.RunnerUp.ID)
	}
	return ids
}

func previewRuns(t *testing.T) []store.Run {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT id, command, source, started_at, finished_at, requests, failures, stop_reason, note FROM run WHERE command = 'preview' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []store.Run
	for rows.Next() {
		var r store.Run
		if err := rows.Scan(&r.ID, &r.Command, &r.Source, &r.StartedAt, &r.FinishedAt, &r.Requests, &r.Failures, &r.StopReason, &r.Note); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestPreviewWritesNothingTheBriefOwns: a preview writes preview.json and one finished
// preview run row, and nothing the brief owns: no brief_log row, no shortlisting, nothing
// in the outbox.
func TestPreviewWritesNothingTheBriefOwns(t *testing.T) {
	data := previewSetup(t)
	shortlisted := dbCount(t, `SELECT count(*) FROM status_event WHERE status = 'shortlisted'`)
	statusEvents := dbCount(t, `SELECT count(*) FROM status_event`)
	outboxBefore := outboxFiles(t, defaultOutbox)
	code, out, stderr := runCmdErr(t, "preview", "--allow-fake")
	if code != 0 {
		t.Fatalf("preview: exit %d, stderr %q", code, stderr)
	}
	if !strings.Contains(out, "preview: ") || !strings.Contains(out, filepath.Join(data, preview.FileName)) {
		t.Errorf("stdout = %q", out)
	}
	if got := dbCount(t, `SELECT count(*) FROM brief_log`); got != 0 {
		t.Errorf("brief_log rows = %d, want 0", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM status_event WHERE status = 'shortlisted'`); got != shortlisted {
		t.Errorf("shortlisted events %d → %d", shortlisted, got)
	}
	if got := dbCount(t, `SELECT count(*) FROM status_event`); got != statusEvents {
		t.Errorf("status events %d → %d", statusEvents, got)
	}
	if got := outboxFiles(t, defaultOutbox); got != outboxBefore {
		t.Errorf("outbox files %d → %d", outboxBefore, got)
	}
	if got := outboxFiles(t, filepath.Join(data, "outbox")); got != 0 {
		t.Errorf("data outbox files = %d", got)
	}
	runs := previewRuns(t)
	if len(runs) != 1 || runs[0].FinishedAt == nil || runs[0].Source != "" || !strings.Contains(runs[0].Note, "listed") {
		t.Fatalf("preview runs = %+v, want one finished, source \"\"", runs)
	}
	p, err := preview.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Apply)+len(p.YourCall) == 0 {
		t.Fatalf("preview lists nothing: %+v", p)
	}
}

// TestPreviewMatchesComposeNow: the file lists the same jobs, in the same order, as the
// brief's own compose step on the same database.
func TestPreviewMatchesComposeNow(t *testing.T) {
	data := previewSetup(t)
	ctx := context.Background()
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL", "LEADHOUND_DATA_DIR")
	if err != nil {
		t.Fatal(err)
	}
	prof, err := loadProfileDir(data)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(config.DefaultTZ)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in, err := composeNow(ctx, s, cfg, prof, loc, time.Now().In(loc), true, true)
	if err != nil {
		t.Fatal(err)
	}
	want := in.JobIDs()
	if in.RunnerUp != nil {
		want = append(want, in.RunnerUp.ID)
	}
	if code, _, stderr := runCmdErr(t, "preview", "--allow-fake"); code != 0 {
		t.Fatalf("preview: exit %d, stderr %q", code, stderr)
	}
	p, err := preview.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := previewIDs(p); len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("preview ids = %v, compose ids = %v", got, want)
	}
}

// TestPreviewAfterFetchSource: --after-fetch marks the run row's source fetch.
func TestPreviewAfterFetchSource(t *testing.T) {
	previewSetup(t)
	if code, _, stderr := runCmdErr(t, "preview", "--allow-fake"); code != 0 {
		t.Fatalf("preview: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCmdErr(t, "preview", "--allow-fake", "--after-fetch"); code != 0 {
		t.Fatalf("preview --after-fetch: exit %d, stderr %q", code, stderr)
	}
	runs := previewRuns(t)
	if len(runs) != 2 || runs[0].Source != "" || runs[1].Source != "fetch" {
		t.Fatalf("preview runs = %+v, want sources \"\" then fetch", runs)
	}
}

// TestPreviewColdStartAndReplace: no preview.json yet → created, live off says the
// recheck was off; a second run replaces it atomically with a later MadeAt and leaves no
// temporary file.
func TestPreviewColdStartAndReplace(t *testing.T) {
	data := previewSetup(t)
	if _, err := preview.Read(data); err != preview.ErrNone {
		t.Fatalf("before: %v, want ErrNone", err)
	}
	if code, _, stderr := runCmdErr(t, "preview", "--allow-fake"); code != 0 {
		t.Fatalf("preview: exit %d, stderr %q", code, stderr)
	}
	first, err := preview.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if !first.RecheckOff {
		t.Error("live off: RecheckOff = false")
	}
	if st, err := os.Stat(filepath.Join(data, preview.FileName)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("preview.json = %v, %v; want 0600", st, err)
	}
	time.Sleep(10 * time.Millisecond)
	if code, _, stderr := runCmdErr(t, "preview", "--allow-fake"); code != 0 {
		t.Fatalf("second preview: exit %d, stderr %q", code, stderr)
	}
	second, err := preview.Read(data)
	if err != nil {
		t.Fatal(err)
	}
	if !second.MadeAt.After(first.MadeAt) {
		t.Errorf("MadeAt %v → %v, want later", first.MadeAt, second.MadeAt)
	}
	es, err := os.ReadDir(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".preview-") {
			t.Errorf("temporary file left: %s", e.Name())
		}
	}
}

// TestPreviewUsage: a stray argument is a usage error (exit 2).
func TestPreviewUsage(t *testing.T) {
	previewSetup(t)
	if code, _, stderr := runCmdErr(t, "preview", "extra"); code != 2 || !strings.Contains(stderr, "leadhound preview") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}
