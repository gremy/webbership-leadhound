//go:build integration

package leadstore_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migrations 00023 (job_switch) and 00024 (campaign_mailbox, its seed, and
// mailbox_check.landing_url).

const campaignMailboxVersion = 24 // 00024_campaign_mailbox.sql

// toVersionBefore24 empties the database and migrates it to 00023: core, then leads up to 23.
func toVersionBefore24(t *testing.T, owner *pgx.Conn) {
	t.Helper()
	dropSchema(t, owner)
	t.Cleanup(func() {
		migrateAll(t)
		leadstoretest.Reset(t)
	})
	if err := store.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	if _, err := leadsProvider(t).UpTo(context.Background(), campaignMailboxVersion-1); err != nil {
		t.Fatalf("leads up to %d: %v", campaignMailboxVersion-1, err)
	}
}

func TestMigrationCampaignMailboxSeed(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	toVersionBefore24(t, owner)

	mailbox := func(local string) int64 {
		return ownerInt(t, owner, `INSERT INTO sender_mailbox (address, domain, login, smtp_addr, imap_addr, dkim_selector, password_sealed, sealed_by)
			VALUES ($1 || '@sender.example', 'sender.example', $1 || '@sender.example', 'smtp.sender.invalid:465', 'imap.sender.invalid:993', 'lh', '\x00', 'k')
			RETURNING id`, local)
	}
	cafe := ownerInt(t, owner, `INSERT INTO campaign (account_id, slug, name) VALUES (1, 'cafe', 'Cafe') RETURNING id`)
	batch := func(campaign int64, shadow bool) int64 {
		return ownerInt(t, owner, `INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256, campaign_id)
			VALUES ($1, '\x00', '\x00', $2) RETURNING id`, shadow, campaign)
	}
	drupalLive, cafeLive := batch(1, false), batch(cafe, false)
	drupalShadow, cafeShadow := batch(1, true), batch(cafe, true)
	orgN := 0
	message := func(b, campaign, mb int64) {
		orgN++
		org := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'org' || $1::int || '.example', 'probe') RETURNING id`, orgN)
		lead := ownerInt(t, owner, `INSERT INTO lead (org_id, kind, channel, campaign_id) VALUES ($1, 'hand', 'email', $2) RETURNING id`, org, campaign)
		ownerRun(t, owner, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, mailbox_id)
			VALUES ($1, $2, 0, 'email', 'hello', '{}', $3)`, b, lead, mb)
	}
	// selftest writes what leadstore.CreateSelftest does: an org and lead of kind selftest and
	// its own live one-message batch, on campaign 1 (the batch default).
	selftestN := 0
	selftest := func(mb int64) {
		selftestN++
		org := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('selftest', 'selftest-' || $1::int, 'selftest') RETURNING id`, selftestN)
		lead := ownerInt(t, owner, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'selftest', 'email') RETURNING id`, org)
		b := ownerInt(t, owner, `INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256) VALUES (false, '\x00', '\x00') RETURNING id`)
		ownerRun(t, owner, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, mailbox_id)
			VALUES ($1, $2, 0, 'email', 'hello', '{}', $3)`, b, lead, mb)
	}

	// one: only campaign 1's live messages (two of them), plus a shadow message of the
	// café campaign, which does not count.
	one := mailbox("one")
	message(drupalLive, 1, one)
	message(drupalLive, 1, one)
	message(cafeShadow, cafe, one)
	// cafeOnly: only the café campaign's live messages.
	cafeOnly := mailbox("cafe")
	message(cafeLive, cafe, cafeOnly)
	// two: live messages of both campaigns.
	two := mailbox("two")
	message(drupalLive, 1, two)
	message(cafeLive, cafe, two)
	// shadowOnly: shadow messages only, so no live message.
	shadowOnly := mailbox("shadow")
	message(drupalShadow, 1, shadowOnly)
	// none: no message at all.
	none := mailbox("none")
	// retired: only campaign 1's live messages, but retired.
	retired := mailbox("retired")
	message(drupalLive, 1, retired)
	ownerRun(t, owner, `INSERT INTO mailbox_state (mailbox_id, state, actor) VALUES ($1, 'retired', 'operator')`, retired)
	// paused: only campaign 1's live messages and paused; paused is not retired, so it is seeded.
	paused := mailbox("paused")
	message(drupalLive, 1, paused)
	ownerRun(t, owner, `INSERT INTO mailbox_state (mailbox_id, state, actor) VALUES ($1, 'paused', 'operator')`, paused)

	// selftestOnly: only a selftest (a live batch on campaign 1), so no row.
	selftestOnly := mailbox("selftest")
	selftest(selftestOnly)
	// cafeSelftest: the café campaign's live messages plus a selftest on campaign 1; the
	// selftest does not make it two campaigns.
	cafeSelftest := mailbox("cafeselftest")
	message(cafeLive, cafe, cafeSelftest)
	selftest(cafeSelftest)
	// drupalSelftest: campaign 1's live messages plus a selftest.
	drupalSelftest := mailbox("drupalselftest")
	message(drupalLive, 1, drupalSelftest)
	selftest(drupalSelftest)

	if _, err := leadsProvider(t).UpTo(ctx, campaignMailboxVersion); err != nil {
		t.Fatalf("up to %d: %v", campaignMailboxVersion, err)
	}

	want := map[int64]int64{one: 1, cafeOnly: cafe, paused: 1, cafeSelftest: cafe, drupalSelftest: 1} // mailbox → campaign
	rows, err := owner.Query(ctx, `SELECT campaign_id, mailbox_id, assigned, actor FROM campaign_mailbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]int64{}
	for rows.Next() {
		var c, m int64
		var assigned bool
		var actor string
		if err := rows.Scan(&c, &m, &assigned, &actor); err != nil {
			t.Fatal(err)
		}
		if !assigned || actor != "system" {
			t.Errorf("mailbox %d: assigned %v by %q, want true by system", m, assigned, actor)
		}
		if _, dup := got[m]; dup {
			t.Errorf("mailbox %d seeded twice", m)
		}
		got[m] = c
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for m, c := range want {
		if got[m] != c {
			t.Errorf("mailbox %d seeded to campaign %d, want %d", m, got[m], c)
		}
	}
	for _, m := range []int64{two, shadowOnly, none, retired, selftestOnly} {
		if c, ok := got[m]; ok {
			t.Errorf("mailbox %d seeded to campaign %d, want no row", m, c)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%d seed rows, want %d: %v", len(got), len(want), got)
	}

	// Down drops the table and the column; up again seeds the same rows.
	p := leadsProvider(t)
	if _, err := p.DownTo(ctx, campaignMailboxVersion-1); err != nil {
		t.Fatalf("down to %d: %v", campaignMailboxVersion-1, err)
	}
	var reg *string
	if err := owner.QueryRow(ctx, `SELECT to_regclass('public.campaign_mailbox')::text`).Scan(&reg); err != nil || reg != nil {
		t.Errorf("campaign_mailbox after down: %v %v, want gone", reg, err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
		AND table_name = 'mailbox_check' AND column_name = 'landing_url'`); n != 0 {
		t.Error("mailbox_check.landing_url left after down")
	}
	if _, err := p.UpTo(ctx, campaignMailboxVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM campaign_mailbox`); n != int64(len(want)) {
		t.Errorf("%d seed rows after down and up, want %d", n, len(want))
	}
}

func TestMigrationCampaignMailboxEmpty(t *testing.T) {
	owner := storetest.OwnerConn(t)
	toVersionBefore24(t, owner)
	if _, err := leadsProvider(t).UpTo(context.Background(), campaignMailboxVersion); err != nil {
		t.Fatalf("up to %d: %v", campaignMailboxVersion, err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM campaign_mailbox`); n != 0 {
		t.Errorf("%d seed rows on an empty database, want none", n)
	}
}

// TestJobSwitchAndCampaignMailboxRules writes the new tables as the app role: the defaults,
// the checks, the landing_url grant, and no UPDATE.
func TestJobSwitchAndCampaignMailboxRules(t *testing.T) {
	ctx := context.Background()
	leadstoretest.Reset(t)
	owner := storetest.OwnerConn(t)
	app := storetest.AppConn(t)

	var account int64
	var reason, actor string
	if err := app.QueryRow(ctx, `INSERT INTO job_switch (job, is_on, actor) VALUES ('batch', false, 'gremy')
		RETURNING account_id, reason, actor`).Scan(&account, &reason, &actor); err != nil {
		t.Fatalf("insert job switch as app: %v", err)
	}
	if account != 1 || reason != "" || actor != "gremy" {
		t.Errorf("job switch defaults: account %d reason %q actor %q", account, reason, actor)
	}
	for _, job := range []string{"sync", "probe", "contacts", "research", "verify"} {
		if _, err := app.Exec(ctx, `INSERT INTO job_switch (job, is_on, reason, actor) VALUES ($1, true, 'back on', 'system')`, job); err != nil {
			t.Errorf("job %s: %v", job, err)
		}
	}
	if _, err := app.Exec(ctx, `INSERT INTO job_switch (job, is_on, reason, actor) VALUES ('batch', true, $1, 'gremy')`,
		strings.Repeat("x", 300)); err != nil {
		t.Errorf("300-character reason: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO job_switch (job, is_on, actor) VALUES ('send', false, 'gremy')`,
		`INSERT INTO job_switch (job, is_on, actor) VALUES ('build', false, 'gremy')`,
		`INSERT INTO job_switch (job, is_on, actor) VALUES ('batch', false, 'operator')`,
		`INSERT INTO job_switch (job, is_on, reason, actor) VALUES ('batch', false, E'two\nlines', 'gremy')`,
		`INSERT INTO job_switch (job, is_on, reason, actor) VALUES ('batch', false, E'cr\r', 'gremy')`,
		`INSERT INTO job_switch (job, is_on, reason, actor) VALUES ('batch', false, repeat('x', 301), 'gremy')`,
	} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "23514" {
			t.Errorf("%s: err = %v, want 23514 check_violation", q, err)
		}
	}
	if _, err := app.Exec(ctx, `INSERT INTO job_switch (account_id, job, is_on, actor) VALUES (999, 'batch', false, 'gremy')`); sqlState(err) != "23503" {
		t.Errorf("job switch of a missing account: err = %v, want 23503 foreign_key_violation", err)
	}

	mb := ownerInt(t, owner, `INSERT INTO sender_mailbox (address, domain, login, smtp_addr, imap_addr, dkim_selector, password_sealed, sealed_by)
		VALUES ('a@sender.example', 'sender.example', 'a@sender.example', 'smtp.sender.invalid:465', 'imap.sender.invalid:993', 'lh', '\x00', 'k')
		RETURNING id`)
	if _, err := app.Exec(ctx, `INSERT INTO campaign_mailbox (campaign_id, mailbox_id, assigned, actor) VALUES (1, $1, true, 'gremy')`, mb); err != nil {
		t.Fatalf("assign as app: %v", err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO campaign_mailbox (campaign_id, mailbox_id, assigned, actor) VALUES (1, $1, false, 'operator')`, mb); sqlState(err) != "23514" {
		t.Errorf("assign by operator: err = %v, want 23514 check_violation", err)
	}
	for _, q := range []string{
		`INSERT INTO campaign_mailbox (campaign_id, mailbox_id, assigned, actor) VALUES (999, $1, true, 'gremy')`,
		`INSERT INTO campaign_mailbox (campaign_id, mailbox_id, assigned, actor) VALUES (1, $1 + 999, true, 'gremy')`,
	} {
		if _, err := app.Exec(ctx, q, mb); sqlState(err) != "23503" {
			t.Errorf("%s: err = %v, want 23503 foreign_key_violation", q, err)
		}
	}

	// landing_url is a result column the check job writes; the app may UPDATE it.
	if !appHasColumn(t, owner, "mailbox_check", "landing_url", "UPDATE") {
		t.Error("leadhound_app lacks UPDATE on mailbox_check.landing_url")
	}
	if appHas(t, owner, "mailbox_check", "UPDATE") {
		t.Error("leadhound_app has table-wide UPDATE on mailbox_check")
	}
	check := ownerInt(t, owner, `INSERT INTO mailbox_check (mailbox_id, actor) VALUES ($1, 'operator') RETURNING id`, mb)
	if _, err := app.Exec(ctx, `UPDATE mailbox_check SET landing_url = 'https://sender.example/' WHERE id = $1`, check); err != nil {
		t.Errorf("app writes landing_url: %v", err)
	}
	if _, err := app.Exec(ctx, `UPDATE mailbox_check SET mailbox_id = $1 WHERE id = $2`, mb, check); sqlState(err) != "42501" {
		t.Errorf("app moves a check: err = %v, want 42501", err)
	}
}

// TestGrantsSQLCoversNewTables runs grants.sql after a migrate and checks it leaves the new
// tables append-only and landing_url writable, as leadstore.Migrate does.
func TestGrantsSQLCoversNewTables(t *testing.T) {
	leadstoretest.Migrated(t)
	owner := storetest.OwnerConn(t)
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatalf("read grants.sql: %v", err)
	}
	if _, err := owner.Exec(context.Background(), string(grants)); err != nil {
		t.Fatalf("run grants.sql: %v", err)
	}
	for _, table := range []string{"job_switch", "campaign_mailbox"} {
		if appHas(t, owner, table, "UPDATE") {
			t.Errorf("after grants.sql: leadhound_app has UPDATE on %s", table)
		}
		if !appHas(t, owner, table, "INSERT") || !appHas(t, owner, table, "SELECT") {
			t.Errorf("after grants.sql: leadhound_app lacks INSERT or SELECT on %s", table)
		}
	}
	if !appHasColumn(t, owner, "mailbox_check", "landing_url", "UPDATE") {
		t.Error("after grants.sql: leadhound_app lacks UPDATE on mailbox_check.landing_url")
	}
	if appHas(t, owner, "mailbox_check", "UPDATE") {
		t.Error("after grants.sql: leadhound_app has table-wide UPDATE on mailbox_check")
	}
}
