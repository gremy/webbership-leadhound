-- limits.sql: per-role Postgres fences for leadhound's two roles. Cluster-wide, so it runs
-- against any database (make db-local and install.sh run it against "postgres") by a
-- superuser, after roles.sql; safe to run again, and every deploy does.
--
-- The server's Postgres is shared with other databases, and it spends a query's CPU, memory
-- and temp disk in its own service, outside the leadhound units' limits. So the bounds on
-- what one leadhound query may take live here, on the roles, where Postgres enforces them.
-- A setting applies to sessions that start after this runs.
--
-- leadhound_app: no statement past 3 minutes (a longer one is the program's choice, set for
-- one session with a stated bound); no transaction left idle past 10 minutes, which is above
-- the usage lock's worst case (a transaction held open across an AI call: 3 tries of 20 s
-- each plus backoff, about 62 s), so a billed call's usage row is never rolled back; no
-- parallel workers and no JIT, so one query takes one CPU; at most 512 MB of temp files per
-- session; at most 24 connections (the console's pool and the units' pools, 4 each, with the
-- two leads units running at once). temp_file_limit is superuser-only, hence the superuser.
--
-- leadhound_owner runs the migrations: no parallel workers and no JIT either, and no
-- statement timeout, so an index build may take as long as it needs.

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    ALTER ROLE leadhound_app SET statement_timeout = '3min';
    ALTER ROLE leadhound_app SET idle_in_transaction_session_timeout = '10min';
    ALTER ROLE leadhound_app SET max_parallel_workers_per_gather = 0;
    ALTER ROLE leadhound_app SET jit = off;
    ALTER ROLE leadhound_app SET temp_file_limit = '512MB';
    ALTER ROLE leadhound_app CONNECTION LIMIT 24;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_owner') THEN
    ALTER ROLE leadhound_owner SET max_parallel_workers_per_gather = 0;
    ALTER ROLE leadhound_owner SET max_parallel_maintenance_workers = 0;
    ALTER ROLE leadhound_owner SET jit = off;
  END IF;
END
$$;
