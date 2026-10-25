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
  END IF;
  IF to_regclass('public.outreach_batch') IS NOT NULL THEN
    REVOKE UPDATE ON public.outreach_batch FROM leadhound_app;
    GRANT UPDATE (approved_at, approved_hash, discarded_at) ON public.outreach_batch TO leadhound_app;
  END IF;
  IF to_regclass('public.outreach_message') IS NOT NULL THEN
    REVOKE UPDATE ON public.outreach_message FROM leadhound_app;
    GRANT UPDATE (state, due_at, sent_at, smtp_reply, cancel_reason, approved_sha256) ON public.outreach_message TO leadhound_app;
  END IF;
  IF to_regclass('public.lead_event') IS NOT NULL THEN
    REVOKE UPDATE ON public.lead_event FROM leadhound_app;
  END IF;
  IF to_regclass('public.suppression') IS NOT NULL THEN
    REVOKE UPDATE ON public.suppression FROM leadhound_app;
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
END
$$;
