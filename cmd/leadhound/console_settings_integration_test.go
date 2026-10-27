//go:build integration

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// withoutKindPoints rewrites dir's leads.yaml without the priority block's kind points, so
// kinds_priority gives each kind its points (leadconfig.KindPoints) and its order reaches the
// queue.
func withoutKindPoints(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, leadconfig.FileName)
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(raw)) {
		if !strings.HasPrefix(line, "  kinds: {") {
			out = append(out, line)
		}
	}
	if len(out) == strings.Count(string(raw), "\n") {
		t.Fatal("leads.yaml priority.kinds not found")
	}
	if err := os.WriteFile(path, []byte(strings.Join(out, "")), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

// settingsLine is the line `leads settings show` prints for name.
func settingsLine(t *testing.T, out, name string) string {
	t.Helper()
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("settings show has no %s line:\n%s", name, out)
	return ""
}

// kindsFirst is v's kinds with first moved to the top.
func kindsFirst(v console.LeadSettingsValues, first string) []console.KindToggle {
	i := slices.IndexFunc(v.Kinds, func(k console.KindToggle) bool { return k.Name == first })
	out := []console.KindToggle{v.Kinds[i]}
	for j, k := range v.Kinds {
		if j != i {
			out = append(out, k)
		}
	}
	return out
}

// TestSettingsRoundTrip (Task 5.3, D10, D11): the kinds' order changed through the console
// backend is what `leads settings show` reports, from the console, and what the queue scores
// by; a section saved after it keeps it (the save merges); a kind turned off in the console
// leaves batch new with nothing, and back on drafts the eight; the kinds' reset is the file
// again for that section only, and `leads settings reset` puts everything back.
func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := orderScene(t)
	withoutKindPoints(t, dir)
	b := leadsBackend(t)

	page, err := b.LeadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range page.Values.Kinds {
		names = append(names, k.Name)
	}
	if !slices.Equal(names, []string{"agency_partner", "unsupported", "d10"}) || page.Sources.KindsOrder.Console {
		t.Fatalf("kinds %v from %+v", names, page.Sources.KindsOrder)
	}
	if got := settingsLine(t, mustRun(t, "leads", "settings", "show"), "kinds_priority"); got != "kinds_priority: agency_partner, unsupported, d10 · file" {
		t.Errorf("show before: %q", got)
	}
	before := map[int64]int{}
	for _, c := range consoleQueueOf(t, b) {
		before[c.LeadID] = c.Score
	}
	if len(before) != len(orderSites) {
		t.Fatalf("queue holds %d leads, want %d", len(before), len(orderSites))
	}

	v := page.Values
	v.Kinds = kindsFirst(v, "unsupported")
	if err := b.SaveLeadSettings(ctx, console.SettingsKinds, v, false); err != nil {
		t.Fatal(err)
	}
	if page, err = b.LeadSettings(ctx); err != nil || !page.Sources.KindsOrder.Console || page.Values.Kinds[0].Name != "unsupported" {
		t.Fatalf("after save: %+v %v %v", page.Sources.KindsOrder, page.Values.Kinds, err)
	}
	show := mustRun(t, "leads", "settings", "show")
	if got := settingsLine(t, show, "kinds_priority"); !strings.HasPrefix(got, "kinds_priority: unsupported, agency_partner, d10 · console 20") {
		t.Errorf("show after the save: %q", got)
	}
	if got := settingsLine(t, show, "batches_from"); got != "batches_from: starred · file" {
		t.Errorf("show batches_from: %q", got)
	}
	// unsupported was 2nd of 3 (20 points), now 1st (30): every lead scores 10 more.
	for _, c := range consoleQueueOf(t, b) {
		if c.Score != before[c.LeadID]+10 {
			t.Errorf("lead %d scores %d, want %d", c.LeadID, c.Score, before[c.LeadID]+10)
		}
	}

	// Batches from every lead, then unsupported off: batch new drafts nothing.
	v = page.Values
	v.BatchesFrom = console.BatchesStarredThenRest
	if err := b.SaveLeadSettings(ctx, console.SettingsBatches, v, false); err != nil {
		t.Fatal(err)
	}
	for i := range v.Kinds {
		v.Kinds[i].On = v.Kinds[i].Name != "unsupported"
	}
	if err := b.SaveLeadSettings(ctx, console.SettingsKinds, v, false); err != nil {
		t.Fatal(err)
	}
	show = mustRun(t, "leads", "settings", "show")
	for _, name := range []string{"kinds_priority", "kinds_off", "batches_from"} {
		if !strings.Contains(settingsLine(t, show, name), "· console 20") {
			t.Errorf("show %s: %q", name, settingsLine(t, show, name))
		}
	}
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, "No batch") {
		t.Errorf("batch new with unsupported off:\n%s", out)
	}
	for i := range v.Kinds {
		v.Kinds[i].On = true
	}
	if err := b.SaveLeadSettings(ctx, console.SettingsKinds, v, false); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, "Batch 1 (shadow)") {
		t.Fatalf("batch new with unsupported on:\n%s", out)
	}
	if n := batchCount(t, `SELECT count(DISTINCT lead_id)::int FROM outreach_message WHERE batch_id = 1`); n != len(orderSites) {
		t.Errorf("batch 1 holds %d leads, want %d", n, len(orderSites))
	}
	mustRun(t, "leads", "batch", "discard", "1")

	// The kinds' reset: the file again for kinds, the batches kept.
	if err := b.SaveLeadSettings(ctx, console.SettingsKinds, console.LeadSettingsValues{}, true); err != nil {
		t.Fatal(err)
	}
	show = mustRun(t, "leads", "settings", "show")
	if got := settingsLine(t, show, "kinds_priority"); got != "kinds_priority: agency_partner, unsupported, d10 · file" {
		t.Errorf("show after the kinds' reset: %q", got)
	}
	if got := settingsLine(t, show, "batches_from"); !strings.HasPrefix(got, "batches_from: starred_then_rest · console 20") {
		t.Errorf("batches_from after the kinds' reset: %q", got)
	}
	for _, c := range consoleQueueOf(t, b) {
		if c.Score != before[c.LeadID] {
			t.Errorf("after the reset lead %d scores %d, want %d", c.LeadID, c.Score, before[c.LeadID])
		}
	}

	if out := mustRun(t, "leads", "settings", "reset"); !strings.Contains(out, "every setting is back to leads.yaml") {
		t.Errorf("settings reset:\n%s", out)
	}
	for line := range strings.Lines(mustRun(t, "leads", "settings", "show")) {
		if strings.Contains(line, " · ") && !strings.HasSuffix(strings.TrimSpace(line), "· file") {
			t.Errorf("after reset: %q", line)
		}
	}
	if out := mustRun(t, "leads", "settings", "reset"); !strings.Contains(out, "nothing to reset") {
		t.Errorf("a second reset:\n%s", out)
	}
	if out := mustRun(t, "leads", "batch", "new"); !strings.Contains(out, noStarredWords) {
		t.Errorf("batch new after reset (the file draws from starred leads):\n%s", out)
	}
	if code, _, _ := leadsRun(t, "leads", "settings", "reset", "--section", "nope"); code == 0 {
		t.Error("reset of no such section exited 0")
	}
}

// secretSentinels are values the environment holds that no saved setting and no Settings
// page may: every variable set outside console.EnvAllowlist, with the secret ones set to
// sentinels here, and the sender's operator address from the example leads.yaml.
func secretSentinels(t *testing.T) []string {
	t.Helper()
	for _, name := range []string{"LEADHOUND_CONSOLE_PASSWORD_HASH", "LEADHOUND_JEV_API_KEY", "TYPESAFE_API_KEY", "RESEND_API_KEY",
		"LEADHOUND_MAILBOX_PASSWORD"} {
		t.Setenv(name, "sentinel-"+strings.ToLower(name))
	}
	out := []string{"operator@sender.example"}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if (strings.HasPrefix(name, "LEADHOUND_") || strings.HasSuffix(name, "_API_KEY")) &&
			!slices.Contains(console.EnvAllowlist, name) && len(value) >= 8 {
			out = append(out, value)
		}
	}
	return out
}

// TestSettingsSecretSweep (Task 5.3, D11): after every section is saved through the console
// backend, lead_setting and priority_profile hold no operator address and no environment
// value outside the allowlist, and neither does the Settings page; the env panel lists only
// allowlisted names.
func TestSettingsSecretSweep(t *testing.T) {
	ctx := context.Background()
	orderScene(t)
	sentinels := secretSentinels(t)
	b := leadsBackend(t)
	page, err := b.LeadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v := page.Values
	for i := range v.Countries {
		v.Countries[i].Wanted = v.Countries[i].Code != "GB"
	}
	v.NeverContact = "fr, it"
	v.Kinds = kindsFirst(v, "d10")
	v.SkipRankOver = "250000"
	v.BatchesFrom = console.BatchesStarredThenRest
	for i := range v.Dates {
		if v.Dates[i].Name == "d10_eol" {
			v.Dates[i].Value = "2026-12-31"
		}
	}
	v.Rating, v.LeadsCap = false, "40"
	v.Order.Countries, v.Order.RankBands = "GB 30", "5000 20"
	sections := []string{console.SettingsWho, console.SettingsKinds, console.SettingsSmallSites, console.SettingsBatches,
		console.SettingsDates, console.SettingsRelevance, console.SettingsOrder}
	for _, s := range sections {
		if err := b.SaveLeadSettings(ctx, s, v, false); err != nil {
			t.Fatalf("save %s: %v", s, err)
		}
	}
	page, err = b.LeadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	src := page.Sources
	for name, s := range map[string]console.SettingSource{"who": src.Who, "kinds on": src.KindsOn, "kinds order": src.KindsOrder,
		"small sites": src.SkipRankOver, "batches": src.BatchesFrom, "rating": src.Rating, "cap": src.LeadsCap, "order": src.Order} {
		if !s.Console {
			t.Errorf("%s is not from the console after its save", name)
		}
	}
	if !strings.Contains(settingsLine(t, mustRun(t, "leads", "settings", "show"), "priority.exclude_countries (never contact)"), "GB, FR, IT · console") {
		t.Error("show does not say the never-contact countries came from the console")
	}

	var docs []string
	rows, err := storetest.OwnerConn(t).Query(ctx,
		`SELECT coalesce(settings::text, '') FROM lead_setting UNION ALL SELECT coalesce(profile::text, '') FROM priority_profile`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(docs) < len(sections) {
		t.Fatalf("%d saved rows, want at least %d", len(docs), len(sections))
	}

	h, c := jobsHandler(t, b)
	r := httptest.NewRequest(http.MethodGet, "/leads/settings", nil)
	r.Header.Set("Cookie", c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("settings page: %d", w.Code)
	}
	for _, s := range sentinels {
		for _, d := range docs {
			if strings.Contains(d, s) {
				t.Errorf("a saved settings row holds a secret value (%d chars)", len(s))
			}
		}
		if strings.Contains(w.Body.String(), s) {
			t.Errorf("the settings page shows a secret value (%d chars)", len(s))
		}
	}
	for _, e := range page.ReadOnly.Env {
		if !slices.Contains(console.EnvAllowlist, e.Name) {
			t.Errorf("env panel lists %s", e.Name)
		}
	}
	if len(page.ReadOnly.Templates) == 0 {
		t.Error("no template names on the page")
	}
	for _, name := range page.ReadOnly.Templates {
		if strings.Contains(name, "{{") || len(name) > 120 {
			t.Errorf("a template line looks like a body: %q", name)
		}
	}

	// Who's reset keeps the fallback order's points; the order's reset then leaves nothing.
	if err := b.SaveLeadSettings(ctx, console.SettingsWho, console.LeadSettingsValues{}, true); err != nil {
		t.Fatal(err)
	}
	if page, _ = b.LeadSettings(ctx); page.Sources.Who.Console || !page.Sources.Order.Console || page.Values.Order.Countries != "GB 30" {
		t.Errorf("after who's reset: who %+v, order %+v %q", page.Sources.Who, page.Sources.Order, page.Values.Order.Countries)
	}
	if err := b.SaveLeadSettings(ctx, console.SettingsOrder, console.LeadSettingsValues{}, true); err != nil {
		t.Fatal(err)
	}
	if _, saved, err := leadstoretest.Q(t).EffectiveProfile(ctx, leadconfig.Profile{}); err != nil || saved {
		t.Errorf("after both resets a profile is saved (%v)", err)
	}
}

// TestSettingsCapRefused (D9): a leads cap above LEADHOUND_JEV_LEADS_DAILY_CALL_CAP is refused
// in plain words and saves nothing; the server's cap itself is allowed, and so is a bad save
// of another section refused with leads.yaml's own words.
func TestSettingsCapRefused(t *testing.T) {
	ctx := context.Background()
	leadsDB(t)
	t.Setenv("LEADHOUND_JEV_LEADS_DAILY_CALL_CAP", "500")
	b := leadsBackend(t)
	page, err := b.LeadSettings(ctx)
	if err != nil || page.ServerCap != 500 {
		t.Fatalf("server cap %d (%v)", page.ServerCap, err)
	}
	rows := func() int { return batchCount(t, `SELECT count(*)::int FROM lead_setting`) }
	v := page.Values
	v.LeadsCap = "501"
	err = b.SaveLeadSettings(ctx, console.SettingsRelevance, v, false)
	var refusal *console.SettingsRefused
	if !errors.As(err, &refusal) || !errors.Is(err, console.ErrUserFixable) ||
		!slices.Equal(refusal.Problems, []string{"The server allows at most 500 a day; raise it in .env first."}) {
		t.Errorf("cap above the server's: %v", err)
	}
	v.LeadsCap = "lots"
	if err := b.SaveLeadSettings(ctx, console.SettingsRelevance, v, false); !errors.As(err, &refusal) {
		t.Errorf("a cap that is no number: %v", err)
	}
	v.SkipRankOver = "-4"
	if err := b.SaveLeadSettings(ctx, console.SettingsSmallSites, v, false); !errors.As(err, &refusal) ||
		!strings.Contains(strings.Join(refusal.Problems, " "), "skip_rank_over must be 0 (off) or more") {
		t.Errorf("a negative skip_rank_over: %v", err)
	}
	if n := rows(); n != 0 {
		t.Fatalf("%d settings rows after refusals", n)
	}
	v.LeadsCap = "500"
	if err := b.SaveLeadSettings(ctx, console.SettingsRelevance, v, false); err != nil {
		t.Fatal(err)
	}
	if page, _ = b.LeadSettings(ctx); page.Values.LeadsCap != "500" || !page.Sources.LeadsCap.Console {
		t.Errorf("after save: %q from %+v", page.Values.LeadsCap, page.Sources.LeadsCap)
	}
	if err := b.SaveLeadSettings(ctx, console.SettingsAll, v, false); !errors.As(err, &refusal) {
		t.Errorf("a save of everything: %v", err)
	}
}

// TestSettingsStaleOverrideNote (D10): a saved change leads.yaml no longer accepts shows as a
// note on the page and in `settings show`, every value says file, and the next save replaces
// it rather than carrying it.
func TestSettingsStaleOverrideNote(t *testing.T) {
	ctx := context.Background()
	leadsDB(t)
	b := leadsBackend(t)
	saveSettings(t, `{"kinds_off":["d5"],"skip_rank_over":500}`)
	page, err := b.LeadSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Notes) == 0 || !strings.Contains(page.Notes[0], "no longer passes the checks leads.yaml passes") ||
		!strings.Contains(page.Notes[0], "kinds_off") {
		t.Errorf("notes %q", page.Notes)
	}
	if page.Sources.KindsOn.Console || page.Sources.SkipRankOver.Console || page.Values.SkipRankOver != "0" {
		t.Errorf("a stale change shows as in force: %+v, skip %q", page.Sources, page.Values.SkipRankOver)
	}
	show := mustRun(t, "leads", "settings", "show")
	if !strings.Contains(show, "note: The change saved in the console on ") || settingsLine(t, show, "skip_rank_over") != "skip_rank_over: 0 (off) · file" {
		t.Errorf("show with a stale change:\n%s", show)
	}
	v := page.Values
	v.BatchesFrom = console.BatchesStarredThenRest
	if err := b.SaveLeadSettings(ctx, console.SettingsBatches, v, false); err != nil {
		t.Fatal(err)
	}
	raw, _, _, _, err := leadstoretest.Q(t).LatestSettings(ctx, leadstore.DefaultAccountID)
	if err != nil {
		t.Fatal(err)
	}
	o, err := leadconfig.DecodeOverrides(raw)
	if err != nil || o.KindsOff != nil || o.SkipRankOver != nil || o.BatchesFrom == nil {
		t.Errorf("the save after a stale change saved %s (%v)", raw, err)
	}
	page, _ = b.LeadSettings(ctx)
	for _, n := range page.Notes {
		if strings.Contains(n, "no longer passes") {
			t.Errorf("the stale note stays after the save: %q", n)
		}
	}
}
