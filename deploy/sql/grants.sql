-- grants.sql: what leadhound_app may do in one database. Run by a superuser in each
-- leadhound database (make db-local runs it in leadhound_dev and leadhound_test) after
-- roles.sql and after the database exists; safe to run again, before or after a migrate.
--
-- Default privileges are stored per database, so they cannot live in the cluster-wide
-- roles.sql. They are set FOR ROLE leadhound_owner WITHOUT "IN SCHEMA", so they survive a
-- DROP SCHEMA public / CREATE SCHEMA public and reach every table a later migration creates.
-- The app role gets no DELETE, no TRUNCATE and owns nothing, so it cannot drop a table.

ALTER DEFAULT PRIVILEGES FOR ROLE leadhound_owner
  GRANT SELECT, INSERT, UPDATE ON TABLES TO leadhound_app;
ALTER DEFAULT PRIVILEGES FOR ROLE leadhound_owner
  GRANT USAGE, SELECT ON SEQUENCES TO leadhound_app;

-- A recreated schema loses this grant, so it is repeated on every run.
GRANT USAGE ON SCHEMA public TO leadhound_app;
-- Nobody but the owner creates objects in the schema (Postgres 15+ already starts this way;
-- an older or hand-made schema may not).
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

-- The tables that already exist, for a run after a migrate: the default privileges above only
-- reach tables created later.
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA public TO leadhound_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO leadhound_app;

-- goose's version tables stay read-only to the app: a version row the app could forge would
-- make goose skip a real migration. store.Migrate and leadstore.Migrate run the same revokes
-- after every migrate, so the order of this file and a migrate does not matter.
DO $$
BEGIN
  IF to_regclass('public.goose_db_version') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.goose_db_version FROM leadhound_app;
  END IF;
  IF to_regclass('public.goose_db_version_leads') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.goose_db_version_leads FROM leadhound_app;
  END IF;
END
$$;

-- The append-only lead tables take no UPDATE from the app; the blanket grant above gave it.
-- leadstore.Migrate runs the same revokes.
DO $$
BEGIN
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
  -- org's one state pointer: UPDATE on site_id only.
  IF to_regclass('public.org') IS NOT NULL THEN
    REVOKE UPDATE ON public.org FROM leadhound_app;
    GRANT UPDATE (site_id) ON public.org TO leadhound_app;
  END IF;
  -- The outreach tables' state pointers: UPDATE on those columns only (00003 names them;
  -- 00004 adds outreach_batch.discarded_at).
  -- mailbox_cursor keeps the blanket UPDATE: every column of it is a pointer.
  IF to_regclass('public.lead') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead FROM leadhound_app;
    GRANT UPDATE (state, state_at, kind, probe_id, agency_finding_id, channel, route_id) ON public.lead TO leadhound_app;
    -- finding_id comes with leads 00010; a database not yet migrated that far skips it, and
    -- the next run after its migrate grants it.
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
    GRANT UPDATE (state, due_at, sent_at, smtp_reply, cancel_reason, approved_sha256) ON public.outreach_message TO leadhound_app;
    -- hard_bounce comes with 00007; a database not yet migrated that far skips it, and the
    -- next run after its migrate grants it.
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND table_name = 'outreach_message' AND column_name = 'hard_bounce') THEN
      GRANT UPDATE (hard_bounce) ON public.outreach_message TO leadhound_app;
    END IF;
  END IF;
  IF to_regclass('public.lead_event') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_event FROM leadhound_app;
  END IF;
  IF to_regclass('public.suppression') IS NOT NULL THEN
    REVOKE UPDATE ON public.suppression FROM leadhound_app;
  END IF;
  -- suppression_lift (00009) is append-only like suppression. The default privileges and the
  -- blanket grant above reach views too, and suppression_active is a simple, auto-updatable
  -- view that runs with the owner's rights on suppression: left granted, the app could UPDATE
  -- suppression through it. So the view is read-only to the app.
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
  -- send_pause (00004) and the priority tables (00005) are append-only too; a table not yet
  -- made is skipped, and the next run after its migrate revokes it.
  IF to_regclass('public.send_pause') IS NOT NULL THEN
    REVOKE UPDATE ON public.send_pause FROM leadhound_app;
  END IF;
  IF to_regclass('public.priority_profile') IS NOT NULL THEN
    REVOKE UPDATE ON public.priority_profile FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_priority') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_priority FROM leadhound_app;
  END IF;
  -- The sending mailboxes (00007): sender_mailbox and mailbox_check take UPDATE on their
  -- pointer columns only (a mailbox's address is fixed), mailbox_state is append-only, and
  -- outreach_message.hard_bounce joins that table's pointers above.
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
    -- landing_url comes with leads 00024; a database not yet migrated that far skips it.
    IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public'
               AND table_name = 'mailbox_check' AND column_name = 'landing_url') THEN
      GRANT UPDATE (landing_url) ON public.mailbox_check TO leadhound_app;
    END IF;
  END IF;
  -- The lead kinds and channels (leads 00010) are reference rows that change only by
  -- migration: the app reads them only.
  IF to_regclass('public.lead_kind') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.lead_kind FROM leadhound_app;
  END IF;
  IF to_regclass('public.lead_channel') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.lead_channel FROM leadhound_app;
  END IF;
  -- Findings (00010), stars (00011), org facts and enrich attempts (00012), relevance and its
  -- evals (00013), the leads settings (00014) and the operator's calls (00015) are append-only.
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
  -- Accounts and campaigns (00016): account and campaign_version take no UPDATE; campaign
  -- takes UPDATE on its one pointer, retired_at.
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
  -- Call outcomes and email consents (00017) are append-only. The columns 00017 adds to
  -- outreach_message and outreach_batch are fixed at insert: the pointer grants above stay.
  IF to_regclass('public.call_outcome') IS NOT NULL THEN
    REVOKE UPDATE ON public.call_outcome FROM leadhound_app;
  END IF;
  IF to_regclass('public.email_consent') IS NOT NULL THEN
    REVOKE UPDATE ON public.email_consent FROM leadhound_app;
  END IF;
  -- Contact research (00018): UPDATE on the columns a run moves only; the org, lead,
  -- campaign, trigger, want, request time and actor are fixed at insert.
  IF to_regclass('public.contact_research') IS NOT NULL THEN
    REVOKE UPDATE ON public.contact_research FROM leadhound_app;
    GRANT UPDATE (status, model, cost_micros, candidates, verified, started_at, finished_at)
      ON public.contact_research TO leadhound_app;
  END IF;
  -- Claude drafts, their reviews and the drafting evals (00019) are append-only.
  IF to_regclass('public.draft_generation') IS NOT NULL THEN
    REVOKE UPDATE ON public.draft_generation FROM leadhound_app;
  END IF;
  IF to_regclass('public.draft_review') IS NOT NULL THEN
    REVOKE UPDATE ON public.draft_review FROM leadhound_app;
  END IF;
  IF to_regclass('public.draft_eval') IS NOT NULL THEN
    REVOKE UPDATE ON public.draft_eval FROM leadhound_app;
  END IF;
  -- Phone screenings, phone statuses and phone research rows (00020) are append-only.
  IF to_regclass('public.phone_screening') IS NOT NULL THEN
    REVOKE UPDATE ON public.phone_screening FROM leadhound_app;
  END IF;
  IF to_regclass('public.phone_status') IS NOT NULL THEN
    REVOKE UPDATE ON public.phone_status FROM leadhound_app;
  END IF;
  IF to_regclass('public.phone_research') IS NOT NULL THEN
    REVOKE UPDATE ON public.phone_research FROM leadhound_app;
  END IF;
  -- Places and their imports (00021) are append-only; the org's region is fixed at insert.
  IF to_regclass('public.place_import') IS NOT NULL THEN
    REVOKE UPDATE ON public.place_import FROM leadhound_app;
  END IF;
  IF to_regclass('public.place') IS NOT NULL THEN
    REVOKE UPDATE ON public.place FROM leadhound_app;
  END IF;
  -- The job switches (00023) and the campaign mailbox assignments (00024) are append-only.
  IF to_regclass('public.job_switch') IS NOT NULL THEN
    REVOKE UPDATE ON public.job_switch FROM leadhound_app;
  END IF;
  IF to_regclass('public.campaign_mailbox') IS NOT NULL THEN
    REVOKE UPDATE ON public.campaign_mailbox FROM leadhound_app;
  END IF;
END
$$;
