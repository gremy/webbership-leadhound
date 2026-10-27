//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00009: ai_usage records Claude's tokens, cost, account, lead and batch flag.

const claudeCostVersion = 9 // 00009_ai_usage_cost.sql

// TestClaudeUsageColumns: after a migrate the new columns exist with their defaults, a Jev
// writer that names none of them still inserts, and the indexes are there.
func TestClaudeUsageColumns(t *testing.T) {
	ctx := context.Background()
	appStore(t)
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx,
		`INSERT INTO ai_usage (provider, model, day, calls, input_tokens) VALUES ('typesafe', 'm', '2026-10-23', 1, 5)`); err != nil {
		t.Fatalf("Jev-shaped insert: %v", err)
	}
	var out, cr, cw, ws int
	var cost, account int64
	var lead *int64
	var batch bool
	if err := app.QueryRow(ctx, `SELECT output_tokens, cache_read_tokens, cache_write_tokens, web_searches,
		cost_micros, account_id, lead_id, batch FROM ai_usage`).Scan(&out, &cr, &cw, &ws, &cost, &account, &lead, &batch); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if out != 0 || cr != 0 || cw != 0 || ws != 0 || cost != 0 || account != 1 || lead != nil || batch {
		t.Fatalf("defaults = %d %d %d %d %d %d %v %v; want zeros, account 1, no lead, not batch",
			out, cr, cw, ws, cost, account, lead, batch)
	}
	for _, idx := range []string{"ai_usage_claude_day", "ai_usage_claude_lead"} {
		var ok bool
		if err := app.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, idx).Scan(&ok); err != nil || !ok {
			t.Errorf("index %s missing (%v)", idx, err)
		}
	}
}

// TestClaudeUsagePurposeCheck: the three Claude purposes are accepted, anything else (the
// empty purpose included) is refused by the CHECK.
func TestClaudeUsagePurposeCheck(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, bucharest(t))
	for _, p := range []string{claude.PurposeDrafting, claude.PurposeResearch, claude.PurposeDraftsEval} {
		if err := q.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, Purpose: p, Model: "claude-sonnet-5-5", Day: day}); err != nil {
			t.Fatalf("purpose %s: %v", p, err)
		}
	}
	for _, p := range []string{"other", ""} {
		err := q.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, Purpose: p, Model: "claude-sonnet-5-5", Day: day})
		if sqlState(err) != "23514" {
			t.Errorf("purpose %q: err = %v, want 23514 check_violation", p, err)
		}
	}
	if err := q.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "m", Purpose: "drafting2", Day: day, Calls: 1}); sqlState(err) != "23514" {
		t.Errorf("Jev purpose drafting2: err = %v, want 23514", err)
	}
	// An account below 1 and a negative cost never reach the database.
	if err := q.RecordClaudeUsage(ctx, claude.UsageRow{Purpose: claude.PurposeDrafting, Model: "m", Day: day}); err == nil {
		t.Error("account 0: err = nil, want a refusal")
	}
	if err := q.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, Purpose: claude.PurposeDrafting, Model: "m", Day: day, CostMicros: -1}); err == nil {
		t.Error("negative cost: err = nil, want a refusal")
	}
}

// TestClaudeSpendSums: cost is summed per account, day (the local date) and purpose, and per
// lead across days; every field of a row is stored as given.
func TestClaudeSpendSums(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, loc) }

	rows := []claude.UsageRow{
		{AccountID: 1, LeadID: 7, Purpose: claude.PurposeDrafting, Model: "claude-sonnet-5-5", Day: day(28),
			InputTokens: 1200, OutputTokens: 300, CacheReadTokens: 900, CacheWriteTokens: 50, CostMicros: 610_000, Batch: true},
		{AccountID: 1, LeadID: 7, Purpose: claude.PurposeResearch, Model: "claude-haiku-5-5", Day: day(28),
			InputTokens: 4000, WebSearches: 3, CostMicros: 230_000},
		{AccountID: 1, LeadID: 8, Purpose: claude.PurposeDrafting, Model: "claude-sonnet-5-5", Day: day(28), CostMicros: 5_000},
		{AccountID: 1, Purpose: claude.PurposeDraftsEval, Model: "claude-sonnet-5-5", Day: day(28), CostMicros: 1_000},
		{AccountID: 2, LeadID: 9, Purpose: claude.PurposeDrafting, Model: "claude-sonnet-5-5", Day: day(28), CostMicros: 99_000},
		{AccountID: 1, LeadID: 7, Purpose: claude.PurposeDrafting, Model: "claude-sonnet-5-5", Day: day(27), CostMicros: 20_000},
	}
	for _, r := range rows {
		if err := q.RecordClaudeUsage(ctx, r); err != nil {
			t.Fatalf("RecordClaudeUsage %+v: %v", r, err)
		}
	}

	// The 28th, 23:59 local is still the 28th: the stored date is the local one.
	late := time.Date(2026, 10, 28, 23, 59, 0, 0, loc)
	for _, c := range []struct {
		name    string
		account int64
		purpose string
		day     time.Time
		want    int64
	}{
		{"account 1, all", 1, "", day(28), 846_000},
		{"account 1, all, 23:59", 1, "", late, 846_000},
		{"account 1, drafting", 1, claude.PurposeDrafting, day(28), 615_000},
		{"account 1, research", 1, claude.PurposeResearch, day(28), 230_000},
		{"account 1, drafts_eval", 1, claude.PurposeDraftsEval, day(28), 1_000},
		{"account 2, all", 2, "", day(28), 99_000},
		{"account 1, the 27th", 1, "", day(27), 20_000},
		{"account 1, the 29th", 1, "", day(29), 0},
		{"account 3", 3, "", day(28), 0},
	} {
		if got, err := q.ClaudeSpendOn(ctx, c.account, c.purpose, c.day); err != nil || got != c.want {
			t.Errorf("ClaudeSpendOn %s = %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	for _, c := range []struct {
		lead    int64
		purpose string
		want    int64
	}{
		{7, "", 860_000}, {7, claude.PurposeDrafting, 630_000}, {7, claude.PurposeResearch, 230_000},
		{8, "", 5_000}, {9, claude.PurposeDrafting, 99_000}, {10, "", 0},
	} {
		if got, err := q.ClaudeSpendForLead(ctx, c.lead, c.purpose); err != nil || got != c.want {
			t.Errorf("ClaudeSpendForLead %d %q = %d, %v; want %d", c.lead, c.purpose, got, err, c.want)
		}
	}

	byPurpose, err := q.ClaudeSpendByPurpose(ctx, 1, day(28))
	if err != nil {
		t.Fatalf("ClaudeSpendByPurpose: %v", err)
	}
	want := map[string]int64{claude.PurposeDrafting: 615_000, claude.PurposeResearch: 230_000, claude.PurposeDraftsEval: 1_000}
	if len(byPurpose) != len(want) {
		t.Fatalf("ClaudeSpendByPurpose = %v, want %v", byPurpose, want)
	}
	for k, v := range want {
		if byPurpose[k] != v {
			t.Errorf("ClaudeSpendByPurpose[%s] = %d, want %d", k, byPurpose[k], v)
		}
	}
	if quiet, err := q.ClaudeSpendByPurpose(ctx, 1, day(29)); err != nil || quiet == nil || len(quiet) != 0 {
		t.Errorf("quiet day = %v, %v; want an empty non-nil map", quiet, err)
	}

	// The first row round-trips whole.
	var (
		provider, model, purpose, stored string
		in, out, cr, cw, ws, calls       int
		cost, account                    int64
		lead                             *int64
		batch                            bool
	)
	if err := storetest.AppConn(t).QueryRow(ctx, `SELECT provider, model, purpose, day::text, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, web_searches, calls, cost_micros, account_id, lead_id, batch
		FROM ai_usage WHERE id = 1`).Scan(&provider, &model, &purpose, &stored, &in, &out, &cr, &cw, &ws, &calls,
		&cost, &account, &lead, &batch); err != nil {
		t.Fatalf("read row 1: %v", err)
	}
	if provider != claude.Provider || model != "claude-sonnet-5-5" || purpose != claude.PurposeDrafting || stored != "2026-10-28" ||
		in != 1200 || out != 300 || cr != 900 || cw != 50 || ws != 0 || calls != 1 || cost != 610_000 || account != 1 ||
		lead == nil || *lead != 7 || !batch {
		t.Fatalf("row 1 = %s %s %s %s %d %d %d %d %d %d %d %d %v %v", provider, model, purpose, stored, in, out, cr, cw, ws,
			calls, cost, account, lead, batch)
	}
	var noLead *int64
	if err := storetest.AppConn(t).QueryRow(ctx, `SELECT lead_id FROM ai_usage WHERE id = 4`).Scan(&noLead); err != nil || noLead != nil {
		t.Fatalf("row 4 lead = %v, %v; want NULL (LeadID 0 stores no lead)", noLead, err)
	}
}

// TestClaudeJevColdStartTwin: Claude spend and Jev counts never read each other. On an empty
// ai_usage both read zero; a day with only Jev rows still reads zero Claude spend; a day with
// only Claude rows reads zero Jev calls and tokens; together, each reads only its own.
func TestClaudeJevColdStartTwin(t *testing.T) {
	ctx := context.Background()
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, bucharest(t))
	jev := []ai.UsageRow{
		{Provider: "typesafe", Model: "jev-1.14.0", Purpose: ai.PurposeJobs, Day: day, Calls: 2, InputTokens: 500},
		{Provider: "typesafe", Model: "jev-1.14.0", Purpose: ai.PurposeLeads, Day: day, Calls: 3, InputTokens: 70},
	}
	cl := []claude.UsageRow{
		{AccountID: 1, LeadID: 7, Purpose: claude.PurposeDrafting, Model: "claude-sonnet-5-5", Day: day, InputTokens: 9000, CostMicros: 610_000},
		{AccountID: 1, Purpose: claude.PurposeResearch, Model: "claude-haiku-5-5", Day: day, InputTokens: 4000, CostMicros: 230_000},
	}
	type reading struct {
		claudeDay, claudeLead int64
		jobs, leads, all      int
		usage                 store.AIUsage
	}
	read := func(t *testing.T, q store.Queries) reading {
		t.Helper()
		var r reading
		var err error
		if r.claudeDay, err = q.ClaudeSpendOn(ctx, 1, "", day); err != nil {
			t.Fatal(err)
		}
		if r.claudeLead, err = q.ClaudeSpendForLead(ctx, 7, ""); err != nil {
			t.Fatal(err)
		}
		if r.jobs, err = q.AICallsOn(ctx, "typesafe", ai.PurposeJobs, day); err != nil {
			t.Fatal(err)
		}
		if r.leads, err = q.AICallsOn(ctx, "typesafe", ai.PurposeLeads, day); err != nil {
			t.Fatal(err)
		}
		// Jev's count over every purpose: AICallsOn filters by provider, so no Claude row counts.
		if r.all, err = q.AICallsOn(ctx, "typesafe", "", day); err != nil {
			t.Fatal(err)
		}
		if r.usage, err = q.AIUsageOn(ctx, day); err != nil {
			t.Fatal(err)
		}
		return r
	}
	record := func(t *testing.T, q store.Queries, withJev, withClaude bool) {
		t.Helper()
		if withJev {
			for _, r := range jev {
				if err := q.RecordAIUsage(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
		}
		if withClaude {
			for _, r := range cl {
				if err := q.RecordClaudeUsage(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	jevOnly := store.AIUsage{JobsCalls: 2, LeadsCalls: 3, InputTokens: 570}
	for _, c := range []struct {
		name            string
		withJev, withCl bool
		want            reading
	}{
		{"empty", false, false, reading{}},
		{"Jev only", true, false, reading{jobs: 2, leads: 3, all: 5, usage: jevOnly}},
		{"Claude only", false, true, reading{claudeDay: 840_000, claudeLead: 610_000, usage: store.AIUsage{ClaudeMicros: 840_000}}},
		{"both", true, true, reading{claudeDay: 840_000, claudeLead: 610_000, jobs: 2, leads: 3, all: 5,
			usage: store.AIUsage{JobsCalls: 2, LeadsCalls: 3, InputTokens: 570, ClaudeMicros: 840_000}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := appStore(t).Q()
			record(t, q, c.withJev, c.withCl)
			if got := read(t, q); got != c.want {
				t.Fatalf("reading = %+v, want %+v", got, c.want)
			}
		})
	}
	// The anthropic provider's call count is the Claude rows only, never Jev's.
	q := appStore(t).Q()
	record(t, q, true, true)
	if n, err := q.AICallsOn(ctx, claude.Provider, "", day); err != nil || n != 2 {
		t.Fatalf("AICallsOn anthropic = %d, %v; want 2", n, err)
	}
	if n, err := q.AICallsOn(ctx, claude.Provider, ai.PurposeJobs, day); err != nil || n != 0 {
		t.Fatalf("AICallsOn anthropic jobs = %d, %v; want 0", n, err)
	}
	if got := (store.AIUsage{ClaudeMicros: 840_000}).ClaudeUSD(); got != 0.84 {
		t.Fatalf("ClaudeUSD = %v, want 0.84", got)
	}
}

// TestClaudeUsageLockSerialises: two callers each read the day's spend, wait, then record;
// under WithClaudeUsageLock the second reads the first's row, so they never both read zero.
func TestClaudeUsageLockSerialises(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, bucharest(t))
	var (
		mu   sync.Mutex
		seen []int64
		wg   sync.WaitGroup
		errs = make([]error, 2)
	)
	for i := range 2 {
		wg.Go(func() {
			errs[i] = s.Q().WithClaudeUsageLock(ctx, func(st claude.UsageStore) error {
				spent, err := st.ClaudeSpendOn(ctx, 1, claude.PurposeDrafting, day)
				if err != nil {
					return err
				}
				mu.Lock()
				seen = append(seen, spent)
				mu.Unlock()
				time.Sleep(200 * time.Millisecond)
				return st.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, Purpose: claude.PurposeDrafting,
					Model: "claude-sonnet-5-5", Day: day, CostMicros: 100})
			})
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("WithClaudeUsageLock: %v", err)
		}
	}
	if len(seen) != 2 || seen[0]+seen[1] != 100 || (seen[0] != 0 && seen[1] != 0) {
		t.Fatalf("spend seen = %v; want one caller to read 0 and the other 100", seen)
	}
	if got, err := s.Q().ClaudeSpendOn(ctx, 1, "", day); err != nil || got != 200 {
		t.Fatalf("spend after = %d, %v; want 200", got, err)
	}

	// A failing fn leaves no row, and its error comes back as it is.
	boom := errors.New("boom")
	err := s.Q().WithClaudeUsageLock(ctx, func(st claude.UsageStore) error {
		if err := st.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, Purpose: claude.PurposeResearch,
			Model: "claude-haiku-5-5", Day: day, CostMicros: 5}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if got, err := s.Q().ClaudeSpendOn(ctx, 1, claude.PurposeResearch, day); err != nil || got != 0 {
		t.Fatalf("research spend after rollback = %d, %v; want 0", got, err)
	}
}

// coreProvider is goose over the core migrations on disk, for stepping one version down and up.
func coreProvider(t *testing.T) *goose.Provider {
	t.Helper()
	cfg, err := pgx.ParseConfig(storetest.MigrateDSN())
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestClaudeUsageDown: 00009's Down deletes the Claude-purpose rows, keeps Jev's, drops the
// new columns and restores the two-purpose CHECK; Up again brings them back.
func TestClaudeUsageDown(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	t.Cleanup(func() { storetest.ResetDB(t) })
	owner := storetest.OwnerConn(t)
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, bucharest(t))
	if err := q.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "m", Purpose: ai.PurposeLeads, Day: day, Calls: 1, InputTokens: 9}); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, LeadID: 7, Purpose: claude.PurposeDrafting, Model: "m", Day: day, CostMicros: 1}); err != nil {
		t.Fatal(err)
	}
	p := coreProvider(t)
	if _, err := p.DownTo(ctx, claudeCostVersion-1); err != nil {
		t.Fatalf("down to %d: %v", claudeCostVersion-1, err)
	}
	var rows, cols int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM ai_usage WHERE provider = 'typesafe' AND purpose = 'leads'`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("after down: Jev rows = %d, %v; want 1", rows, err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM ai_usage`).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("after down: rows = %d, %v; want 1 (the Claude row deleted)", rows, err)
	}
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
		AND table_name = 'ai_usage' AND column_name IN ('output_tokens', 'cache_read_tokens', 'cache_write_tokens',
		'web_searches', 'cost_micros', 'account_id', 'lead_id', 'batch')`).Scan(&cols); err != nil || cols != 0 {
		t.Errorf("after down: %d new columns remain (%v)", cols, err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO ai_usage (provider, model, day, calls, input_tokens, purpose)
		VALUES ('x', 'm', '2026-10-28', 1, 0, 'drafting')`); sqlState(err) != "23514" {
		t.Errorf("after down, purpose drafting: err = %v, want 23514 (the old CHECK is back)", err)
	}
	if _, err := p.UpTo(ctx, claudeCostVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := q.RecordClaudeUsage(ctx, claude.UsageRow{AccountID: 1, Purpose: claude.PurposeDrafting, Model: "m", Day: day, CostMicros: 3}); err != nil {
		t.Fatalf("record after up again: %v", err)
	}
	if err := store.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("migrate to latest after up again: %v", err)
	}
}
