//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// sqlState returns the SQLSTATE of a Postgres error, or "" for any other error.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// leadMigrationFiles returns the number of the newest file in migrations/ (the files the
// binary embeds) and how many there are.
func leadMigrationFiles(t *testing.T) (latest int64, count int64) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations: %v (%d files)", err, len(names))
	}
	for _, n := range names {
		num, _, _ := strings.Cut(filepath.Base(n), "_")
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			t.Fatalf("migration %s: no leading number: %v", n, err)
		}
		latest = max(latest, v)
	}
	return latest, int64(len(names))
}

// dropSchema empties the database as the owner: the schema goes with every table and both
// goose version tables.
func dropSchema(t *testing.T, owner *pgx.Conn) {
	t.Helper()
	for _, q := range []string{
		"DROP SCHEMA public CASCADE",
		"CREATE SCHEMA public",
		"GRANT USAGE ON SCHEMA public TO leadhound_app", // the per-database line in deploy/sql/grants.sql
	} {
		if _, err := owner.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// migrateAll runs the core migrations, then the leads migrations.
func migrateAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := store.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	if err := leadstore.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("leads migrate: %v", err)
	}
}

// appliedCount returns how many applied migrations a goose version table records.
func appliedCount(t *testing.T, owner *pgx.Conn, table string) int64 {
	t.Helper()
	var n int64
	q := "SELECT count(*) FROM " + pgx.Identifier{table}.Sanitize() + " WHERE version_id > 0"
	if err := owner.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return n
}

// appHas reports whether the app role holds priv on table.
func appHas(t *testing.T, owner *pgx.Conn, table, priv string) bool {
	t.Helper()
	var ok bool
	q := "SELECT has_table_privilege('leadhound_app', format('public.%I', $1::text), $2)"
	if err := owner.QueryRow(context.Background(), q, table, priv).Scan(&ok); err != nil {
		t.Fatalf("has_table_privilege(%s, %s): %v", table, priv, err)
	}
	return ok
}

// appHasColumn reports whether the app role holds priv on column of table.
func appHasColumn(t *testing.T, owner *pgx.Conn, table, column, priv string) bool {
	t.Helper()
	var ok bool
	q := "SELECT has_column_privilege('leadhound_app', format('public.%I', $1::text), $2, $3)"
	if err := owner.QueryRow(context.Background(), q, table, column, priv).Scan(&ok); err != nil {
		t.Fatalf("has_column_privilege(%s.%s, %s): %v", table, column, priv, err)
	}
	return ok
}

// appendOnlyLeadTables are the append-only lead tables from 00010 on: INSERT and SELECT, no
// UPDATE, no DELETE.
var appendOnlyLeadTables = []string{"finding", "lead_star", "org_fact", "enrich_attempt", "lead_relevance",
	"lead_relevance_eval", "lead_setting", "lead_label", "campaign_version", "account", "call_outcome", "email_consent",
	"draft_generation", "draft_review", "draft_eval", "phone_screening", "phone_status", "phone_research",
	"place_import", "place", "job_switch", "campaign_mailbox"}

// referenceLeadTables are read-only to the app role (00010): they change only by migration.
var referenceLeadTables = []string{"lead_kind", "lead_channel"}

// assertLeadPrivileges fails the test when the app role may write the leads version table,
// UPDATE an append-only lead table, write a reference table, or UPDATE an org column other
// than site_id.
func assertLeadPrivileges(t *testing.T, owner *pgx.Conn, when string) {
	t.Helper()
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
		if appHas(t, owner, "goose_db_version_leads", priv) {
			t.Errorf("%s: leadhound_app has %s on goose_db_version_leads, want none", when, priv)
		}
	}
	for _, table := range append([]string{"site_probe", "fetch_header", "contact_route", "agency_finding", "contact_attempt",
		"lead_event", "suppression", "mailbox_event", "sender_check"}, appendOnlyLeadTables...) {
		if appHas(t, owner, table, "UPDATE") {
			t.Errorf("%s: leadhound_app has UPDATE on %s, want none", when, table)
		}
		if !appHas(t, owner, table, "INSERT") {
			t.Errorf("%s: leadhound_app lacks INSERT on %s", when, table)
		}
	}
	if !appHas(t, owner, "site", "UPDATE") {
		t.Errorf("%s: leadhound_app lacks UPDATE on site (its state pointers)", when)
	}
	if appHas(t, owner, "org", "UPDATE") {
		t.Errorf("%s: leadhound_app has table-wide UPDATE on org, want site_id only", when)
	}
	if !appHasColumn(t, owner, "org", "site_id", "UPDATE") {
		t.Errorf("%s: leadhound_app lacks UPDATE on org.site_id", when)
	}
	for _, col := range []string{"kind", "domain", "name", "country", "source", "source_fetch_id"} {
		if appHasColumn(t, owner, "org", col, "UPDATE") {
			t.Errorf("%s: leadhound_app has UPDATE on org.%s, want none", when, col)
		}
	}
	if !appHas(t, owner, "org", "INSERT") {
		t.Errorf("%s: leadhound_app lacks INSERT on org", when)
	}
	assertPointerColumns(t, owner, when)
	if !appHas(t, owner, "mailbox_cursor", "UPDATE") || !appHas(t, owner, "mailbox_cursor", "INSERT") {
		t.Errorf("%s: leadhound_app lacks INSERT or UPDATE on mailbox_cursor (every column a pointer)", when)
	}
	// suppression_lift (00009) is append-only like suppression; the suppression_active view is
	// read-only to the app, or it could UPDATE suppression through it (an auto-updatable view).
	if appHas(t, owner, "suppression_lift", "UPDATE") || !appHas(t, owner, "suppression_lift", "INSERT") ||
		!appHas(t, owner, "suppression_lift", "SELECT") {
		t.Errorf("%s: leadhound_app on suppression_lift: want SELECT and INSERT, no UPDATE", when)
	}
	if !appHas(t, owner, "suppression_active", "SELECT") || appHas(t, owner, "suppression_active", "INSERT") ||
		appHas(t, owner, "suppression_active", "UPDATE") {
		t.Errorf("%s: leadhound_app on suppression_active: want SELECT only", when)
	}
	for _, table := range referenceLeadTables {
		if !appHas(t, owner, table, "SELECT") {
			t.Errorf("%s: leadhound_app lacks SELECT on %s", when, table)
		}
		for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
			if appHas(t, owner, table, priv) {
				t.Errorf("%s: leadhound_app has %s on %s, want SELECT only", when, priv, table)
			}
		}
	}
	for _, table := range append([]string{"lead", "lead_event", "outreach_batch", "outreach_message", "suppression",
		"suppression_lift", "suppression_active", "mailbox_event", "mailbox_cursor", "sender_check", "campaign", "contact_research"}, appendOnlyLeadTables...) {
		if appHas(t, owner, table, "DELETE") || appHas(t, owner, table, "TRUNCATE") {
			t.Errorf("%s: leadhound_app may DELETE or TRUNCATE %s", when, table)
		}
	}
}

// pointerColumns are the outreach tables whose app UPDATE is cut to their state pointers
// (00003's header): table → pointer columns, then the columns that must stay fixed.
var pointerColumns = map[string][2][]string{
	"lead": {{"state", "state_at", "kind", "probe_id", "agency_finding_id", "finding_id", "channel", "route_id"},
		{"org_id", "created_at", "campaign_id"}},
	"outreach_batch": {{"approved_at", "approved_hash"},
		{"shadow", "created_at", "content_sha256", "templates_sha256", "account_id", "campaign_id", "sequence"}},
	// 00016: a campaign's one pointer is retired_at; its owner, slug and name are fixed.
	"campaign": {{"retired_at"}, {"account_id", "slug", "name", "created_at"}},
	// 00018: a research row moves its run columns only; who, what and why it was asked are fixed.
	"contact_research": {{"status", "model", "cost_micros", "candidates", "verified", "started_at", "finished_at"},
		{"org_id", "lead_id", "campaign_id", "trigger", "want", "requested_at", "actor"}},
	"outreach_message": {{"state", "due_at", "sent_at", "smtp_reply", "cancel_reason", "approved_sha256"},
		{"batch_id", "lead_id", "step", "channel", "to_address", "subject", "body_text", "message_id",
			"in_reply_to", "slots", "created_at", "condition", "phone_fact_id", "sequence_name"}},
}

// assertPointerColumns fails the test when the app role holds table-wide UPDATE on an
// outreach table with pointers, lacks UPDATE on a pointer, or holds it on another column.
func assertPointerColumns(t *testing.T, owner *pgx.Conn, when string) {
	t.Helper()
	for table, cols := range pointerColumns {
		if appHas(t, owner, table, "UPDATE") {
			t.Errorf("%s: leadhound_app has table-wide UPDATE on %s", when, table)
		}
		if !appHas(t, owner, table, "INSERT") {
			t.Errorf("%s: leadhound_app lacks INSERT on %s", when, table)
		}
		for _, c := range cols[0] {
			if !appHasColumn(t, owner, table, c, "UPDATE") {
				t.Errorf("%s: leadhound_app lacks UPDATE on %s.%s", when, table, c)
			}
		}
		for _, c := range cols[1] {
			if !appHasColumn(t, owner, table, c, "SELECT") {
				t.Errorf("%s: leadhound_app lacks SELECT on %s.%s", when, table, c)
			}
			if appHasColumn(t, owner, table, c, "UPDATE") {
				t.Errorf("%s: leadhound_app has UPDATE on %s.%s, want none", when, table, c)
			}
		}
	}
}

func TestLeadsMigrateFromEmpty(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	dropSchema(t, owner)
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatalf("read grants.sql: %v", err)
	}
	// grants.sql runs clean on an empty database: every revoke is guarded by its table.
	if _, err := owner.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("run grants.sql on an empty database: %v", err)
	}
	migrateAll(t)

	var version int64
	if err := owner.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version_leads").Scan(&version); err != nil {
		t.Fatalf("read leads goose version: %v", err)
	}
	latest, count := leadMigrationFiles(t)
	if version != latest {
		t.Fatalf("leads goose version after migrate = %d, want %d", version, latest)
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != count {
		t.Fatalf("leads goose rows = %d, want %d", got, count)
	}

	// 00016 seeds the default account and its Drupal campaign.
	var acct, camp string
	if err := owner.QueryRow(ctx, `SELECT a.slug, c.slug FROM campaign c JOIN account a ON a.id = c.account_id
		WHERE a.id = 1 AND c.id = 1`).Scan(&acct, &camp); err != nil || acct != "operator" || camp != "drupal-eol" {
		t.Fatalf("seeded account and campaign = %q %q (%v), want operator drupal-eol", acct, camp, err)
	}

	// 00017: a phone suppression holds a hash, never the number, and the phone reasons are
	// phone-only; the step, channel and condition lists and the consent pairing are in place.
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	hash := "sha256:" + strings.Repeat("ab", 32)
	if _, err := tx.Exec(ctx, "INSERT INTO suppression (kind, value, reason) VALUES ('phone', $1, 'do_not_call')", hash); err != nil {
		t.Fatalf("insert hashed phone suppression: %v", err)
	}
	for _, q := range []string{
		"INSERT INTO suppression (kind, value, reason) VALUES ('phone', '+441234567890', 'tps')",
		"INSERT INTO suppression (kind, value, reason) VALUES ('domain', 'tps.example', 'ctps')",
	} {
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		if _, err := tx.Exec(ctx, q); sqlState(err) != "23514" {
			t.Errorf("%s: err = %v, want 23514 check_violation", q, err)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	for _, c := range [][2]string{
		{"outreach_message_step_check", "step <= 9"},
		{"outreach_message_channel_check", "'phone'"},
		{"outreach_message_condition_check", "'email_consent'"},
		{"outreach_message_phone_fact_check", "'phone'"},
		{"outreach_batch_sequence_check", "'object'"},
		{"call_outcome_consent_check", "consent_address"},
		{"call_outcome_note_check", "500"},
		{"email_consent_wording_check", "500"},
		{"contact_research_trigger_check", "'button'"},
		{"contact_research_status_check", "'capped'"},
		{"contact_research_want_check", "'phone'"},
		{"contact_research_found_check", "verified > 0"},
		{"contact_research_times_check", "started_at IS NULL"},
		{"draft_generation_attempt_check", "2"},
		{"draft_generation_purpose_check", "'eval'"},
		{"draft_generation_ok_check", "jsonb_array_length"},
		{"draft_generation_fact_sha_check", "{64}"},
		{"draft_review_verdict_check", "'reject'"},
		{"draft_review_reasons_check", "'wrong_tone'"},
		{"draft_review_note_check", "500"},
		{"draft_eval_share_check", "after_retry_pass >= first_try_pass"},
		{"draft_eval_decider_check", "'claude'"},
		{"phone_screening_register_check", "'gr_art11'"},
		{"phone_screening_result_check", "'listed'"},
		{"phone_screening_number_check", "{8,15}"},
		{"phone_screening_checked_check", "1 day"},
		{"phone_status_reason_check", "'corrected'"},
		{"phone_status_outcome_check", "call_outcome_id IS NOT NULL"},
		{"phone_research_line_kind_check", "'decision_maker'"},
		{"phone_research_role_check", "60"},
		{"org_source_check", "'place'"},
		{"org_region_check", "[A-Z]{2}"},
		{"site_origin_check", "'place'"},
		{"place_chain_check", "'domain'"},
		{"place_import_counts_check", "no_name"},
		{"place_confidence_check", "1"},
	} {
		var def string
		if err := owner.QueryRow(ctx, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1", c[0]).Scan(&def); err != nil {
			t.Errorf("constraint %s: %v", c[0], err)
		} else if !strings.Contains(def, c[1]) {
			t.Errorf("constraint %s = %s, want it to name %s", c[0], def, c[1])
		}
	}
	var onDelete string
	if err := owner.QueryRow(ctx, `SELECT confdeltype FROM pg_constraint WHERE conrelid = 'outreach_message'::regclass
		AND contype = 'f' AND confrelid = 'org_fact'::regclass`).Scan(&onDelete); err != nil || onDelete != "n" {
		t.Errorf("outreach_message.phone_fact_id on delete = %q (%v), want n (SET NULL)", onDelete, err)
	}
	// 00020: a phone status goes with its fact (CASCADE); a research row outlives it (SET NULL).
	for table, want := range map[string]string{"phone_status": "c", "phone_research": "n"} {
		if err := owner.QueryRow(ctx, `SELECT confdeltype FROM pg_constraint WHERE conrelid = $1::regclass
			AND contype = 'f' AND confrelid = 'org_fact'::regclass`, table).Scan(&onDelete); err != nil || onDelete != want {
			t.Errorf("%s.fact_id on delete = %q (%v), want %s", table, onDelete, err, want)
		}
	}

	// The app role can write the new tables through the owner's default privileges.
	app := storetest.AppConn(t)
	var runID, fetchID, siteID int64
	if err := app.QueryRow(ctx, "INSERT INTO run (command) VALUES ('leads-sites') RETURNING id").Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := app.QueryRow(ctx,
		"INSERT INTO http_fetch (run_id, source, kind, url, status) VALUES ($1, 'site', 'site', 'https://northwind.example/', 200) RETURNING id", runID).Scan(&fetchID); err != nil {
		t.Fatalf("insert http_fetch: %v", err)
	}
	if err := app.QueryRow(ctx,
		"INSERT INTO site (domain, origin) VALUES ('northwind.example', 'manual') RETURNING id").Scan(&siteID); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	var probeID int64
	if err := app.QueryRow(ctx,
		"INSERT INTO site_probe (site_id, run_id, outcome, fetch_ids, derived_by) VALUES ($1, $2, 'ok', ARRAY[$3::bigint], 'drupal@1') RETURNING id",
		siteID, runID, fetchID).Scan(&probeID); err != nil {
		t.Fatalf("insert site_probe: %v", err)
	}
	if _, err := app.Exec(ctx, "INSERT INTO fetch_header (fetch_id, headers) VALUES ($1, '{}')", fetchID); err != nil {
		t.Fatalf("insert fetch_header: %v", err)
	}
	if _, err := app.Exec(ctx, "UPDATE site SET state = 'probed', last_probe_id = $1 WHERE id = $2", probeID, siteID); err != nil {
		t.Fatalf("update site state pointers: %v", err)
	}
	assertLeadPrivileges(t, owner, "after migrate")

	// grants.sql after migrate (the deploy order) re-grants every table and must still leave
	// the leads revokes in place; a migrate after it keeps them too.
	if _, err := owner.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("run grants.sql after migrate: %v", err)
	}
	assertLeadPrivileges(t, owner, "after grants.sql")
	migrateAll(t)
	assertLeadPrivileges(t, owner, "after grants.sql then migrate")
	leadstoretest.Reset(t)
}

func TestLeadsMigrateIdempotent(t *testing.T) {
	leadstoretest.Migrated(t)
	owner := storetest.OwnerConn(t)
	before := appliedCount(t, owner, "goose_db_version_leads")
	coreBefore := appliedCount(t, owner, "goose_db_version")
	for range 2 {
		if err := leadstore.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
			t.Fatalf("leads migrate again: %v", err)
		}
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != before {
		t.Fatalf("leads goose rows after re-migrate = %d, want %d (a no-op)", got, before)
	}
	if got := appliedCount(t, owner, "goose_db_version"); got != coreBefore {
		t.Fatalf("core goose rows after leads migrate = %d, want %d", got, coreBefore)
	}
	// A no-op migrate still runs the revokes: 00016's grants hold after it.
	assertLeadPrivileges(t, owner, "after a no-op migrate")
}

func TestLeadsMigrateNeedsCoreSchema(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	dropSchema(t, owner)
	t.Cleanup(func() { migrateAll(t) })

	err := leadstore.Migrate(ctx, storetest.MigrateDSN())
	if !errors.Is(err, leadstore.ErrNoCoreSchema) {
		t.Fatalf("leads migrate on an empty database: err = %v, want ErrNoCoreSchema", err)
	}
	// Nothing was created: not the version table, not a lead table.
	var made bool
	if err := owner.QueryRow(ctx,
		"SELECT to_regclass('public.goose_db_version_leads') IS NOT NULL OR to_regclass('public.site') IS NOT NULL").Scan(&made); err != nil {
		t.Fatalf("check tables: %v", err)
	}
	if made {
		t.Fatal("leads migrate without the core schema created tables")
	}
}

func TestLeadAppRoleCannotDelete(t *testing.T) {
	leadstoretest.Reset(t)
	app := storetest.AppConn(t)
	for _, q := range []string{
		"DELETE FROM site",
		"DELETE FROM site_probe",
		"DELETE FROM fetch_header",
		"DELETE FROM org",
		"DELETE FROM contact_route",
		"DELETE FROM agency_finding",
		"DELETE FROM contact_attempt",
		"DELETE FROM lead",
		"DELETE FROM lead_event",
		"DELETE FROM outreach_batch",
		"DELETE FROM outreach_message",
		"DELETE FROM suppression",
		"DELETE FROM suppression_lift",
		"DELETE FROM suppression_active",
		"DELETE FROM mailbox_event",
		"DELETE FROM mailbox_cursor",
		"DELETE FROM sender_check",
		"DELETE FROM finding",
		"DELETE FROM lead_star",
		"DELETE FROM org_fact",
		"DELETE FROM enrich_attempt",
		"DELETE FROM lead_relevance",
		"DELETE FROM lead_relevance_eval",
		"DELETE FROM lead_setting",
		"DELETE FROM lead_label",
		"DELETE FROM account",
		"DELETE FROM campaign",
		"DELETE FROM campaign_version",
		"TRUNCATE campaign",
		"DELETE FROM call_outcome",
		"DELETE FROM email_consent",
		"TRUNCATE call_outcome",
		"DELETE FROM contact_research",
		"TRUNCATE contact_research",
		"DELETE FROM draft_generation",
		"DELETE FROM draft_review",
		"DELETE FROM draft_eval",
		"TRUNCATE draft_generation",
		"DELETE FROM phone_screening",
		"DELETE FROM phone_status",
		"DELETE FROM phone_research",
		"TRUNCATE phone_screening",
		"TRUNCATE phone_status",
		"TRUNCATE phone_research",
		"DELETE FROM place",
		"DELETE FROM place_import",
		"TRUNCATE place",
		"DELETE FROM job_switch",
		"TRUNCATE job_switch",
		"DELETE FROM campaign_mailbox",
		"TRUNCATE campaign_mailbox",
		"DELETE FROM lead_kind",
		"DELETE FROM lead_channel",
		"TRUNCATE lead_kind",
		"TRUNCATE suppression",
		"TRUNCATE site",
		"DROP TABLE site",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

func TestLeadAppRoleCannotUpdateAppendOnly(t *testing.T) {
	leadstoretest.Reset(t)
	app := storetest.AppConn(t)
	for _, q := range []string{
		"UPDATE site_probe SET outcome = 'ok'",
		"UPDATE fetch_header SET headers = '{}'",
		"UPDATE contact_route SET mx_ok = true",
		"UPDATE agency_finding SET phrase = ''",
		"UPDATE contact_attempt SET routes_found = 0",
		"UPDATE org SET name = ''",
		"UPDATE lead_event SET reason = ''",
		"UPDATE suppression SET reason = 'manual'",
		"UPDATE suppression_lift SET reason = 'rewritten'",
		// An auto-updatable view would write suppression with the owner's rights.
		"UPDATE suppression_active SET reason = 'manual'",
		"INSERT INTO suppression_active (kind, value, reason) VALUES ('domain', 'view.invalid', 'manual')",
		"UPDATE mailbox_event SET kind = 'reply'",
		"UPDATE sender_check SET spf = true",
		"UPDATE outreach_message SET body_text = ''",
		"UPDATE outreach_batch SET content_sha256 = ''",
		"UPDATE lead SET org_id = 1",
		"UPDATE lead SET created_at = now()",
		"UPDATE finding SET subject = '{}'",
		"UPDATE lead_star SET position = 1",
		"UPDATE org_fact SET value = 'x'",
		"UPDATE enrich_attempt SET facts_found = 0",
		"UPDATE lead_relevance SET score = 0",
		"UPDATE lead_relevance_eval SET trusted = true",
		"UPDATE lead_setting SET settings = NULL",
		"UPDATE lead_label SET band = NULL",
		// 00016: accounts are fixed, a campaign changes only its retired_at, versions are
		// append-only, and no row moves to another account or campaign.
		"UPDATE account SET name = 'renamed'",
		"UPDATE account SET slug = 'other'",
		"UPDATE campaign SET name = 'renamed'",
		"UPDATE campaign SET slug = 'other'",
		"UPDATE campaign SET account_id = 1",
		"UPDATE campaign_version SET spec = '{}'",
		"UPDATE campaign_version SET spec_sha256 = ''",
		"UPDATE lead SET campaign_id = 1",
		"UPDATE outreach_batch SET account_id = 1",
		"UPDATE sender_mailbox SET account_id = 1",
		"UPDATE suppression SET account_id = 1",
		"UPDATE lead_setting SET account_id = 1",
		// 00017: call outcomes and consents are append-only, and a batch's frozen sequences,
		// its campaign, and a row's condition, phone fact and sequence name are fixed.
		"UPDATE call_outcome SET outcome = 'connected'",
		"UPDATE call_outcome SET note = ''",
		"UPDATE call_outcome SET email_consent = false, consent_address = NULL",
		"UPDATE email_consent SET address = 'x@example.invalid'",
		"UPDATE email_consent SET wording = 'rewritten'",
		"UPDATE outreach_batch SET sequence = NULL",
		"UPDATE outreach_batch SET campaign_id = 1",
		"UPDATE outreach_message SET condition = 'always'",
		"UPDATE outreach_message SET phone_fact_id = NULL",
		"UPDATE outreach_message SET sequence_name = NULL",
		// 00018: a research row never moves to another org, lead, campaign, trigger, want,
		// request time or actor.
		"UPDATE contact_research SET org_id = 1",
		"UPDATE contact_research SET lead_id = 1",
		"UPDATE contact_research SET campaign_id = 1",
		"UPDATE contact_research SET trigger = 'cli'",
		"UPDATE contact_research SET want = ARRAY['email']",
		"UPDATE contact_research SET requested_at = now()",
		"UPDATE contact_research SET actor = 'system'",
		// 00019: Claude drafts, reviews and evals are append-only.
		"UPDATE draft_generation SET check_ok = true",
		"UPDATE draft_generation SET output = NULL",
		"UPDATE draft_review SET verdict = 'approve'",
		"UPDATE draft_review SET note = ''",
		"UPDATE draft_eval SET first_try_pass = 1",
		"UPDATE phone_screening SET result = 'clean'",
		"UPDATE phone_screening SET checked_at = now()",
		"UPDATE phone_status SET status = 'good'",
		"UPDATE phone_status SET fact_id = 1",
		"UPDATE phone_research SET fact_id = NULL",
		"UPDATE phone_research SET role = ''",
		// 00021: places and imports are append-only; an org's region is fixed at insert.
		"UPDATE place SET org_id = NULL",
		"UPDATE place SET phones = '[]'",
		"UPDATE place_import SET kept = 0",
		"UPDATE org SET region = 'US-IL'",
		// 00023 and 00024: job switches and campaign mailbox assignments are append-only.
		"UPDATE job_switch SET is_on = true",
		"UPDATE job_switch SET reason = ''",
		"UPDATE campaign_mailbox SET assigned = true",
		"UPDATE campaign_mailbox SET mailbox_id = 1",
		// The reference rows change only by migration.
		"UPDATE lead_kind SET outreach = true",
		"UPDATE lead_channel SET label = ''",
		"INSERT INTO lead_kind (name, label, outreach) VALUES ('forged', 'Forged', true)",
		"INSERT INTO lead_channel (name, label) VALUES ('pigeon', 'Pigeon')",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

func TestLeadAppRoleCannotWriteVersionTable(t *testing.T) {
	leadstoretest.Migrated(t)
	owner := storetest.OwnerConn(t)
	assertLeadPrivileges(t, owner, "after migrate")
	app := storetest.AppConn(t)
	for _, q := range []string{
		"INSERT INTO goose_db_version_leads (version_id, is_applied) VALUES (999, true)",
		"UPDATE goose_db_version_leads SET is_applied = false",
		"DELETE FROM goose_db_version_leads",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

func TestResetKeepsVersionTables(t *testing.T) {
	leadstoretest.Reset(t)
	owner := storetest.OwnerConn(t)
	_, leadCount := leadMigrationFiles(t)
	core := appliedCount(t, owner, "goose_db_version")
	if core == 0 {
		t.Fatal("goose_db_version is empty after Reset")
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != leadCount {
		t.Fatalf("goose_db_version_leads rows after Reset = %d, want %d", got, leadCount)
	}

	storetest.ResetDB(t)
	if err := leadstore.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
		t.Fatalf("leads migrate after ResetDB: %v", err)
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != leadCount {
		t.Fatalf("goose_db_version_leads rows after ResetDB and migrate = %d, want %d (a no-op)", got, leadCount)
	}
	if got := appliedCount(t, owner, "goose_db_version"); got != core {
		t.Fatalf("goose_db_version rows after ResetDB = %d, want %d", got, core)
	}
	// The lead tables were emptied with the rest.
	var sites int64
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM site").Scan(&sites); err != nil {
		t.Fatalf("count sites: %v", err)
	}
	if sites != 0 {
		t.Fatalf("site rows after Reset = %d, want 0", sites)
	}
}
