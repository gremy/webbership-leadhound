package leadstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver goose needs
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// versionTable is goose's version table for the leads set, separate from the core set's
// goose_db_version, so the two sets are numbered independently.
const versionTable = "goose_db_version_leads"

// ErrNoCoreSchema is returned by Migrate when the core tables the lead tables reference do
// not exist yet.
var ErrNoCoreSchema = errors.New("leadstore: migrate: core schema missing, run store.Migrate first")

// Migrate applies every pending leads migration to the database at migrateDSN, which must
// be the owner role and must already hold the core schema (store.Migrate). A database
// already at the latest leads version is left as it is.
//
// It takes the same Postgres session advisory lock as store.Migrate, so a leads migrate
// and a core migrate never run at the same time.
func Migrate(ctx context.Context, migrateDSN string) (err error) {
	connCfg, err := pgx.ParseConfig(migrateDSN)
	if err != nil {
		// The DSN may carry a password, so it never goes into the error.
		return fmt.Errorf("leadstore: migrate: parse dsn: %w", err)
	}
	db := stdlib.OpenDB(*connCfg)
	defer func() {
		if cerr := db.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("leadstore: migrate: close: %w", cerr))
		}
	}()
	return migrate(ctx, db)
}

func migrate(ctx context.Context, db *sql.DB) error {
	var core bool
	if err := db.QueryRowContext(ctx,
		"SELECT to_regclass('public.run') IS NOT NULL AND to_regclass('public.http_fetch') IS NOT NULL").Scan(&core); err != nil {
		return fmt.Errorf("leadstore: migrate: check core schema: %w", err)
	}
	if !core {
		return ErrNoCoreSchema
	}
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("leadstore: migrate: migrations dir: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("leadstore: migrate: locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub,
		goose.WithTableName(versionTable), goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("leadstore: migrate: provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("leadstore: migrate: up: %w", err)
	}
	if _, err := db.ExecContext(ctx, revokeLeadsSQL); err != nil {
		return fmt.Errorf("leadstore: migrate: revoke app writes: %w", err)
	}
	return nil
}

// revokeLeadsSQL takes from the app role what the owner's default privileges gave it and
// it must not have: every write on the leads version table, and UPDATE on the append-only
// lead tables (send_pause and the priority tables among them). On org, lead, outreach_batch
// and outreach_message it swaps the table-wide UPDATE for UPDATE on their state-pointer
// columns only (00002, 00003 and 00004 name them); sender_mailbox and mailbox_check get the
// same, outreach_message gains hard_bounce, and mailbox_state is append-only (00007);
// mailbox_cursor keeps its table-wide UPDATE, every column of it being a pointer.
// suppression_lift is append-only and the suppression_active view takes no INSERT or UPDATE
// (00009: an auto-updatable view would let the app UPDATE suppression through it). lead's
// pointers gain finding_id, finding is append-only, and the reference rows lead_kind and
// lead_channel are read-only to the app (00010); lead_star (00011), org_fact and
// enrich_attempt (00012), lead_relevance and lead_relevance_eval (00013), lead_setting
// (00014) and lead_label (00015) are append-only; account takes no UPDATE, campaign takes
// UPDATE on retired_at only, and campaign_version is append-only (00016), which also
// recreates suppression_active, so its revokes above run again; call_outcome and
// email_consent are append-only (00017), whose new outreach_message and outreach_batch
// columns are fixed at insert and join no pointer grant; contact_research takes UPDATE on the
// columns a research run moves only (00018); draft_generation, draft_review and draft_eval
// are append-only (00019), and so are phone_screening, phone_status and phone_research
// (00020; the owner alone deletes from them, to erase an org); place_import and place are
// append-only (00021), whose new org.region column is fixed at insert. It runs after every migrate, a no-op one
// too; deploy/sql/grants.sql does the same, so either may run last. Each statement is
// skipped when the app role, its table or its column does not exist.
const revokeLeadsSQL = `DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    RETURN;
  END IF;
  IF to_regclass('public.goose_db_version_leads') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.goose_db_version_leads FROM leadhound_app;
  END IF;
  IF to_regclass('public.site_probe') IS NOT NULL THEN
    REVOKE UPDATE ON public.site_probe FROM leadhound_app;
  END IF;
  IF to_regclass('public.fetch_header') IS NOT NULL THEN
    REVOKE UPDATE ON public.fetch_header FROM leadhound_app;
  END IF;
  IF to_regclass('public.contact_route') IS NOT NULL THEN
    REVOKE UPDATE ON public.contact_route FROM leadhound_app;
  END IF;
  IF to_regclass('public.agency_finding') IS NOT NULL THEN
    REVOKE UPDATE ON public.agency_finding FROM leadhound_app;
  END IF;
  IF to_regclass('public.contact_attempt') IS NOT NULL THEN
    REVOKE UPDATE ON public.contact_attempt FROM leadhound_app;
  END IF;
  IF to_regclass('public.org') IS NOT NULL THEN
    REVOKE UPDATE ON public.org FROM leadhound_app;
    GRANT UPDATE (site_id) ON public.org TO leadhound_app;
  END IF;
  IF to_regclass('public.lead') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead FROM leadhound_app;
    GRANT UPDATE (state, state_at, kind, probe_id, agency_finding_id, channel, route_id) ON public.lead TO leadhound_app;
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND table_name = 'lead' AND column_name = 'finding_id') THEN
      GRANT UPDATE (finding_id) ON public.lead TO leadhound_app;
    END IF;
  END IF;
  IF to_regclass('public.outreach_batch') IS NOT NULL THEN
    REVOKE UPDATE ON public.outreach_batch FROM leadhound_app;
    GRANT UPDATE (approved_at, approved_hash, discarded_at) ON public.outreach_batch TO leadhound_app;
  END IF;
  IF to_regclass('public.outreach_message') IS NOT NULL THEN
    REVOKE UPDATE ON public.outreach_message FROM leadhound_app;
    GRANT UPDATE (state, due_at, sent_at, smtp_reply, cancel_reason, approved_sha256, hard_bounce) ON public.outreach_message TO leadhound_app;
  END IF;
  IF to_regclass('public.lead_event') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_event FROM leadhound_app;
  END IF;
  IF to_regclass('public.suppression') IS NOT NULL THEN
    REVOKE UPDATE ON public.suppression FROM leadhound_app;
  END IF;
  IF to_regclass('public.suppression_lift') IS NOT NULL THEN
    REVOKE UPDATE ON public.suppression_lift FROM leadhound_app;
  END IF;
  IF to_regclass('public.suppression_active') IS NOT NULL THEN
    REVOKE INSERT, UPDATE ON public.suppression_active FROM leadhound_app;
  END IF;
  IF to_regclass('public.mailbox_event') IS NOT NULL THEN
    REVOKE UPDATE ON public.mailbox_event FROM leadhound_app;
  END IF;
  IF to_regclass('public.sender_check') IS NOT NULL THEN
    REVOKE UPDATE ON public.sender_check FROM leadhound_app;
  END IF;
  IF to_regclass('public.send_pause') IS NOT NULL THEN
    REVOKE UPDATE ON public.send_pause FROM leadhound_app;
  END IF;
  IF to_regclass('public.priority_profile') IS NOT NULL THEN
    REVOKE UPDATE ON public.priority_profile FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_priority') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_priority FROM leadhound_app;
  END IF;
  IF to_regclass('public.sender_mailbox') IS NOT NULL THEN
    REVOKE UPDATE ON public.sender_mailbox FROM leadhound_app;
    GRANT UPDATE (login, smtp_addr, imap_addr, dkim_selector, from_name, password_sealed, sealed_by) ON public.sender_mailbox TO leadhound_app;
  END IF;
  IF to_regclass('public.mailbox_state') IS NOT NULL THEN
    REVOKE UPDATE ON public.mailbox_state FROM leadhound_app;
  END IF;
  IF to_regclass('public.mailbox_check') IS NOT NULL THEN
    REVOKE UPDATE ON public.mailbox_check FROM leadhound_app;
    GRANT UPDATE (checked_at, smtp_ok, imap_ok, folders_ok, spf, dkim, dmarc, fix, outcome) ON public.mailbox_check TO leadhound_app;
  END IF;
  IF to_regclass('public.lead_kind') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.lead_kind FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_channel') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.lead_channel FROM leadhound_app;
  END IF;
  IF to_regclass('public.finding') IS NOT NULL THEN
    REVOKE UPDATE ON public.finding FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_star') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_star FROM leadhound_app;
  END IF;
  IF to_regclass('public.org_fact') IS NOT NULL THEN
    REVOKE UPDATE ON public.org_fact FROM leadhound_app;
  END IF;
  IF to_regclass('public.enrich_attempt') IS NOT NULL THEN
    REVOKE UPDATE ON public.enrich_attempt FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_relevance') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_relevance FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_relevance_eval') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_relevance_eval FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_setting') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_setting FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_label') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_label FROM leadhound_app;
  END IF;
  IF to_regclass('public.account') IS NOT NULL THEN
    REVOKE UPDATE ON public.account FROM leadhound_app;
  END IF;
  IF to_regclass('public.campaign') IS NOT NULL THEN
    REVOKE UPDATE ON public.campaign FROM leadhound_app;
    GRANT UPDATE (retired_at) ON public.campaign TO leadhound_app;
  END IF;
  IF to_regclass('public.campaign_version') IS NOT NULL THEN
    REVOKE UPDATE ON public.campaign_version FROM leadhound_app;
  END IF;
  IF to_regclass('public.call_outcome') IS NOT NULL THEN
    REVOKE UPDATE ON public.call_outcome FROM leadhound_app;
  END IF;
  IF to_regclass('public.email_consent') IS NOT NULL THEN
    REVOKE UPDATE ON public.email_consent FROM leadhound_app;
  END IF;
  IF to_regclass('public.contact_research') IS NOT NULL THEN
    REVOKE UPDATE ON public.contact_research FROM leadhound_app;
    GRANT UPDATE (status, model, cost_micros, candidates, verified, started_at, finished_at)
      ON public.contact_research TO leadhound_app;
  END IF;
  IF to_regclass('public.draft_generation') IS NOT NULL THEN
    REVOKE UPDATE ON public.draft_generation FROM leadhound_app;
  END IF;
  IF to_regclass('public.draft_review') IS NOT NULL THEN
    REVOKE UPDATE ON public.draft_review FROM leadhound_app;
  END IF;
  IF to_regclass('public.draft_eval') IS NOT NULL THEN
    REVOKE UPDATE ON public.draft_eval FROM leadhound_app;
  END IF;
  IF to_regclass('public.phone_screening') IS NOT NULL THEN
    REVOKE UPDATE ON public.phone_screening FROM leadhound_app;
  END IF;
  IF to_regclass('public.phone_status') IS NOT NULL THEN
    REVOKE UPDATE ON public.phone_status FROM leadhound_app;
  END IF;
  IF to_regclass('public.phone_research') IS NOT NULL THEN
    REVOKE UPDATE ON public.phone_research FROM leadhound_app;
  END IF;
  IF to_regclass('public.place_import') IS NOT NULL THEN
    REVOKE UPDATE ON public.place_import FROM leadhound_app;
  END IF;
  IF to_regclass('public.place') IS NOT NULL THEN
    REVOKE UPDATE ON public.place FROM leadhound_app;
  END IF;
END
$$`
