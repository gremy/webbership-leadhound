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

	"github.com/gremy/webbership-leadhound/internal/enrich"
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
	"place_import", "place", "job_switch", "campaign_mailbox", "email_check", "send_override", "lead_stage_event",
	"org_person", "contact_mark", "message_edit", "org_sector", "proof", "proof_retire", "lead_proposal", "lead_proposal_status",
	"lead_place_type", "roastery_outreach_switch",
	"ig_profile", "ig_follow", "ig_match", "ig_judgment", "ig_signal_hint", "ig_review_status",
	"ig_vendor_touch", "lead_intent", "lead_intent_call"}

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
		"suppression_lift", "suppression_active", "mailbox_event", "mailbox_cursor", "sender_check", "campaign", "contact_research",
		"draft_request"}, appendOnlyLeadTables...) {
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
	"outreach_batch": {{"approved_at", "approved_hash", "manual", "content_sha256"},
		{"shadow", "created_at", "templates_sha256", "account_id", "campaign_id", "sequence", "origin"}},
	// 00016: a campaign's one pointer is retired_at; its owner, slug and name are fixed.
	"campaign": {{"retired_at"}, {"account_id", "slug", "name", "created_at"}},
	// 00018: a research row moves its run columns only; who, what and why it was asked are fixed.
	"contact_research": {{"status", "model", "cost_micros", "candidates", "verified", "started_at", "finished_at"},
		{"org_id", "lead_id", "campaign_id", "trigger", "want", "requested_at", "actor"}},
	"outreach_message": {{"state", "due_at", "sent_at", "smtp_reply", "cancel_reason", "approved_sha256", "subject", "body_text"},
		{"batch_id", "lead_id", "step", "channel", "to_address", "message_id",
			"in_reply_to", "slots", "created_at", "condition", "phone_fact_id", "sequence_name"}},
	// 00029: a draft request moves its run columns only; its lead, channel and time are fixed.
	"draft_request": {{"status", "reason", "batch_id", "started_at", "finished_at"},
		{"lead_id", "channel", "requested_at"}},
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
		"DELETE FROM email_check",
		"TRUNCATE email_check",
		"DELETE FROM org_person",
		"DELETE FROM contact_mark",
		"DELETE FROM message_edit",
		"DELETE FROM org_sector",
		"DELETE FROM proof",
		"DELETE FROM proof_retire",
		"DELETE FROM lead_proposal",
		"DELETE FROM lead_proposal_status",
		"DELETE FROM draft_request",
		"DELETE FROM lead_place_type",
		"DELETE FROM roastery_outreach_switch",
		"TRUNCATE lead_place_type",
		"TRUNCATE roastery_outreach_switch",
		// 00031: the Instagram tables are append-only; the owner alone erases from them.
		"DELETE FROM ig_profile",
		"DELETE FROM ig_follow",
		"DELETE FROM ig_match",
		"DELETE FROM ig_judgment",
		"DELETE FROM ig_signal_hint",
		"DELETE FROM ig_review_status",
		"TRUNCATE ig_profile",
		"TRUNCATE ig_follow",
		"TRUNCATE ig_match",
		"TRUNCATE ig_judgment",
		"TRUNCATE ig_signal_hint",
		"TRUNCATE ig_review_status",
		// 00033: the vendor touches, intent classes and intent calls are append-only.
		"DELETE FROM ig_vendor_touch",
		"DELETE FROM lead_intent",
		"DELETE FROM lead_intent_call",
		"TRUNCATE ig_vendor_touch",
		"TRUNCATE lead_intent",
		"TRUNCATE lead_intent_call",
		"TRUNCATE contact_mark",
		"TRUNCATE draft_request",
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
		// 00030: the call a skip wrote is set at INSERT and never changes.
		"UPDATE lead_event SET skip_label_id = NULL",
		"UPDATE lead_place_type SET place_type = 'cafe'",
		"UPDATE roastery_outreach_switch SET is_on = true",
		// 00031: every Instagram row is fixed at insert; the latest row decides.
		"UPDATE ig_profile SET bio = ''",
		"UPDATE ig_follow SET full_name = NULL",
		"UPDATE ig_match SET lead_id = NULL",
		"UPDATE ig_judgment SET choice = 'unclear'",
		"UPDATE ig_signal_hint SET value = 'x'",
		"UPDATE ig_review_status SET status = 'new'",
		// 00033: a touch, a class and a call are fixed at insert; the latest row decides.
		"UPDATE ig_follow SET direction = 'followers'",
		"UPDATE ig_vendor_touch SET text = NULL",
		"UPDATE lead_intent SET class = 'cold'",
		"UPDATE lead_intent_call SET class = NULL",
		"UPDATE suppression SET reason = 'manual'",
		"UPDATE suppression_lift SET reason = 'rewritten'",
		// An auto-updatable view would write suppression with the owner's rights.
		"UPDATE suppression_active SET reason = 'manual'",
		"INSERT INTO suppression_active (kind, value, reason) VALUES ('domain', 'view.invalid', 'manual')",
		"UPDATE mailbox_event SET kind = 'reply'",
		"UPDATE sender_check SET spf = true",
		// 00029: subject, body_text and content_sha256 joined the pointers; the rest stays fixed.
		"UPDATE outreach_message SET to_address = ''",
		"UPDATE outreach_batch SET origin = 'batch'",
		"UPDATE contact_route SET person_id = NULL",
		"UPDATE draft_generation SET channel = 'email'",
		"UPDATE org_person SET name = NULL",
		"UPDATE contact_mark SET note = NULL",
		"UPDATE message_edit SET body_after = ''",
		"UPDATE org_sector SET sector = 'other'",
		"UPDATE proof SET public = true",
		"UPDATE proof_retire SET reason = ''",
		"UPDATE lead_proposal SET title = 'x'",
		"UPDATE lead_proposal_status SET note = ''",
		"UPDATE draft_request SET lead_id = 1",
		"UPDATE draft_request SET channel = 'email'",
		"UPDATE draft_request SET requested_at = now()",
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
		// 00025: email check results are append-only.
		"UPDATE email_check SET status = 'safe'",
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

const leadOutreachVersion = 29 // 00029_lead_outreach.sql

// leadOutreachTables are the tables 00029 adds.
var leadOutreachTables = []string{"org_person", "contact_mark", "message_edit", "org_sector", "proof", "proof_retire",
	"lead_proposal", "lead_proposal_status", "draft_request"}

// assertLeadOutreachGrants fails the test unless the app role may SELECT and INSERT every
// 00029 table, UPDATE only the listed columns of them, and never DELETE or TRUNCATE.
func assertLeadOutreachGrants(t *testing.T, owner *pgx.Conn, when string) {
	t.Helper()
	for _, table := range leadOutreachTables {
		for _, priv := range []string{"SELECT", "INSERT"} {
			if !appHas(t, owner, table, priv) {
				t.Errorf("%s: leadhound_app lacks %s on %s", when, priv, table)
			}
		}
		for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
			if appHas(t, owner, table, priv) {
				t.Errorf("%s: leadhound_app has %s on %s, want none", when, priv, table)
			}
		}
	}
	for _, c := range [][2]string{{"draft_request", "status"}, {"draft_request", "reason"}, {"draft_request", "batch_id"},
		{"draft_request", "started_at"}, {"draft_request", "finished_at"}, {"outreach_message", "subject"},
		{"outreach_message", "body_text"}, {"outreach_batch", "content_sha256"}} {
		if !appHasColumn(t, owner, c[0], c[1], "UPDATE") {
			t.Errorf("%s: leadhound_app lacks UPDATE on %s.%s", when, c[0], c[1])
		}
	}
	for _, c := range [][2]string{{"draft_request", "lead_id"}, {"draft_request", "channel"}, {"draft_request", "requested_at"},
		{"outreach_batch", "origin"}, {"contact_route", "person_id"}, {"draft_generation", "channel"},
		{"outreach_message", "to_address"}, {"proof", "public"}, {"contact_mark", "note"}, {"message_edit", "body_after"}} {
		if appHasColumn(t, owner, c[0], c[1], "UPDATE") {
			t.Errorf("%s: leadhound_app has UPDATE on %s.%s, want none", when, c[0], c[1])
		}
	}
	for _, table := range []string{"outreach_message", "outreach_batch"} {
		if appHas(t, owner, table, "UPDATE") || appHas(t, owner, table, "DELETE") {
			t.Errorf("%s: leadhound_app has table-wide UPDATE or DELETE on %s", when, table)
		}
	}
}

// TestMigration00029Grants: migrate.go and grants.sql each leave the same privileges, in
// either order.
func TestMigration00029Grants(t *testing.T) {
	leadstoretest.Migrated(t)
	owner := storetest.OwnerConn(t)
	assertLeadOutreachGrants(t, owner, "after migrate")
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatalf("read grants.sql: %v", err)
	}
	if _, err := owner.Exec(context.Background(), string(grants)); err != nil {
		t.Fatalf("run grants.sql: %v", err)
	}
	assertLeadOutreachGrants(t, owner, "after grants.sql")
	migrateAll(t)
	assertLeadOutreachGrants(t, owner, "after grants.sql then migrate")
}

// TestMigration00029DownUp: down succeeds with a row in every new table, a lead on a linkedin
// route and a lead on a route the operator typed, keeps what was found on a page, and restores
// the old CHECKs; up again backfills the LinkedIn pages and the manual batches' origin.
func TestMigration00029DownUp(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := f.owner
	orgOf := func(lead int64) int64 { return ownerInt(t, owner, `SELECT org_id FROM lead WHERE id = $1`, lead) }

	leadA := f.siteLead(t, "harbour-lane.example", nil, 7, "info@harbour-lane.example")
	leadB := f.siteLead(t, "north-quay.example", nil, 7, "")
	leadC := f.siteLead(t, "third-street.example", nil, 7, "info@third-street.example")
	leadD := f.siteLead(t, "fourth-street.example", nil, 7, "info@fourth-street.example")
	orgA, orgB := orgOf(leadA), orgOf(leadB)

	person := ownerInt(t, owner, `INSERT INTO org_person (org_id, name, role, derived_by, fetch_id, actor)
		VALUES ($1, 'Harbour Person', 'Director', 'claude-research@1', $2, 'system') RETURNING id`, orgA, f.fid)
	profile := ownerInt(t, owner, `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by, person_id)
		VALUES ($1, 'linkedin', 'https://linkedin.com/' || 'in/harbour-person', $2, 'claude-research@1', $3) RETURNING id`, orgA, f.fid, person)
	f.ownerRun(t, `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by, person_id)
		VALUES ($1, 'email', 'person@harbour-lane.example', $2, 'claude-research@1', $3)`, orgA, f.fid, person)
	f.ownerRun(t, `UPDATE lead SET channel = 'linkedin', route_id = $2 WHERE id = $1`, leadA, profile)
	typed := ownerInt(t, owner, `INSERT INTO contact_route (org_id, kind, value, derived_by)
		VALUES ($1, 'form', 'https://north-quay.example/contact', 'operator@1') RETURNING id`, orgB)
	f.ownerRun(t, `UPDATE lead SET channel = 'form', route_id = $2 WHERE id = $1`, leadB, typed)
	phone := ownerInt(t, owner, `INSERT INTO org_fact (org_id, kind, value, normalized, derived_by)
		VALUES ($1, 'phone', '+44 20 7946 0000', '+442079460000', 'operator@1') RETURNING id`, orgB)
	f.ownerRun(t, `INSERT INTO contact_mark (route_id, mark, note, actor) VALUES ($1, 'claim', 'Their own form', 'operator')`, typed)
	f.ownerRun(t, `INSERT INTO contact_mark (route_id, mark, replaced_by_route_id, actor) VALUES ($1, 'retire', $2, 'operator')`, profile, typed)
	f.ownerRun(t, `INSERT INTO contact_mark (fact_id, mark, actor) VALUES ($1, 'claim', 'operator')`, phone)
	f.ownerRun(t, `INSERT INTO contact_mark (person_id, mark, actor) VALUES ($1, 'claim', 'operator')`, person)

	batch, _ := f.batch(t, false, leadC)
	msg := ownerInt(t, owner, `SELECT min(id) FROM outreach_message WHERE batch_id = $1`, batch)
	f.ownerRun(t, `INSERT INTO message_edit (message_id, body_before, body_after, actor) VALUES ($1, 'before', 'after', 'operator')`, msg)
	f.ownerRun(t, `INSERT INTO draft_request (lead_id, channel, status, batch_id, started_at, finished_at)
		VALUES ($1, 'form', 'done', $2, now(), now())`, leadC, batch)
	f.ownerRun(t, `INSERT INTO draft_request (lead_id) VALUES ($1)`, leadA)
	f.ownerRun(t, `INSERT INTO org_sector (org_id, sector, source, actor) VALUES ($1, 'education', 'operator', 'operator')`, orgA)
	proof := ownerInt(t, owner, `INSERT INTO proof (account_id, title, line, url, sectors, public, actor)
		VALUES (1, 'A school', 'We moved a school.', 'https://proof.example/school', '{education}', true, 'operator') RETURNING id`)
	f.ownerRun(t, `INSERT INTO proof_retire (proof_id, reason) VALUES ($1, 'old')`, proof)
	proposal := ownerInt(t, owner, `INSERT INTO lead_proposal (lead_id, title, value_cents, currency, actor)
		VALUES ($1, 'The upgrade', 100000, 'EUR', 'operator') RETURNING id`, leadA)
	f.ownerRun(t, `INSERT INTO lead_proposal_status (proposal_id, status, note, actor) VALUES ($1, 'sent', 'by post', 'operator')`, proposal)
	f.ownerRun(t, `INSERT INTO draft_generation (lead_id, campaign_id, attempt, model, fact_sha256, facts, output, check_ok, purpose, channel)
		VALUES ($1, 1, 1, 'm', repeat('a', 64), '{}', '{}', true, 'batch', 'linkedin')`, leadA)
	f.ownerRun(t, `UPDATE outreach_batch SET origin = 'draft_it', manual = true WHERE id = $1`, batch)
	both := ownerInt(t, owner, `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor)
		VALUES ($1, $2, 1, 'cli', '{email,phone,linkedin}', 'system') RETURNING id`, orgA, leadA)
	only := ownerInt(t, owner, `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor)
		VALUES ($1, $2, 1, 'cli', '{linkedin}', 'system') RETURNING id`, orgB, leadB)

	p := leadsProvider(t)
	t.Cleanup(func() { migrateAll(t) })
	plain, _ := f.batch(t, false, leadD) // before the down: today's batch writes origin
	if _, err := p.DownTo(ctx, leadOutreachVersion-1); err != nil {
		t.Fatalf("down to %d: %v", leadOutreachVersion-1, err)
	}

	for _, table := range leadOutreachTables {
		var reg *string
		if err := owner.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, table).Scan(&reg); err != nil || reg != nil {
			t.Errorf("%s after down: %v %v, want gone", table, reg, err)
		}
	}
	for _, col := range [][2]string{{"contact_route", "person_id"}, {"draft_generation", "channel"}, {"outreach_batch", "origin"}} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
			AND table_name = $1 AND column_name = $2`, col[0], col[1]); n != 0 {
			t.Errorf("%s.%s left after down", col[0], col[1])
		}
	}
	for _, lead := range []int64{leadA, leadB} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM lead WHERE id = $1 AND channel = 'none' AND route_id IS NULL`, lead); n != 1 {
			t.Errorf("lead %d after down: not on channel none with no route", lead)
		}
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_channel WHERE name = 'linkedin'`); n != 0 {
		t.Error("lead_channel linkedin left after down")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_route WHERE org_id IN ($1, $2)`, orgA, orgB); n != 2 {
		t.Errorf("routes after down = %d, want the two email routes found on a page", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM org_fact WHERE id = $1`, phone); n != 0 {
		t.Error("the operator's phone fact stayed after down")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_research WHERE id = $1 AND want = '{email,phone}'`, both); n != 1 {
		t.Error("research with linkedin among its wants: want it kept without linkedin")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_research WHERE id = $1`, only); n != 0 {
		t.Error("research that wanted only linkedin stayed after down")
	}
	for q, want := range map[string]string{
		`INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by) VALUES (` + strconv.FormatInt(orgA, 10) + `, 'linkedin', 'https://linkedin.com/company/a', ` + strconv.FormatInt(f.fid, 10) + `, 'x')`: "23514",
		`INSERT INTO contact_route (org_id, kind, value, derived_by) VALUES (` + strconv.FormatInt(orgA, 10) + `, 'form', 'https://a.example/', 'operator@1')`:                                                        "23514",
		`INSERT INTO org_fact (org_id, kind, value, normalized, derived_by) VALUES (` + strconv.FormatInt(orgA, 10) + `, 'phone', '+442079460001', '+442079460001', 'operator@1')`:                                    "23514",
		`UPDATE contact_research SET want = '{email,linkedin}' WHERE id = ` + strconv.FormatInt(both, 10):                                                                                                             "23514",
		`UPDATE lead SET channel = 'linkedin' WHERE id = ` + strconv.FormatInt(leadA, 10):                                                                                                                             "23503",
	} {
		if _, err := owner.Exec(ctx, q); sqlState(err) != want {
			t.Errorf("after down, %s: %v, want SQLSTATE %s", q, err, want)
		}
	}

	// Now the state the backfill meets in production: social facts from a page and from a
	// place, a social fact that is not LinkedIn, and one manual batch beside a plain one.
	imp := ownerInt(t, owner, `INSERT INTO place_import (source, release, campaign_id, lines, kept, blank, malformed, not_cafe,
		low_confidence, other_country, no_name, stored, already, chains, orgs, sites, actor)
		VALUES ('overture', 'r1', 1, 1, 1, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 'system') RETURNING id`)
	place := ownerInt(t, owner, `INSERT INTO place (import_id, source, source_id, release, name, category, confidence, org_id)
		VALUES ($1, 'overture', 'p1', 'r1', 'North Quay', 'cafe', 0.9, $2) RETURNING id`, imp, orgB)
	for _, s := range []struct {
		org   int64
		url   string
		fetch *int64
		place *int64
	}{
		{orgA, "https://linkedin.com/company/harbour-lane", &f.fid, nil},
		{orgA, "https://linkedin.com/school/harbour-lane", &f.fid, nil},
		{orgB, "https://linkedin.com/company/north-quay", nil, &place},
		{orgA, "https://facebook.com/harbour-lane", &f.fid, nil},
		{orgA, "https://linkedin.com.example/company/harbour-lane", &f.fid, nil},
	} {
		f.ownerRun(t, `INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, place_id, derived_by)
			VALUES ($1, 'social', $2, $2, $3, $4, 'enrich@1')`, s.org, s.url, s.fetch, s.place)
	}
	if _, err := p.UpTo(ctx, leadOutreachVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}

	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_channel WHERE name = 'linkedin' AND label = 'LinkedIn'`); n != 1 {
		t.Error("lead_channel linkedin missing after up")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_route WHERE kind = 'linkedin'`); n != 3 {
		t.Errorf("linkedin routes after backfill = %d, want 3 (company and school pages only)", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_route WHERE org_id = $1 AND kind = 'linkedin' AND derived_by = 'enrich@1'
		AND fetch_id = $2 AND place_id IS NULL AND value IN ('https://linkedin.com/company/harbour-lane', 'https://linkedin.com/school/harbour-lane')`, orgA, f.fid); n != 2 {
		t.Errorf("page-read LinkedIn routes = %d, want 2 with the fact's fetch and derived_by", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_route WHERE org_id = $1 AND kind = 'linkedin' AND derived_by = 'enrich@1'
		AND fetch_id IS NULL AND place_id = $2 AND value = 'https://linkedin.com/company/north-quay'`, orgB, place); n != 1 {
		t.Errorf("place-read LinkedIn routes = %d, want 1 with the fact's place", n)
	}
	for id, want := range map[int64]string{batch: "send_now", plain: "batch"} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM outreach_batch WHERE id = $1 AND origin = $2`, id, want); n != 1 {
			t.Errorf("batch %d origin after backfill: want %s", id, want)
		}
	}
	// A second down with the backfilled routes in use, and up again, stays clean.
	route := ownerInt(t, owner, `SELECT id FROM contact_route WHERE value = 'https://linkedin.com/company/harbour-lane'`)
	f.ownerRun(t, `UPDATE lead SET channel = 'linkedin', route_id = $2 WHERE id = $1`, leadA, route)
	if _, err := p.DownTo(ctx, leadOutreachVersion-1); err != nil {
		t.Fatalf("second down: %v", err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM org_fact WHERE normalized = 'https://linkedin.com/company/harbour-lane'`); n != 1 {
		t.Error("down took the social fact with its route")
	}
	if _, err := p.UpTo(ctx, leadOutreachVersion); err != nil {
		t.Fatalf("second up: %v", err)
	}
}

// TestMigration00029Checks: each new CHECK takes the edge that is allowed and refuses a bad row
// written straight to the table; an owner's erase of a route takes its marks.
func TestMigration00029Checks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "harbour-lane.example", nil, 7, "info@harbour-lane.example")
	org := ownerInt(t, f.owner, `SELECT org_id FROM lead WHERE id = $1`, lead)
	route := ownerInt(t, f.owner, `SELECT id FROM contact_route WHERE org_id = $1`, org)
	person := ownerInt(t, f.owner, `INSERT INTO org_person (org_id, role, derived_by, actor) VALUES ($1, 'Director', 'operator@1', 'operator') RETURNING id`, org)
	fact := ownerInt(t, f.owner, `INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, derived_by)
		VALUES ($1, 'phone', '+442079460000', '+442079460000', $2, 'enrich@1') RETURNING id`, org, f.fid)
	f.ownerRun(t, `INSERT INTO draft_request (lead_id) VALUES ($1)`, lead)
	f.batch(t, false, lead)

	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, c := range []struct {
		name, want, sql string
		args            []any
	}{
		// Decision 4: operator@1 alone may have neither a fetch nor a place.
		{"route operator neither", "", `INSERT INTO contact_route (org_id, kind, value, derived_by) VALUES ($1, 'linkedin', 'https://linkedin.com/company/a', 'operator@1')`, []any{org}},
		{"route other neither", "23514", `INSERT INTO contact_route (org_id, kind, value, derived_by) VALUES ($1, 'linkedin', 'https://linkedin.com/company/a', 'contacts@1')`, []any{org}},
		{"fact operator neither", "", `INSERT INTO org_fact (org_id, kind, value, normalized, derived_by) VALUES ($1, 'phone', '+442079460001', '+442079460001', 'operator@1')`, []any{org}},
		{"fact other neither", "23514", `INSERT INTO org_fact (org_id, kind, value, normalized, derived_by) VALUES ($1, 'phone', '+442079460001', '+442079460001', 'enrich@1')`, []any{org}},
		// The person on a route: linkedin or email only.
		{"person on linkedin", "", `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by, person_id) VALUES ($1, 'linkedin', 'https://linkedin.com/' || 'in/a', $2, 'x', $3)`, []any{org, f.fid, person}},
		{"person on email", "", `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by, person_id) VALUES ($1, 'email', 'a@harbour-lane.example', $2, 'x', $3)`, []any{org, f.fid, person}},
		{"person on form", "23514", `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by, person_id) VALUES ($1, 'form', 'https://harbour-lane.example/', $2, 'x', $3)`, []any{org, f.fid, person}},
		{"route kind", "23514", `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by) VALUES ($1, 'pigeon', 'x', $2, 'x')`, []any{org, f.fid}},
		// org_person.
		{"person no fetch", "23514", `INSERT INTO org_person (org_id, role, derived_by, actor) VALUES ($1, 'Owner', 'claude-research@1', 'system')`, []any{org}},
		{"person nothing", "23514", `INSERT INTO org_person (org_id, derived_by, actor) VALUES ($1, 'operator@1', 'operator')`, []any{org}},
		{"person name only", "", `INSERT INTO org_person (org_id, name, derived_by, actor) VALUES ($1, 'A Person', 'operator@1', 'operator')`, []any{org}},
		{"person role long", "23514", `INSERT INTO org_person (org_id, role, derived_by, actor) VALUES ($1, repeat('r', 61), 'operator@1', 'operator')`, []any{org}},
		// contact_mark: one target, claim or retire, a short note.
		{"mark no target", "23514", `INSERT INTO contact_mark (mark, actor) VALUES ('claim', 'operator')`, nil},
		{"mark two targets", "23514", `INSERT INTO contact_mark (route_id, fact_id, mark, actor) VALUES ($1, $2, 'claim', 'operator')`, []any{route, fact}},
		{"mark kind", "23514", `INSERT INTO contact_mark (route_id, mark, actor) VALUES ($1, 'star', 'operator')`, []any{route}},
		{"mark note edge", "", `INSERT INTO contact_mark (person_id, mark, note, actor) VALUES ($1, 'claim', repeat('n', 200), 'operator')`, []any{person}},
		{"mark note long", "23514", `INSERT INTO contact_mark (route_id, mark, note, actor) VALUES ($1, 'claim', repeat('n', 201), 'operator')`, []any{route}},
		{"mark claim replaced", "23514", `INSERT INTO contact_mark (route_id, mark, replaced_by_route_id, actor) VALUES ($1, 'claim', $1, 'operator')`, []any{route}},
		{"mark fact replaced by route", "23514", `INSERT INTO contact_mark (fact_id, mark, replaced_by_route_id, actor) VALUES ($1, 'retire', $2, 'operator')`, []any{fact, route}},
		{"mark actor", "23514", `INSERT INTO contact_mark (route_id, mark, actor) VALUES ($1, 'claim', 'gremy')`, []any{route}},
		// The channels and origins.
		{"generation channel", "23514", `INSERT INTO draft_generation (lead_id, campaign_id, attempt, model, fact_sha256, facts, output, check_ok, purpose, channel)
			VALUES ($1, 1, 1, 'm', repeat('a', 64), '{}', '{}', true, 'batch', 'phone')`, []any{lead}},
		{"batch origin", "23514", `UPDATE outreach_batch SET origin = 'other'`, nil},
		// org_sector and proof: the closed list.
		{"sector edge", "", `INSERT INTO org_sector (org_id, sector, source, actor) VALUES ($1, 'hospitality', 'rule', 'system')`, []any{org}},
		{"sector other word", "23514", `INSERT INTO org_sector (org_id, sector, source, actor) VALUES ($1, 'mining', 'rule', 'system')`, []any{org}},
		{"sector source", "23514", `INSERT INTO org_sector (org_id, sector, source, actor) VALUES ($1, 'media', 'guess', 'system')`, []any{org}},
		{"proof edge", "", `INSERT INTO proof (account_id, title, line, sectors, public, actor) VALUES (1, repeat('t', 80), repeat('l', 240), '{education,other}', false, 'operator')`, nil},
		{"proof no sectors", "23514", `INSERT INTO proof (account_id, title, line, sectors, public, actor) VALUES (1, 't', 'l', '{}', false, 'operator')`, nil},
		{"proof bad sector", "23514", `INSERT INTO proof (account_id, title, line, sectors, public, actor) VALUES (1, 't', 'l', '{education,mining}', false, 'operator')`, nil},
		{"proof null sector", "23514", `INSERT INTO proof (account_id, title, line, sectors, public, actor) VALUES (1, 't', 'l', '{education,NULL}', false, 'operator')`, nil},
		{"proof title long", "23514", `INSERT INTO proof (account_id, title, line, sectors, public, actor) VALUES (1, repeat('t', 81), 'l', '{other}', false, 'operator')`, nil},
		{"proof line long", "23514", `INSERT INTO proof (account_id, title, line, sectors, public, actor) VALUES (1, 't', repeat('l', 241), '{other}', false, 'operator')`, nil},
		{"proof http", "23514", `INSERT INTO proof (account_id, title, line, url, sectors, public, actor) VALUES (1, 't', 'l', 'http://proof.example/', '{other}', false, 'operator')`, nil},
		// lead_proposal and its status.
		{"proposal edge", "", `INSERT INTO lead_proposal (lead_id, title, url, value_cents, currency, actor) VALUES ($1, repeat('t', 120), 'https://proposal.example/1', 0, 'RON', 'operator')`, []any{lead}},
		{"proposal title long", "23514", `INSERT INTO lead_proposal (lead_id, title, actor) VALUES ($1, repeat('t', 121), 'operator')`, []any{lead}},
		{"proposal negative", "23514", `INSERT INTO lead_proposal (lead_id, title, value_cents, actor) VALUES ($1, 't', -1, 'operator')`, []any{lead}},
		{"proposal currency", "23514", `INSERT INTO lead_proposal (lead_id, title, currency, actor) VALUES ($1, 't', 'JPY', 'operator')`, []any{lead}},
		{"proposal http", "23514", `INSERT INTO lead_proposal (lead_id, title, url, actor) VALUES ($1, 't', 'http://proposal.example/', 'operator')`, []any{lead}},
		// draft_request: one open per lead, the times follow the status.
		{"second open request", "23505", `INSERT INTO draft_request (lead_id) VALUES ($1)`, []any{lead}},
		{"second finished request", "", `INSERT INTO draft_request (lead_id, status, reason, started_at, finished_at) VALUES ($1, 'failed', repeat('r', 200), now(), now())`, []any{lead}},
		{"request reason long", "23514", `INSERT INTO draft_request (lead_id, status, reason, started_at, finished_at) VALUES ($1, 'failed', repeat('r', 201), now(), now())`, []any{lead}},
		{"request channel", "23514", `INSERT INTO draft_request (lead_id, status, channel, started_at, finished_at) VALUES ($1, 'done', 'phone', now(), now())`, []any{lead}},
		{"request status", "23514", `INSERT INTO draft_request (lead_id, status) VALUES ($1, 'other')`, []any{lead}},
		{"request times", "23514", `INSERT INTO draft_request (lead_id, status) VALUES ($1, 'done')`, []any{lead}},
		// contact_research.want.
		{"want three", "", `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor) VALUES ($1, $2, 1, 'cli', '{email,phone,linkedin}', 'system')`, []any{org, lead}},
		{"want other", "23514", `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor) VALUES ($1, $2, 1, 'cli', '{fax}', 'system')`, []any{org, lead}},
		{"want four", "23514", `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor) VALUES ($1, $2, 1, 'cli', '{email,phone,linkedin,email}', 'system')`, []any{org, lead}},
		{"want none", "23514", `INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor) VALUES ($1, $2, 1, 'cli', '{}', 'system')`, []any{org, lead}},
	} {
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, c.sql, c.args...)
		if got := sqlState(err); got != c.want || (c.want == "" && err != nil) {
			t.Errorf("%s: %v (SQLSTATE %q), want %q", c.name, err, got, c.want)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
	}

	// An erase by the owner: deleting a route, fact or person takes its marks, and a retire
	// that named a deleted route as its replacement keeps its own place with no replacement.
	other := ownerTxInt(t, tx, `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by) VALUES ($1, 'form', 'https://harbour-lane.example/c', $2, 'x') RETURNING id`, org, f.fid)
	retire := ownerTxInt(t, tx, `INSERT INTO contact_mark (route_id, mark, replaced_by_route_id, actor) VALUES ($1, 'retire', $2, 'operator') RETURNING id`, route, other)
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`INSERT INTO contact_mark (route_id, mark, actor) VALUES ($1, 'claim', 'operator')`, other},
		{`INSERT INTO contact_mark (fact_id, mark, actor) VALUES ($1, 'retire', 'operator')`, fact},
		{`INSERT INTO contact_mark (person_id, mark, actor) VALUES ($1, 'retire', 'operator')`, person},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.arg); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []struct {
		sql string
		arg int64
	}{
		{`DELETE FROM contact_route WHERE id = $1`, other},
		{`DELETE FROM org_fact WHERE id = $1`, fact},
		{`DELETE FROM org_person WHERE id = $1`, person},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.arg); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	if n := ownerTxInt(t, tx, `SELECT count(*) FROM contact_mark WHERE id = $1 AND replaced_by_route_id IS NULL`, retire); n != 1 {
		t.Error("a retire whose replacement was erased: want it kept with no replacement")
	}
	if n := ownerTxInt(t, tx, `SELECT count(*) FROM contact_mark`); n != 1 {
		t.Errorf("marks after erase = %d, want only the retire on the kept route", n)
	}
}

// ownerTxInt is ownerInt inside a transaction.
func ownerTxInt(t *testing.T, tx pgx.Tx, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := tx.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

const instagramVersion = 31 // 00031_instagram.sql

// instagramTables are the tables 00031 adds.
var instagramTables = []string{"ig_profile", "ig_follow", "ig_match", "ig_judgment", "ig_signal_hint", "ig_review_status"}

// igURL builds an Instagram profile address in pieces; the handles are invented.
func igURL(h string) string { return "https://instagram.com/" + h }

// TestMigration00031DownUp: down refuses while an Instagram generation or draft request exists;
// then, with a row in every new table and a lead on an Instagram route, it moves the lead to no
// channel, takes every instagram route, restores the old CHECKs and keeps the social facts; up
// again backfills a route from every Instagram profile fact, in the canonical form.
func TestMigration00031DownUp(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := f.owner
	leadA := f.siteLead(t, "harbour-lane.example", nil, 7, "info@harbour-lane.example")
	leadB := f.siteLead(t, "north-quay.example", nil, 7, "")
	orgA := ownerInt(t, owner, `SELECT org_id FROM lead WHERE id = $1`, leadA)
	orgB := ownerInt(t, owner, `SELECT org_id FROM lead WHERE id = $1`, leadB)

	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgA, Kind: leadstore.RouteInstagram, Value: "@Harbour_Example",
		DerivedBy: leadstore.DerivedByInstagram}); err != nil {
		t.Fatalf("import route: %v", err)
	}
	route := ownerInt(t, owner, `SELECT id FROM contact_route WHERE org_id = $1 AND kind = 'instagram'`, orgA)
	f.ownerRun(t, `UPDATE lead SET channel = 'instagram', route_id = $2 WHERE id = $1`, leadA, route)
	f.ownerRun(t, `INSERT INTO contact_mark (route_id, mark, actor) VALUES ($1, 'claim', 'operator')`, route)
	profile := ownerInt(t, owner, `INSERT INTO ig_profile (handle, source, capture_sha256, captured_at)
		VALUES ('harbour_example', 'lead', sha256('a'), now()) RETURNING id`)
	f.ownerRun(t, `INSERT INTO ig_follow (list_owner, handle, captured_at, capture_sha256) VALUES ('vendor.example', 'harbour_example', now(), sha256('b'))`)
	f.ownerRun(t, `INSERT INTO ig_match (handle, lead_id, tier, actor) VALUES ('harbour_example', $1, 'handle', 'system')`, leadA)
	f.ownerRun(t, `INSERT INTO ig_judgment (profile_id, question, choice, source) VALUES ($1, 'ig_place', 'unclear', 'rule')`, profile)
	f.ownerRun(t, `INSERT INTO ig_signal_hint (handle, kind, value, captured_at) VALUES ('harbour_example', 'followed_by_vendor', 'stampino', now())`)
	f.ownerRun(t, `INSERT INTO ig_review_status (handle, status, actor) VALUES ('harbour_example', 'new', 'system')`)

	p := leadsProvider(t)
	t.Cleanup(func() { migrateAll(t) })

	// An Instagram generation, then an Instagram draft request, each stops the down whole.
	for _, row := range []struct{ insert, table string }{
		{`INSERT INTO draft_generation (lead_id, campaign_id, attempt, model, fact_sha256, facts, output, check_ok, purpose, channel)
			VALUES ($1, 1, 1, 'm', repeat('a', 64), '{}', '{}', true, 'batch', 'instagram')`, "draft_generation"},
		{`INSERT INTO draft_request (lead_id, channel) VALUES ($1, 'instagram')`, "draft_request"},
	} {
		f.ownerRun(t, row.insert, leadA)
		_, err := p.DownTo(ctx, instagramVersion-1)
		if err == nil || !strings.Contains(err.Error(), "erase or move them first") {
			t.Fatalf("down with an Instagram %s: %v, want refused", row.table, err)
		}
		if n := ownerInt(t, owner, `SELECT count(*) FROM goose_db_version_leads WHERE version_id = $1`, instagramVersion); n != 1 {
			t.Fatalf("a refused down moved the version (%d rows at %d)", n, instagramVersion)
		}
		if n := ownerInt(t, owner, `SELECT count(*) FROM lead WHERE id = $1 AND channel = 'instagram'`, leadA); n != 1 {
			t.Fatal("a refused down moved the lead")
		}
		f.ownerRun(t, `DELETE FROM `+row.table+` WHERE channel = 'instagram'`)
	}

	if _, err := p.DownTo(ctx, instagramVersion-1); err != nil {
		t.Fatalf("down to %d: %v", instagramVersion-1, err)
	}
	for _, table := range instagramTables {
		var reg *string
		if err := owner.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, table).Scan(&reg); err != nil || reg != nil {
			t.Errorf("%s after down: %v %v, want gone", table, reg, err)
		}
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead WHERE id = $1 AND channel = 'none' AND route_id IS NULL`, leadA); n != 1 {
		t.Error("the Instagram lead after down: not on channel none with no route")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_route WHERE kind = 'instagram'`); n != 0 {
		t.Errorf("instagram routes after down = %d, want 0", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_route WHERE org_id = $1`, orgA); n != 1 {
		t.Errorf("routes of the org after down = %d, want its email route only", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_channel WHERE name = 'instagram'`); n != 0 {
		t.Error("lead_channel instagram left after down")
	}
	org := strconv.FormatInt(orgA, 10)
	fid := strconv.FormatInt(f.fid, 10)
	lead := strconv.FormatInt(leadA, 10)
	for q, want := range map[string]string{
		`INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by) VALUES (` + org + `, 'instagram', 'x', ` + fid + `, 'x')`: "23514",
		`INSERT INTO contact_route (org_id, kind, value, derived_by) VALUES (` + org + `, 'form', 'https://a.example/', 'instagram@1')`:  "23514",
		`INSERT INTO draft_request (lead_id, channel) VALUES (` + lead + `, 'instagram')`:                                                "23514",
		`UPDATE lead SET channel = 'instagram' WHERE id = ` + lead:                                                                       "23503",
	} {
		if _, err := owner.Exec(ctx, q); sqlState(err) != want {
			t.Errorf("after down, %s: %v, want SQLSTATE %s", q, err, want)
		}
	}
	for _, c := range [][2]string{
		{"contact_route_kind_check", "'linkedin'::text]"},
		{"draft_generation_channel_check", "'linkedin'::text]"},
		{"draft_request_channel_check", "'linkedin'::text]"},
		{"outreach_message_channel_check", "'phone'::text]"},
	} {
		def := ownerString(t, owner, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1", c[0])
		if strings.Contains(def, "instagram") || !strings.Contains(def, c[1]) {
			t.Errorf("%s after down = %s, want the 00030 list", c[0], def)
		}
	}

	// The state the backfill meets: Instagram profiles read from a page and from a place, a
	// profile under a sub-page, the same profile twice, Instagram's own pages, and others.
	imp := ownerInt(t, owner, `INSERT INTO place_import (source, release, campaign_id, lines, kept, blank, malformed, not_cafe,
		low_confidence, other_country, no_name, stored, already, chains, orgs, sites, actor)
		VALUES ('overture', 'r1', 1, 1, 1, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 'system') RETURNING id`)
	place := ownerInt(t, owner, `INSERT INTO place (import_id, source, source_id, release, name, category, confidence, org_id)
		VALUES ($1, 'overture', 'p1', 'r1', 'North Quay', 'cafe', 0.9, $2) RETURNING id`, imp, orgB)
	for _, s := range []struct {
		org   int64
		url   string
		fetch *int64
		place *int64
	}{
		{orgA, igURL("harbour_example"), &f.fid, nil},
		{orgA, igURL("harbour.lane.example/reels"), &f.fid, nil},
		{orgA, igURL("harbour_example/tagged"), &f.fid, nil}, // the same profile again
		{orgB, igURL("north.quay.example"), nil, &place},
		{orgA, igURL("p/abc123"), &f.fid, nil},
		{orgA, igURL("explore/tags/cafea"), &f.fid, nil},
		{orgA, igURL("a1234567890123456789012345678901example"), &f.fid, nil}, // too long for a handle
		{orgA, "https://facebook.com/harbour-lane", &f.fid, nil},
		{orgA, "https://instagram.com.example/harbour_example", &f.fid, nil},
	} {
		f.ownerRun(t, `INSERT INTO org_fact (org_id, kind, value, normalized, fetch_id, place_id, derived_by)
			VALUES ($1, 'social', $2, $2, $3, $4, 'enrich@1')`, s.org, s.url, s.fetch, s.place)
	}
	if _, err := p.UpTo(ctx, instagramVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}

	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_channel WHERE name = 'instagram' AND label = 'Instagram'`); n != 1 {
		t.Error("lead_channel instagram missing after up")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM contact_route WHERE kind = 'instagram'`); n != 3 {
		t.Errorf("instagram routes after backfill = %d, want 3 (one per profile per org)", n)
	}
	for _, r := range []struct {
		org   int64
		fact  string
		fetch bool
	}{
		{orgA, igURL("harbour_example"), true},
		{orgA, igURL("harbour.lane.example/reels"), true},
		{orgB, igURL("north.quay.example"), false},
	} {
		h, ok := enrich.InstagramHandle(r.fact)
		if !ok {
			t.Fatalf("%s is not a profile to enrich", r.fact)
		}
		// The SQL backfill and enrich.InstagramURL agree on the one canonical value.
		q := `SELECT count(*) FROM contact_route WHERE org_id = $1 AND kind = 'instagram' AND value = $2 AND derived_by = 'enrich@1'
			AND fetch_id IS NOT DISTINCT FROM $3 AND place_id IS NOT DISTINCT FROM $4`
		var fetch, plc *int64
		if r.fetch {
			fetch = &f.fid
		} else {
			plc = &place
		}
		if n := ownerInt(t, owner, q, r.org, enrich.InstagramURL(h), fetch, plc); n != 1 {
			t.Errorf("backfilled route %s of %s = %d, want 1 with the fact's own fetch or place", enrich.InstagramURL(h), r.fact, n)
		}
	}
	// A second down with a backfilled route in use, and up again, stays clean; the facts stay.
	backfilled := ownerInt(t, owner, `SELECT id FROM contact_route WHERE org_id = $1 AND kind = 'instagram' ORDER BY id LIMIT 1`, orgA)
	f.ownerRun(t, `UPDATE lead SET channel = 'instagram', route_id = $2 WHERE id = $1`, leadA, backfilled)
	if _, err := p.DownTo(ctx, instagramVersion-1); err != nil {
		t.Fatalf("second down: %v", err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM org_fact WHERE kind = 'social' AND normalized LIKE 'https://instagram.com/%'`); n != 7 {
		t.Errorf("Instagram facts after second down = %d, want all 7 kept", n)
	}
	if _, err := p.UpTo(ctx, instagramVersion); err != nil {
		t.Fatalf("second up: %v", err)
	}
}

// ownerString is ownerInt for a text result.
func ownerString(t *testing.T, owner *pgx.Conn, q string, args ...any) string {
	t.Helper()
	var s string
	if err := owner.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s
}

// TestMigration00031Checks: each new CHECK takes the edge that is allowed and refuses a bad row
// written straight to the table.
func TestMigration00031Checks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "harbour-lane.example", nil, 7, "info@harbour-lane.example")
	other := f.siteLead(t, "north-quay.example", nil, 7, "")
	org := ownerInt(t, f.owner, `SELECT org_id FROM lead WHERE id = $1`, lead)
	profile := ownerInt(t, f.owner, `INSERT INTO ig_profile (handle, source, capture_sha256, captured_at)
		VALUES ('cafe.exemplu', 'lead', sha256('p'), now()) RETURNING id`)
	f.ownerRun(t, `INSERT INTO ig_follow (list_owner, handle, captured_at, capture_sha256) VALUES ('vendor.example', 'cafe.exemplu', now(), sha256('f'))`)
	f.ownerRun(t, `INSERT INTO ig_signal_hint (handle, kind, value, captured_at) VALUES ('cafe.exemplu', 'followed_by_vendor', 'stampino', now())`)

	for _, c := range [][2]string{
		{"contact_route_kind_check", "'instagram'"},
		{"contact_route_source_check", "'instagram@1'"},
		{"draft_generation_channel_check", "'instagram'"},
		{"draft_request_channel_check", "'instagram'"},
		{"outreach_message_channel_check", "'instagram'"},
		{"contact_route_person_check", "'linkedin'"},
		{"contact_research_want_check", "'linkedin'"},
	} {
		def := ownerString(t, f.owner, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1", c[0])
		if !strings.Contains(def, c[1]) {
			t.Errorf("constraint %s = %s, want it to name %s", c[0], def, c[1])
		}
	}
	// Decision 9: an Instagram route is never a person's, and research never hunts Instagram.
	for _, name := range []string{"contact_route_person_check", "contact_research_want_check"} {
		if def := ownerString(t, f.owner, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1", name); strings.Contains(def, "instagram") {
			t.Errorf("constraint %s = %s, want no instagram", name, def)
		}
	}

	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sha := func(s string) string { return "sha256('" + s + "')" }
	profileRow := func(cols, vals string) string {
		return `INSERT INTO ig_profile (handle, source, capture_sha256, captured_at` + cols + `) VALUES (` + vals + `)`
	}
	for _, c := range []struct {
		name, want, sql string
		args            []any
	}{
		// The route and the channels.
		{"import route neither", "", `INSERT INTO contact_route (org_id, kind, value, derived_by) VALUES ($1, 'instagram', 'https://www.instagram.com/cafe.exemplu/', 'instagram@1')`, []any{org}},
		{"import route on a form", "23514", `INSERT INTO contact_route (org_id, kind, value, derived_by) VALUES ($1, 'form', 'https://harbour-lane.example/c', 'instagram@1')`, []any{org}},
		{"other instagram route neither", "23514", `INSERT INTO contact_route (org_id, kind, value, derived_by) VALUES ($1, 'instagram', 'https://www.instagram.com/cafe.exemplu/', 'enrich@1')`, []any{org}},
		{"import route with a fetch", "", `INSERT INTO contact_route (org_id, kind, value, fetch_id, place_id, derived_by)
			VALUES ($1, 'instagram', 'https://www.instagram.com/cafe.exemplu/', $2, NULL, 'instagram@1')`, []any{org, f.fid}},
		{"instagram route on a page", "", `INSERT INTO contact_route (org_id, kind, value, fetch_id, derived_by) VALUES ($1, 'instagram', 'https://www.instagram.com/bean.example/', $2, 'enrich@1')`, []any{org, f.fid}},
		{"lead on instagram", "", `UPDATE lead SET channel = 'instagram' WHERE id = $1`, []any{lead}},
		{"request on instagram", "", `INSERT INTO draft_request (lead_id, channel) VALUES ($1, 'instagram')`, []any{lead}},
		{"generation on instagram", "", `INSERT INTO draft_generation (lead_id, campaign_id, attempt, model, fact_sha256, facts, output, check_ok, purpose, channel)
			VALUES ($1, 1, 1, 'm', repeat('a', 64), '{}', '{}', true, 'batch', 'instagram')`, []any{lead}},
		// ig_profile.
		{"profile edge", "", profileRow(`, outcome, ig_user_id, full_name, category, bio, external_url, followers, following, posts, is_private, captions`,
			`'a_b.c', 'lead', `+sha("e")+`, now(), 'ok', '123', repeat('n', 150), repeat('c', 100), repeat('b', 600), 'https://cafe.example/', 0, 0, 0, false, jsonb_build_array('x','x','x','x','x','x','x','x','x','x','x','x')`), nil},
		{"profile missing", "", profileRow(`, outcome`, `'gone.example', 'lead', `+sha("m")+`, now(), 'missing'`), nil},
		{"profile from a list", "", profileRow(`, source_of, source_lead_id`, `'list.example', 'following', `+sha("l")+`, now(), 'vendor.example', 999999`), nil},
		{"profile same capture", "23505", profileRow(``, `'cafe.exemplu', 'lead', `+sha("p")+`, now()`), nil},
		{"profile same handle new capture", "", profileRow(``, `'cafe.exemplu', 'lead', `+sha("p2")+`, now()`), nil},
		{"profile upper case", "23514", profileRow(``, `'Cafe.Exemplu', 'lead', `+sha("u")+`, now()`), nil},
		{"profile at", "23514", profileRow(``, `'@cafe.exemplu', 'lead', `+sha("u")+`, now()`), nil},
		{"profile long handle", "23514", profileRow(``, `repeat('a', 31), 'lead', `+sha("u")+`, now()`), nil},
		{"profile outcome", "23514", profileRow(`, outcome`, `'a.example', 'lead', `+sha("u")+`, now(), 'private'`), nil},
		{"profile user id", "23514", profileRow(`, ig_user_id`, `'a.example', 'lead', `+sha("u")+`, now(), 'abc'`), nil},
		{"profile bio long", "23514", profileRow(`, bio`, `'a.example', 'lead', `+sha("u")+`, now(), repeat('b', 601)`), nil},
		{"profile name line", "23514", profileRow(`, full_name`, `'a.example', 'lead', `+sha("u")+`, now(), E'a\nb'`), nil},
		{"profile negative", "23514", profileRow(`, followers`, `'a.example', 'lead', `+sha("u")+`, now(), -1`), nil},
		{"profile 13 captions", "23514", profileRow(`, captions`, `'a.example', 'lead', `+sha("u")+`, now(), (SELECT jsonb_agg('x'::text) FROM generate_series(1, 13))`), nil},
		{"profile caption not text", "23514", profileRow(`, captions`, `'a.example', 'lead', `+sha("u")+`, now(), '["x", 1]'`), nil},
		{"profile captions object", "23514", profileRow(`, captions`, `'a.example', 'lead', `+sha("u")+`, now(), '{"a": "x"}'`), nil},
		{"profile source", "23514", profileRow(``, `'a.example', 'ad', `+sha("u")+`, now()`), nil},
		{"profile list without owner", "23514", profileRow(``, `'a.example', 'following', `+sha("u")+`, now()`), nil},
		{"profile owner without list", "23514", profileRow(`, source_of`, `'a.example', 'lead', `+sha("u")+`, now(), 'vendor.example'`), nil},
		{"profile short hash", "23514", profileRow(``, `'a.example', 'lead', '\x00'::bytea, now()`), nil},
		// ig_follow.
		{"follow same capture", "23505", `INSERT INTO ig_follow (list_owner, handle, captured_at, capture_sha256) VALUES ('vendor.example', 'cafe.exemplu', now(), sha256('f'))`, nil},
		{"follow bad handle", "23514", `INSERT INTO ig_follow (list_owner, handle, captured_at, capture_sha256) VALUES ('vendor.example', 'Cafe', now(), sha256('g'))`, nil},
		{"follow name", "", `INSERT INTO ig_follow (list_owner, handle, full_name, captured_at, capture_sha256) VALUES ('vendor.example', 'b.example', repeat('n', 150), now(), sha256('g'))`, nil},
		{"follow name long", "23514", `INSERT INTO ig_follow (list_owner, handle, full_name, captured_at, capture_sha256) VALUES ('vendor.example', 'c.example', repeat('n', 151), now(), sha256('g'))`, nil},
		// ig_match: one lead, several candidates, or none.
		{"match strong", "", `INSERT INTO ig_match (handle, lead_id, tier, detail, actor) VALUES ('cafe.exemplu', $1, 'domain', 'cafe.example', 'system')`, []any{lead}},
		{"match weak", "", `INSERT INTO ig_match (handle, lead_id, tier, actor) VALUES ('cafe.exemplu', $1, 'name', 'system')`, []any{lead}},
		{"match tie", "", `INSERT INTO ig_match (handle, tier, candidate_ids, actor) VALUES ('cafe.exemplu', 'name', ARRAY[$1, $2]::bigint[], 'system')`, []any{lead, other}},
		{"match none", "", `INSERT INTO ig_match (handle, tier, actor) VALUES ('cafe.exemplu', 'none', 'system')`, nil},
		{"match operator", "", `INSERT INTO ig_match (handle, lead_id, tier, actor) VALUES ('cafe.exemplu', $1, 'operator', 'gremy')`, []any{lead}},
		{"match unmatch", "", `INSERT INTO ig_match (handle, tier, actor) VALUES ('cafe.exemplu', 'operator', 'gremy')`, nil},
		{"match operator by system", "23514", `INSERT INTO ig_match (handle, lead_id, tier, actor) VALUES ('cafe.exemplu', $1, 'operator', 'system')`, []any{lead}},
		{"match none with a lead", "23514", `INSERT INTO ig_match (handle, lead_id, tier, actor) VALUES ('cafe.exemplu', $1, 'none', 'system')`, []any{lead}},
		{"match none with candidates", "23514", `INSERT INTO ig_match (handle, tier, candidate_ids, actor) VALUES ('cafe.exemplu', 'none', ARRAY[$1, $2]::bigint[], 'system')`, []any{lead, other}},
		{"match one candidate", "23514", `INSERT INTO ig_match (handle, tier, candidate_ids, actor) VALUES ('cafe.exemplu', 'name', ARRAY[$1]::bigint[], 'system')`, []any{lead}},
		{"match no lead", "23514", `INSERT INTO ig_match (handle, tier, actor) VALUES ('cafe.exemplu', 'handle', 'system')`, nil},
		{"match lead and candidates", "23514", `INSERT INTO ig_match (handle, lead_id, tier, candidate_ids, actor) VALUES ('cafe.exemplu', $1, 'name', ARRAY[$1, $2]::bigint[], 'system')`, []any{lead, other}},
		{"match null candidate", "23514", `INSERT INTO ig_match (handle, tier, candidate_ids, actor) VALUES ('cafe.exemplu', 'name', ARRAY[$1, NULL]::bigint[], 'system')`, []any{lead}},
		{"match tier", "23514", `INSERT INTO ig_match (handle, lead_id, tier, actor) VALUES ('cafe.exemplu', $1, 'guess', 'system')`, []any{lead}},
		{"match actor", "23514", `INSERT INTO ig_match (handle, tier, actor) VALUES ('cafe.exemplu', 'none', 'operator')`, nil},
		{"match unknown lead", "23503", `INSERT INTO ig_match (handle, lead_id, tier, actor) VALUES ('cafe.exemplu', 999999999, 'handle', 'system')`, nil},
		// ig_judgment.
		{"judgment jev", "", `INSERT INTO ig_judgment (profile_id, question, choice, raw_choice, confidence, source, model, input_sha256)
			VALUES ($1, 'ig_place', 'unclear', 'cafe_seating', 0.4, 'jev', 'm', sha256('i'))`, []any{profile}},
		{"judgment rule", "", `INSERT INTO ig_judgment (profile_id, question, choice, source, detail) VALUES ($1, 'ig_vendor', 'stampino', 'rule', 'link')`, []any{profile}},
		{"judgment jev no hash", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, raw_choice, source) VALUES ($1, 'ig_place', 'unclear', 'unclear', 'jev')`, []any{profile}},
		{"judgment jev no raw choice", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, source, input_sha256) VALUES ($1, 'ig_place', 'unclear', 'jev', sha256('i'))`, []any{profile}},
		{"judgment rule hash", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, source, input_sha256) VALUES ($1, 'ig_place', 'unclear', 'rule', sha256('i'))`, []any{profile}},
		{"judgment rule confidence", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, confidence, source) VALUES ($1, 'ig_place', 'unclear', 0.5, 'rule')`, []any{profile}},
		{"judgment confidence", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, raw_choice, confidence, source, input_sha256) VALUES ($1, 'ig_place', 'unclear', 'unclear', 1.5, 'jev', sha256('i'))`, []any{profile}},
		{"judgment question", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, source) VALUES ($1, 'ig_mood', 'unclear', 'rule')`, []any{profile}},
		{"judgment choice", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, source) VALUES ($1, 'ig_place', 'Seating!', 'rule')`, []any{profile}},
		{"judgment source", "23514", `INSERT INTO ig_judgment (profile_id, question, choice, source) VALUES ($1, 'ig_place', 'unclear', 'operator')`, []any{profile}},
		{"judgment profile", "23503", `INSERT INTO ig_judgment (profile_id, question, choice, source) VALUES (999999999, 'ig_place', 'unclear', 'rule')`, nil},
		// ig_signal_hint and ig_review_status.
		{"hint twice", "23505", `INSERT INTO ig_signal_hint (handle, kind, value, captured_at) VALUES ('cafe.exemplu', 'followed_by_vendor', 'stampino', now())`, nil},
		{"hint kind", "23514", `INSERT INTO ig_signal_hint (handle, kind, value, captured_at) VALUES ('cafe.exemplu', 'runs_scheme', 'stampino', now())`, nil},
		{"review edge", "", `INSERT INTO ig_review_status (handle, status, note, actor) VALUES ('cafe.exemplu', 'wanted', repeat('n', 200), 'gremy')`, nil},
		{"review status", "23514", `INSERT INTO ig_review_status (handle, status, actor) VALUES ('cafe.exemplu', 'maybe', 'gremy')`, nil},
		{"review note long", "23514", `INSERT INTO ig_review_status (handle, status, note, actor) VALUES ('cafe.exemplu', 'new', repeat('n', 201), 'gremy')`, nil},
		{"review actor", "23514", `INSERT INTO ig_review_status (handle, status, actor) VALUES ('cafe.exemplu', 'new', 'operator')`, nil},
	} {
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, c.sql, c.args...)
		if got := sqlState(err); got != c.want || (c.want == "" && err != nil) {
			t.Errorf("%s: %v (SQLSTATE %q), want %q", c.name, err, got, c.want)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
	}
}

const loyaltyIntentVersion = 33 // 00033_loyalty_intent.sql

// TestMigration00033DownUp: down refuses while a followers row, a vendor touch or an intent call
// exists, and moves nothing; with none (a following row and a derived class only), down drops the
// three tables, restores the old capture key and drops the direction column; up again brings them
// back with the app role's grants.
func TestMigration00033DownUp(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := f.owner
	lead := f.siteLead(t, "intent-down-up.example", nil, 7, "")
	f.ownerRun(t, `INSERT INTO ig_follow (list_owner, handle, captured_at, capture_sha256) VALUES ('vendor.example', 'cafe.exemplu', now(), sha256('f'))`)
	f.ownerRun(t, `INSERT INTO lead_intent (lead_id, class, overlap, score, reason) VALUES ($1, 'curious', 1, 1, 'follows one vendor')`, lead)

	p := leadsProvider(t)
	t.Cleanup(func() { migrateAll(t) })

	for _, row := range []struct{ insert, table, erase string }{
		{`INSERT INTO ig_follow (list_owner, direction, handle, captured_at, capture_sha256)
			VALUES ('vendor.example', 'followers', 'cafe.exemplu', now(), sha256('f'))`, "ig_follow followers",
			`DELETE FROM ig_follow WHERE direction = 'followers'`},
		{`INSERT INTO ig_vendor_touch (vendor_handle, handle, kind, ref, post_code, at, capture_sha256, captured_at)
			VALUES ('vendor.example', 'cafe.exemplu', 'tagged', 'Cabc123', 'Cabc123', now(), sha256('t'), now())`, "ig_vendor_touch",
			`DELETE FROM ig_vendor_touch`},
		{`INSERT INTO lead_intent_call (lead_id, class, actor) VALUES (` + strconv.FormatInt(lead, 10) + `, 'out', 'gremy')`, "lead_intent_call",
			`DELETE FROM lead_intent_call`},
	} {
		f.ownerRun(t, row.insert)
		_, err := p.DownTo(ctx, loyaltyIntentVersion-1)
		if err == nil || !strings.Contains(err.Error(), "erase or move them first") {
			t.Fatalf("down with a %s row: %v, want refused", row.table, err)
		}
		if n := ownerInt(t, owner, `SELECT count(*) FROM goose_db_version_leads WHERE version_id = $1`, loyaltyIntentVersion); n != 1 {
			t.Fatalf("a refused down moved the version (%d rows at %d)", n, loyaltyIntentVersion)
		}
		if n := ownerInt(t, owner, `SELECT count(*) FROM lead_intent`); n != 1 {
			t.Fatal("a refused down moved the derived class")
		}
		f.ownerRun(t, row.erase)
	}

	if _, err := p.DownTo(ctx, loyaltyIntentVersion-1); err != nil {
		t.Fatalf("down to %d: %v", loyaltyIntentVersion-1, err)
	}
	for _, table := range []string{"ig_vendor_touch", "lead_intent", "lead_intent_call"} {
		var reg *string
		if err := owner.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, table).Scan(&reg); err != nil || reg != nil {
			t.Errorf("%s after down: %v %v, want gone", table, reg, err)
		}
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'ig_follow' AND column_name = 'direction'`); n != 0 {
		t.Error("ig_follow.direction left after down")
	}
	if def := ownerString(t, owner, "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'ig_follow_capture_key'"); def != "UNIQUE (list_owner, handle, capture_sha256)" {
		t.Errorf("ig_follow_capture_key after down = %s, want the 00031 key", def)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM ig_follow`); n != 1 {
		t.Errorf("ig_follow rows after down = %d, want the following row kept", n)
	}

	migrateAll(t)
	assertLeadPrivileges(t, owner, "after 00033 down and up")
	if n := ownerInt(t, owner, `SELECT count(*) FROM ig_follow WHERE direction = 'following'`); n != 1 {
		t.Errorf("following rows after up = %d, want 1 (the default)", n)
	}
	// The same account on both lists of one owner in one capture is two rows under the new key.
	f.ownerRun(t, `INSERT INTO ig_follow (list_owner, direction, handle, captured_at, capture_sha256)
		VALUES ('vendor.example', 'followers', 'cafe.exemplu', now(), sha256('f'))`)
	if _, err := owner.Exec(ctx, `INSERT INTO ig_follow (list_owner, direction, handle, captured_at, capture_sha256)
		VALUES ('vendor.example', 'followers', 'cafe.exemplu', now(), sha256('f'))`); sqlState(err) != "23505" {
		t.Errorf("a second followers row of the same capture: %v, want 23505", err)
	}
	f.ownerRun(t, `DELETE FROM ig_follow WHERE direction = 'followers'`)
}

// TestMigration00033Checks: each new CHECK takes the edge that is allowed and refuses a bad row.
func TestMigration00033Checks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "intent-checks.example", nil, 7, "")
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	touch := `INSERT INTO ig_vendor_touch (vendor_handle, handle, kind, ref, post_code, at, text, capture_sha256, captured_at)
		VALUES ($1, $2, $3, $4, 'Cabc123', now(), $5, $6, now())`
	sha := make([]byte, 32)
	long := strings.Repeat("a", 301)
	for _, c := range []struct {
		name, want, sql string
		args            []any
	}{
		{"follow direction", "23514", `INSERT INTO ig_follow (list_owner, direction, handle, captured_at, capture_sha256) VALUES ('vendor.example', 'both', 'cafe.exemplu', now(), $1)`, []any{sha}},
		{"touch comment", "", touch, []any{"vendor.example", "cafe.exemplu", "comment", "17900000000000001", strings.Repeat("a", 300), sha}},
		{"touch tagged", "", touch, []any{"vendor.example", "cafe.exemplu", "tagged", "Cabc123", nil, sha}},
		{"touch kind", "23514", touch, []any{"vendor.example", "cafe.exemplu", "like", "Cabc123", nil, sha}},
		{"touch handle", "23514", touch, []any{"vendor.example", "Cafe.Exemplu", "tagged", "Cabc123", nil, sha}},
		{"touch vendor handle", "23514", touch, []any{"@vendor.example", "cafe.exemplu", "tagged", "Cabc123", nil, sha}},
		{"touch ref", "23514", touch, []any{"vendor.example", "cafe.exemplu", "tagged", "abc/123", nil, sha}},
		{"touch text on a tag", "23514", touch, []any{"vendor.example", "cafe.exemplu", "usertag", "Cabc123", "hi", sha}},
		{"touch text length", "23514", touch, []any{"vendor.example", "cafe.exemplu", "comment", "17900000000000002", long, sha}},
		{"touch sha", "23514", touch, []any{"vendor.example", "cafe.exemplu", "tagged", "Cabc124", nil, sha[:31]}},
		{"intent ok", "", `INSERT INTO lead_intent (lead_id, class, overlap, score, reason) VALUES ($1, 'just_chose', 0, 0, 'tagged by a vendor')`, []any{lead}},
		{"intent class", "23514", `INSERT INTO lead_intent (lead_id, class, overlap, score, reason) VALUES ($1, 'warm', 0, 0, 'x')`, []any{lead}},
		{"intent overlap", "23514", `INSERT INTO lead_intent (lead_id, class, overlap, score, reason) VALUES ($1, 'cold', -1, 0, 'x')`, []any{lead}},
		{"intent reason newline", "23514", `INSERT INTO lead_intent (lead_id, class, overlap, score, reason) VALUES ($1, 'cold', 0, 0, E'a\nb')`, []any{lead}},
		{"intent reason length", "23514", `INSERT INTO lead_intent (lead_id, class, overlap, score, reason) VALUES ($1, 'cold', 0, 0, $2)`, []any{lead, long}},
		{"intent rules", "23514", `INSERT INTO lead_intent (lead_id, class, overlap, score, reason, rules) VALUES ($1, 'cold', 0, 0, 'x', 'intent1')`, []any{lead}},
		{"call clear", "", `INSERT INTO lead_intent_call (lead_id, class, actor) VALUES ($1, NULL, 'gremy')`, []any{lead}},
		{"call class", "23514", `INSERT INTO lead_intent_call (lead_id, class, actor) VALUES ($1, 'warm', 'gremy')`, []any{lead}},
		{"call actor", "23514", `INSERT INTO lead_intent_call (lead_id, class, actor) VALUES ($1, 'out', 'system')`, []any{lead}},
		{"call note", "23514", `INSERT INTO lead_intent_call (lead_id, class, note, actor) VALUES ($1, 'out', E'a\nb', 'gremy')`, []any{lead}},
	} {
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, c.sql, c.args...)
		if got := sqlState(err); got != c.want || (c.want == "" && err != nil) {
			t.Errorf("%s: %v (SQLSTATE %q), want %q", c.name, err, got, c.want)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
	}
}
