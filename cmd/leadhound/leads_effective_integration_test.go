//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// captureLog sends the default logger to a buffer for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return &buf
}

// settingsNotes are the "leads settings ignored" lines logged, each without its time.
func settingsNotes(t *testing.T, logs *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for line := range strings.Lines(logs.String()) {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if msg, _ := rec["msg"].(string); !strings.HasPrefix(msg, "leads settings ignored") {
			continue
		}
		delete(rec, "time")
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// saveSettings saves doc as the operator's latest leads settings and returns its id.
func saveSettings(t *testing.T, doc string) int64 {
	t.Helper()
	id, err := leadstoretest.Q(t).SaveSettings(context.Background(), leadstore.DefaultAccountID, []byte(doc), leadstore.ActorOperator)
	if err != nil {
		t.Fatalf("save %s: %v", doc, err)
	}
	return id
}

// throughDoors reads the leads setup through both doors: leadsOpen (the CLI) and
// backend.leadsSetup (the console).
func throughDoors(t *testing.T, b *backend, load leadsLoad) map[string]leadsSetup {
	t.Helper()
	ctx := context.Background()
	s, cli, err := leadsOpen(ctx, load)
	if err != nil {
		t.Fatalf("leadsOpen: %v", err)
	}
	s.Close()
	con, err := b.leadsSetup(ctx, load)
	if err != nil {
		t.Fatalf("backend.leadsSetup: %v", err)
	}
	return map[string]leadsSetup{"leadsOpen": cli, "backend.leadsSetup": con}
}

// TestStaleOverrideIgnored (Task 5.1, D10): saved settings leads.yaml no longer accepts (a
// retired key, a value out of range, a kind the file does not know) are ignored through
// both doors with a note that names no value, and the file's values apply; the commands keep
// running. Sound settings apply, a reset brings the file back, and no row is the file.
func TestStaleOverrideIgnored(t *testing.T) {
	ctx := context.Background()
	leadsDB(t)
	b := leadsBackend(t)
	file, err := leadsEnv(mustConfig(t), leadsForOutreach)
	if err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)
	isFile := func(what string, got leadsSetup) {
		t.Helper()
		if got.leads.SkipRankOver != file.leads.SkipRankOver || !slices.Equal(got.leads.KindsOff, file.leads.KindsOff) ||
			got.leads.BatchesFrom() != file.leads.BatchesFrom() {
			t.Errorf("%s: skip_rank_over %d, kinds_off %v, batches_from %s; want the file's %d, %v, %s", what,
				got.leads.SkipRankOver, got.leads.KindsOff, got.leads.BatchesFrom(),
				file.leads.SkipRankOver, file.leads.KindsOff, file.leads.BatchesFrom())
		}
		if got.settings.Applied {
			t.Errorf("%s: settings marked applied", what)
		}
	}

	for door, got := range throughDoors(t, b, leadsForOutreach) {
		isFile("no row, "+door, got)
		if got.settings.ID != 0 || got.settings.Ignored != nil {
			t.Errorf("no row, %s: read %+v", door, got.settings)
		}
	}

	sound := saveSettings(t, `{"kinds_off":["d10"],"skip_rank_over":500}`)
	for door, got := range throughDoors(t, b, leadsForOutreach) {
		if !got.settings.Applied || got.settings.ID != sound || got.settings.At.IsZero() || got.settings.Ignored != nil ||
			got.leads.SkipRankOver != 500 || !slices.Equal(got.leads.KindsOff, []string{"d10"}) {
			t.Errorf("sound settings, %s: %+v, skip_rank_over %d, kinds_off %v", door, got.settings, got.leads.SkipRankOver, got.leads.KindsOff)
		}
	}

	for _, c := range []struct{ what, doc, word, value string }{
		{"a retired setting", `{"kinds_off":["d10"],"sending_pace":3}`, "sending_pace", "sending_pace"},
		{"a value out of range", `{"skip_rank_over":-7}`, "skip_rank_over", "-7"},
		{"a kind leads.yaml does not know", `{"kinds_off":["d5"]}`, "kinds_off", "d5"},
	} {
		logs.Reset()
		id := saveSettings(t, c.doc)
		for _, load := range []leadsLoad{leadsForProbe, leadsForOutreach} {
			for door, got := range throughDoors(t, b, load) {
				isFile(c.what+", "+door, got)
				if got.settings.ID != id || len(got.settings.Ignored) == 0 || !strings.Contains(got.settings.Ignored[0].Message, c.word) {
					t.Errorf("%s, %s: read %+v, want ignored for %s", c.what, door, got.settings, c.word)
				}
			}
		}
		// The commands keep running on the file's values.
		mustRun(t, "leads", "build")
		notes := settingsNotes(t, logs)
		if len(notes) == 0 {
			t.Errorf("%s: no note logged:\n%s", c.what, logs.String())
		}
		for _, n := range notes {
			if strings.Contains(n, c.value) {
				t.Errorf("%s: the note names a saved value (%s): %s", c.what, c.value, n)
			}
		}
	}

	reset, err := leadstoretest.Q(t).ResetSettings(ctx, leadstore.DefaultAccountID, leadstore.ActorOperator)
	if err != nil {
		t.Fatal(err)
	}
	for door, got := range throughDoors(t, b, leadsForOutreach) {
		isFile("after a reset, "+door, got)
		if got.settings.ID != reset || got.settings.Ignored != nil {
			t.Errorf("after a reset, %s: read %+v", door, got.settings)
		}
	}
}

// TestBatchUsesEffectiveKindsOff (Task 5.1, D6 and D11): a batch drafts from the effective
// settings, CLI and console alike. leads.yaml draws from starred leads only and nothing is
// starred; saved settings that draw from every lead but turn the leads' kind off draft
// nothing; the kind back on drafts all eight; skip_rank_over leaves the unranked sites out;
// a reset is the file again: nothing starred.
func TestBatchUsesEffectiveKindsOff(t *testing.T) {
	ctx := context.Background()
	orderScene(t)
	b := leadsBackend(t)
	var kind string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT DISTINCT kind FROM lead`).Scan(&kind); err != nil {
		t.Fatalf("one lead kind: %v", err)
	}
	batches := func() int { return batchCount(t, `SELECT count(*)::int FROM outreach_batch`) }

	saveSettings(t, `{"batches_from":"starred_then_rest","kinds_off":["`+kind+`"]}`)
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, "No batch") {
		t.Errorf("batch new, %s off:\n%s", kind, out)
	}
	func() {
		goLive(t)
		defer t.Setenv("LEADHOUND_OUTREACH", "")
		out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch})
		if err != nil || out.BatchID != 0 || !strings.Contains(out.Message, "No batch") {
			t.Errorf("console new batch, %s off: %+v, %v", kind, out, err)
		}
	}()
	if n := batches(); n != 0 {
		t.Fatalf("%d batches with the leads' kind turned off", n)
	}

	saveSettings(t, `{"batches_from":"starred_then_rest"}`)
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, "Batch 1 (shadow)") {
		t.Fatalf("batch new, %s on, drawn from every lead:\n%s", kind, out)
	}
	if n := batchCount(t, `SELECT count(DISTINCT lead_id)::int FROM outreach_message WHERE batch_id = 1`); n != len(orderSites) {
		t.Errorf("batch 1 holds %d leads, want %d", n, len(orderSites))
	}
	mustRun(t, "leads", "batch", "discard", "1")

	saveSettings(t, `{"batches_from":"starred_then_rest","skip_rank_over":1000}`)
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, "No batch") {
		t.Errorf("batch new, unranked sites skipped:\n%s", out)
	}

	if _, err := leadstoretest.Q(t).ResetSettings(ctx, leadstore.DefaultAccountID, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, noStarredWords) {
		t.Errorf("batch new after a reset:\n%s", out)
	}
	if n := batches(); n != 1 {
		t.Errorf("%d batches, want the one discarded", n)
	}
}
