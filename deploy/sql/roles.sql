-- roles.sql: leadhound's two login roles. Cluster-wide, so it runs once against any database
-- (make db-local runs it against "postgres") by a superuser, and is safe to run again.
--
-- leadhound_owner owns the databases and the schema and runs the migrations.
-- leadhound_app is what the program runs as: SELECT, INSERT and UPDATE only (grants.sql).
--
-- No passwords here: locally the roles connect over trust auth; on the server the passwords
-- are set at deploy time, outside the repo (ALTER ROLE … PASSWORD).
-- The privileges are per-database, so they live in grants.sql, run in each database.

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_owner') THEN
    CREATE ROLE leadhound_owner LOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    CREATE ROLE leadhound_app LOGIN;
  END IF;
END
$$;
