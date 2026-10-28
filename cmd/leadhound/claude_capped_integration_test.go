//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// claudeNoon is the fixed clock the Capped tests count spend on: noon in Bucharest, far from
// midnight, so a test never straddles two days.
var claudeNoon = time.Date(2026, 10, 28, 12, 0, 0, 0, time.FixedZone("EET", 2*3600))

// anthropicFake is an httptest Anthropic API. reply answers each request; calls counts them;
// every request must carry the key in X-Api-Key and no Authorization header.
type anthropicFake struct {
	calls atomic.Int32
	srv   *httptest.Server
}

func newAnthropicFake(t *testing.T, key string, reply func(n int) (int, string)) *anthropicFake {
	t.Helper()
	f := &anthropicFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(f.calls.Add(1))
		if r.Header.Get("X-Api-Key") != key || r.Header.Get("Authorization") != "" || r.URL.Path != "/v1/messages" {
			t.Errorf("request %d: wrong key header, an Authorization header or path %s", n, r.URL.Path)
		}
		code, body := reply(n)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After-Ms", "1")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	saved := claudeBaseURL
	claudeBaseURL = f.srv.URL
	t.Cleanup(func() { claudeBaseURL = saved })
	return f
}

// anthropicMessage is a Messages API response.
func anthropicMessage(stop, text string, in, out int) string {
	content, _ := json.Marshal([]map[string]any{{"type": "text", "text": text, "citations": []any{}}})
	return fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5",
"content":%s,"stop_reason":%q,"stop_details":null,
"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,
"server_tool_use":{"web_search_requests":0,"web_fetch_requests":0}}}`, content, stop, in, out)
}

// claudeKeyFile writes key to a 0600 file outside the repo and points the key variable at it,
// with no systemd credentials directory and LEADHOUND_LIVE=1.
func claudeKeyFile(t *testing.T, key string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "anthropic.key")
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claude.KeyFileEnv, path)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("LEADHOUND_LIVE", "1")
	t.Setenv("LEADHOUND_TZ", "Europe/Bucharest")
}

// sentinelKey is a fresh key-shaped value no fixture or table could hold by chance.
func sentinelKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "sk-ant-sentinel-" + hex.EncodeToString(b)
}

// cappedFor builds the command's drafting Capped on the default account with the
// campaign caps given, on the fixed clock, over a store on the test database.
func cappedFor(t *testing.T, q store.Queries, own claudeCampaignCaps) *claude.Capped {
	t.Helper()
	c, err := claudeCapped("test", q, claude.PurposeDrafting, own)
	if err != nil || c == nil {
		t.Fatalf("claudeCapped: %v, %v", c, err)
	}
	c.Now = func() time.Time { return claudeNoon }
	return c
}

// testStore opens the app role's store on the test database.
func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// draftRequest is a small drafting request on lead.
func draftRequest(lead int64) claude.WriteRequest {
	return claude.WriteRequest{LeadID: lead, Model: claude.ModelSonnet, System: "Write short emails.", Prompt: "Fact 1: the site runs Drupal 7.",
		Schema:    map[string]any{"type": "object", "properties": map[string]any{"subject": map[string]any{"type": "string"}}},
		MaxTokens: 300}
}

// claudeDay is claudeNoon's calendar day in Bucharest, as Capped counts it.
func claudeDay(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	l := claudeNoon.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// dumpEveryTable is every row of every table in public as JSON text, one row per line.
func dumpEveryTable(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(tables) < 10 {
		t.Fatalf("only %d tables in public: %v", len(tables), tables)
	}
	var b strings.Builder
	for _, n := range tables {
		var dump string
		// The name comes from information_schema, quoted as an identifier.
		q := fmt.Sprintf(`SELECT coalesce(string_agg(row_to_json(t)::text, E'\n'), '') FROM public.%q t`, n) //nolint:gosec // G201: a table name from information_schema, quoted
		if err := conn.QueryRow(ctx, q).Scan(&dump); err != nil {
			t.Fatalf("dump %s: %v", n, err)
		}
		b.WriteString(n + ":\n" + dump + "\n")
	}
	return b.String()
}

// TestClaudeKeyNeverLeaks: with a sentinel key in a 0600 key file, the command's Capped path
// runs end to end against an httptest Anthropic server that echoes the key in its error
// bodies and the real test database: a successful write, a refusal, a 429 (retried, then
// unavailable), a record the store refuses and a store that cannot be read. Every slog line
// (the Capped value itself logged too), every error string, stdout and every row of every
// table in public are scanned: the sentinel appears nowhere, and the successful and refused
// calls are recorded.
func TestClaudeKeyNeverLeaks(t *testing.T) {
	orderScene(t)
	key := sentinelKey(t)
	claudeKeyFile(t, key)
	out := captureStdout(t)
	var logs bytes.Buffer
	savedLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(savedLog) })

	echo := `{"type":"error","error":{"type":"rate_limit_error","message":"key ` + key + ` is throttled"}}`
	srv := newAnthropicFake(t, key, func(n int) (int, string) {
		switch {
		case n == 1:
			return 200, anthropicMessage("end_turn", `{"subject":"Drupal 7"}`, 120, 40)
		case n == 2:
			return 200, anthropicMessage("refusal", "", 110, 5)
		case n <= 5: // the 429 and its two retries
			return 429, echo
		default: // the call whose record the store refuses
			return 200, anthropicMessage("end_turn", `{"subject":"x"}`, 100, 10)
		}
	})
	s := testStore(t)
	lead := leadIDOf(t, "cpandl.example")
	ctx := context.Background()
	var errs []string
	logged := func(what string, err error) {
		slog.Error("claude call", "what", what, "err", err)
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	c := cappedFor(t, s.Q(), claudeCampaignCaps{})
	slog.Info("capped", "capped", c, "client", c.Client)
	if _, _, err := c.Write(ctx, draftRequest(lead)); err != nil {
		t.Fatalf("write: %v", err)
	} else {
		logged("write", err)
	}
	_, _, err := c.Write(ctx, draftRequest(lead))
	logged("refusal", err)
	if !errors.Is(err, claude.ErrRefused) {
		t.Errorf("refusal: %v, want ErrRefused", err)
	}
	_, _, err = c.Write(ctx, draftRequest(lead))
	logged("429", err)
	if !errors.Is(err, ai.ErrUnavailable) {
		t.Errorf("429: %v, want ai.ErrUnavailable", err)
	}
	// The store refuses a row for account 0 after the call was billed.
	bad := cappedFor(t, s.Q(), claudeCampaignCaps{})
	bad.AccountID = 0
	_, _, err = bad.Write(ctx, draftRequest(lead))
	logged("record refused", err)
	if !errors.Is(err, ai.ErrUsageStore) {
		t.Errorf("record refused: %v, want ai.ErrUsageStore", err)
	}
	// A store that cannot be read stops before any call.
	closed, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	before := srv.calls.Load()
	_, _, err = cappedFor(t, closed.Q(), claudeCampaignCaps{}).Write(ctx, draftRequest(lead))
	logged("store down", err)
	if !errors.Is(err, ai.ErrUsageStore) || srv.calls.Load() != before {
		t.Errorf("store down: %v after %d new calls, want ai.ErrUsageStore and none", err, srv.calls.Load()-before)
	}
	if got := srv.calls.Load(); got != 6 {
		t.Errorf("server saw %d requests, want 6 (write, refusal, 429 ×3, refused record)", got)
	}

	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM ai_usage WHERE provider = 'anthropic' AND purpose = 'drafting'
		AND lead_id = %d AND account_id = 1 AND cost_micros > 0`, lead)); n != 2 {
		t.Errorf("%d drafting rows on the lead, want 2 (the write and the billed refusal)", n)
	}
	dump := dumpEveryTable(t)
	if !strings.Contains(dump, `"provider":"anthropic"`) {
		t.Fatal("the table dump holds no Claude row: the scan would prove nothing")
	}
	for name, text := range map[string]string{"slog": logs.String(), "errors": strings.Join(errs, "\n"), "stdout": out.String(), "tables": dump} {
		if strings.Contains(text, key) {
			t.Errorf("the sentinel key appears in %s", name)
		}
	}
	if len(errs) < 4 || logs.Len() == 0 {
		t.Errorf("captured %d errors and %d bytes of logs: the scan would prove little", len(errs), logs.Len())
	}
}

// TestClaudeCapGrid: the cap end to end against the real store. Under the caps a call is made
// and recorded on its lead; with today's purpose spend leaving less than the worst case it is
// refused without calling; with the lead's spend leaving less than the worst case it is refused
// while another lead still passes; the env ceiling binds a campaign that asks for more; and
// Jev's rows, however large, never count.
func TestClaudeCapGrid(t *testing.T) {
	orderScene(t)
	key := sentinelKey(t)
	claudeKeyFile(t, key)
	srv := newAnthropicFake(t, key, func(int) (int, string) {
		return 200, anthropicMessage("end_turn", `{"subject":"ok"}`, 100, 20)
	})
	s := testStore(t)
	q := s.Q()
	ctx := context.Background()
	day := claudeDay(t)
	leadA, leadB := leadIDOf(t, "cpandl.example"), leadIDOf(t, "margie.example")
	worst, err := claude.WorstWrite(draftRequest(leadA))
	if err != nil {
		t.Fatal(err)
	}
	// Jev rows on the same day: thousands of calls and a billion tokens, never counted.
	if err := q.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "jev-1.13.0", Purpose: ai.PurposeLeads, Day: day,
		Calls: 9000, InputTokens: 1_000_000_000}); err != nil {
		t.Fatal(err)
	}
	seed := func(lead, micros int64) {
		t.Helper()
		if err := q.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, LeadID: lead, Purpose: claude.PurposeDrafting,
			Model: claude.ModelSonnet, Day: day, CostMicros: micros}); err != nil {
			t.Fatal(err)
		}
	}
	call := func(name string, own claudeCampaignCaps, lead int64) error {
		t.Helper()
		before := srv.calls.Load()
		_, _, err := cappedFor(t, q, own).Write(ctx, draftRequest(lead))
		if called := srv.calls.Load() != before; called != (err == nil) {
			t.Errorf("%s: called %v with err %v", name, called, err)
		}
		return err
	}

	// Under the caps, the daily cap exactly the worst case: called and recorded on the lead.
	if err := call("under", claudeCampaignCaps{DailyMicros: worst}, leadA); err != nil {
		t.Fatalf("under: %v", err)
	}
	spentA, err := q.ClaudeSpendForLead(ctx, leadA, claude.PurposeDrafting)
	if err != nil || spentA <= 0 || spentA > worst {
		t.Fatalf("lead A spend = %d, %v; want above 0 and within the worst case %d", spentA, err, worst)
	}
	// Over the purpose cap: today's spend plus the worst case passes it by one micro.
	if err := call("purpose cap", claudeCampaignCaps{DailyMicros: spentA + worst - 1}, leadB); !errors.Is(err, ai.ErrSpendCap) {
		t.Errorf("purpose cap: %v, want ai.ErrSpendCap", err)
	}
	// Over the lead cap: lead A holds spentA + seeded spend, so its next worst case does not fit.
	seed(leadA, 50_000)
	if err := call("lead cap", claudeCampaignCaps{LeadMicros: spentA + 50_000 + worst - 1}, leadA); !errors.Is(err, ai.ErrSpendCap) {
		t.Errorf("lead cap: %v, want ai.ErrSpendCap", err)
	}
	if err := call("other lead", claudeCampaignCaps{LeadMicros: spentA + 50_000 + worst - 1}, leadB); err != nil {
		t.Errorf("other lead under its own cap: %v", err)
	}
	// The env ceiling binds a campaign asking for more than it.
	spent, err := q.ClaudeSpendOn(ctx, 1, "", day)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_CLAUDE_DAILY_USD_CAP", fmt.Sprintf("%d.%06d", (spent+worst-1)/1e6, (spent+worst-1)%1e6))
	if err := call("env ceiling", claudeCampaignCaps{DailyMicros: 1_000_000_000}, leadB); !errors.Is(err, ai.ErrSpendCap) {
		t.Errorf("env ceiling: %v, want ai.ErrSpendCap", err)
	}
	// Jev's rows are still there and still uncounted: Claude's spend is its own rows only.
	if n := dbCount(t, `SELECT count(*) FROM ai_usage WHERE provider = 'typesafe'`); n != 1 {
		t.Errorf("%d Jev rows, want 1", n)
	}
	if want := spentA + 50_000; spent < want {
		t.Errorf("Claude spend today %d, want at least %d", spent, want)
	}
}

// TestClaudeCappedColdStart is the Capped-level cold-start twin: the first-ever Claude call on
// an empty ai_usage and the same call on a day already holding Jev rows go through the same
// way, at a daily cap of exactly the worst case, and record the same cost; Jev's call count
// is unchanged by the Claude row in the second.
func TestClaudeCappedColdStart(t *testing.T) {
	run := func(name string, jev bool) int64 {
		t.Helper()
		orderScene(t)
		key := sentinelKey(t)
		claudeKeyFile(t, key)
		newAnthropicFake(t, key, func(int) (int, string) {
			return 200, anthropicMessage("end_turn", `{"subject":"ok"}`, 100, 20)
		})
		s := testStore(t)
		q := s.Q()
		ctx := context.Background()
		day := claudeDay(t)
		if n := dbCount(t, `SELECT count(*) FROM ai_usage`); n != 0 {
			t.Fatalf("%s: ai_usage holds %d rows before the start", name, n)
		}
		if jev {
			for _, p := range []string{ai.PurposeJobs, ai.PurposeLeads} {
				if err := q.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "jev-1.13.0", Purpose: p, Day: day,
					Calls: 300, InputTokens: 50_000_000}); err != nil {
					t.Fatal(err)
				}
			}
		}
		lead := leadIDOf(t, "cpandl.example")
		worst, err := claude.WorstWrite(draftRequest(lead))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := cappedFor(t, q, claudeCampaignCaps{DailyMicros: worst}).Write(ctx, draftRequest(lead)); err != nil {
			t.Fatalf("%s: first call: %v", name, err)
		}
		spent, err := q.ClaudeSpendOn(ctx, 1, claude.PurposeDrafting, day)
		if err != nil {
			t.Fatal(err)
		}
		calls, err := q.AICallsOn(ctx, "typesafe", ai.PurposeLeads, day)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{false: 0, true: 300}[jev]; calls != want {
			t.Errorf("%s: Jev leads calls = %d, want %d", name, calls, want)
		}
		return spent
	}
	empty := run("empty ai_usage", false)
	withJev := run("a day with Jev rows", true)
	if empty <= 0 || empty != withJev {
		t.Errorf("first call cost %d on an empty table and %d beside Jev rows; want the same, above 0", empty, withJev)
	}
}

// TestConsoleClaudeSpend: the dashboard, the Campaigns tab and the lead page read Claude's
// spend from ai_usage: nothing spent reads $0.00 of the ceiling; after drafting and research
// rows the dashboard names both in order, every campaign shows the account's spend, the
// lead page shows that lead's total and a lead with none shows nothing. Jev rows never count.
func TestConsoleClaudeSpend(t *testing.T) {
	orderScene(t)
	t.Setenv("LEADHOUND_CLAUDE_DAILY_USD_CAP", "5.00")
	ctx := context.Background()
	b := leadsBackend(t)
	d, err := b.Dashboard(ctx)
	if err != nil || d.ClaudeMicros != 0 || d.ClaudeCapMicros != 5_000_000 || len(d.ClaudeByPurpose) != 0 {
		t.Fatalf("quiet day: %v, %d of %d, %v", err, d.ClaudeMicros, d.ClaudeCapMicros, d.ClaudeByPurpose)
	}
	local := b.now().In(b.loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, b.loc)
	q := b.s.Q()
	leadA, leadB := leadIDOf(t, "cpandl.example"), leadIDOf(t, "margie.example")
	for _, r := range []claude.UsageRow{
		{AccountID: 1, LeadID: leadA, Purpose: claude.PurposeResearch, Model: claude.ModelHaiku, Day: day, CostMicros: 230_000},
		{AccountID: 1, LeadID: leadA, Purpose: claude.PurposeDrafting, Model: claude.ModelSonnet, Day: day, CostMicros: 600_000},
		{AccountID: 1, Purpose: claude.PurposeDrafting, Model: claude.ModelSonnet, Day: day, CostMicros: 10_000},
		// Yesterday's spend counts on the lead, never today.
		{AccountID: 1, LeadID: leadA, Purpose: claude.PurposeDrafting, Model: claude.ModelSonnet, Day: day.AddDate(0, 0, -1), CostMicros: 5_000},
	} {
		if err := q.RecordClaudeUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "jev-1.13.0", Purpose: ai.PurposeLeads, Day: day,
		Calls: 10, InputTokens: 1_000_000}); err != nil {
		t.Fatal(err)
	}
	if d, err = b.Dashboard(ctx); err != nil {
		t.Fatal(err)
	}
	if d.ClaudeMicros != 840_000 || fmt.Sprint(d.ClaudeByPurpose) != "[{drafting 610000} {research 230000}]" || d.LeadsCalls != 10 {
		t.Errorf("dashboard: %d %v, Jev leads calls %d", d.ClaudeMicros, d.ClaudeByPurpose, d.LeadsCalls)
	}
	views, err := b.Campaigns(ctx)
	if err != nil || len(views) == 0 {
		t.Fatalf("campaigns: %v %v", views, err)
	}
	for _, v := range views {
		if v.ClaudeSpendToday != "$0.84 of $5.00" {
			t.Errorf("campaign %s: spend %q, want the account's $0.84 of $5.00", v.Slug, v.ClaudeSpendToday)
		}
	}
	pa, err := b.LeadDetail(ctx, leadA)
	if err != nil || pa.ClaudeSpend != "$0.83" {
		t.Errorf("lead A: %q, %v; want $0.83 (every purpose, every day)", pa.ClaudeSpend, err)
	}
	pb, err := b.LeadDetail(ctx, leadB)
	if err != nil || pb.ClaudeSpend != "" {
		t.Errorf("lead B: %q, %v; want nothing", pb.ClaudeSpend, err)
	}
}
