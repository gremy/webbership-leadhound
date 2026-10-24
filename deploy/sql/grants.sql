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
END
$$;
