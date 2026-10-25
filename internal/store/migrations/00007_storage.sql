-- +goose NO TRANSACTION
-- +goose Up
-- The storage schema: the nightly trim's marker and keyset index, the collectors' disk_budget
-- stop reason, and the trim itself as a function the app role may run.
-- The index is built concurrently, so this runs outside a transaction with one statement per
-- line, drops before it builds (a failed concurrent build leaves an invalid index a retry
-- replaces), and bounds its lock wait like 00006.
SET lock_timeout = '5s';

-- When the trim blanked this observation's description_clean; NULL while the text is whole.
ALTER TABLE job_observation ADD COLUMN IF NOT EXISTS description_trimmed_at timestamptz;

-- The trim's keyset scan: the untrimmed observations in id order.
DROP INDEX CONCURRENTLY IF EXISTS job_observation_untrimmed_id_idx;
CREATE INDEX CONCURRENTLY job_observation_untrimmed_id_idx ON job_observation (id) WHERE description_trimmed_at IS NULL;

-- A collector stops storing when the database passes its size budget or the disk its floor.
ALTER TABLE run DROP CONSTRAINT run_stop_reason_check, ADD CONSTRAINT run_stop_reason_check CHECK (stop_reason IN ('', 'throttled', 'blocked', 'failure_rate', 'budget', 'parser_broke', 'spend_cap', 'disk_budget'));

-- leadhound_trim_batch scans the next batch_size untrimmed observations after after_id and
-- blanks the description text of those that are rejected (observed before rejected_before
-- with a failing gate on that observation) or expired (the job's listing expired and last
-- checked before expired_before). A protected job is never trimmed: one with any status event
-- by the operator, one whose current status is applied, interviewing or offer, or one listed
-- in any brief. A job skipped by the system alone is not protected. Every row, verdict and id
-- stays. It returns the last id scanned (NULL when there was nothing after after_id) and the
-- counts; an observation both rejected and expired counts as rejected.
-- It runs as its owner (SECURITY DEFINER) with a fixed search_path, so the app role decides
-- only when it runs, never what it trims.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION leadhound_trim_batch(rejected_before timestamptz, expired_before timestamptz, after_id bigint, batch_size int)
RETURNS TABLE (last_id bigint, rejected int, expired int)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp AS $$
BEGIN
  IF batch_size IS NULL OR batch_size < 1 OR after_id IS NULL
     OR rejected_before IS NULL OR expired_before IS NULL THEN
    RAISE EXCEPTION 'leadhound_trim_batch: arguments must be set and batch_size positive';
  END IF;
  RETURN QUERY
  WITH batch AS (
    SELECT o.id AS obs_id, o.job_id AS obs_job, o.observed_at AS obs_at
    FROM job_observation o
    WHERE o.description_trimmed_at IS NULL AND o.id > after_id
    ORDER BY o.id
    LIMIT batch_size
  ), briefed AS (
    SELECT DISTINCT unnest(b.job_ids) AS briefed_job FROM brief_log b
  ), target AS (
    SELECT t.obs_id,
           (t.obs_at < rejected_before
            AND EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = t.obs_id AND g.verdict = 'fail')) AS is_rej,
           (j.listing_status = 'expired' AND j.status_checked_at < expired_before) AS is_exp
    FROM batch t
    JOIN job j ON j.id = t.obs_job
    WHERE NOT EXISTS (SELECT 1 FROM status_event s WHERE s.job_id = t.obs_job AND s.actor = 'gremy')
      AND coalesce((SELECT s.status FROM status_event s WHERE s.job_id = t.obs_job
                    ORDER BY s.at DESC, s.id DESC LIMIT 1), '') NOT IN ('applied', 'interviewing', 'offer')
      AND NOT EXISTS (SELECT 1 FROM briefed bj WHERE bj.briefed_job = t.obs_job)
  ), trimmed AS (
    UPDATE job_observation o
    SET description_clean = '', description_trimmed_at = now()
    FROM target tg
    WHERE o.id = tg.obs_id AND (tg.is_rej OR tg.is_exp)
    RETURNING tg.is_rej
  )
  SELECT (SELECT max(t.obs_id) FROM batch t),
         (SELECT count(*) FROM trimmed tr WHERE tr.is_rej)::int,
         (SELECT count(*) FROM trimmed tr WHERE NOT tr.is_rej)::int;
END
$$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION leadhound_trim_batch(timestamptz, timestamptz, bigint, int) FROM PUBLIC;

-- Owned by the owner role even when a superuser migrates, and executable by the app role;
-- each only where the role exists.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_owner') THEN
    ALTER FUNCTION leadhound_trim_batch(timestamptz, timestamptz, bigint, int) OWNER TO leadhound_owner;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    GRANT EXECUTE ON FUNCTION leadhound_trim_batch(timestamptz, timestamptz, bigint, int) TO leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- The session goes back to the pool after the migration, so it does not keep the timeout.
RESET lock_timeout;

-- +goose Down
-- Runs as the owner only. Restoring the stop-reason check fails while a run stopped for
-- disk_budget is in the table: those rows are the operator's to change first. The trimmed
-- text does not come back.
SET lock_timeout = '5s';
DROP FUNCTION IF EXISTS leadhound_trim_batch(timestamptz, timestamptz, bigint, int);
ALTER TABLE run DROP CONSTRAINT run_stop_reason_check, ADD CONSTRAINT run_stop_reason_check CHECK (stop_reason IN ('', 'throttled', 'blocked', 'failure_rate', 'budget', 'parser_broke', 'spend_cap'));
DROP INDEX CONCURRENTLY IF EXISTS job_observation_untrimmed_id_idx;
ALTER TABLE job_observation DROP COLUMN IF EXISTS description_trimmed_at;
RESET lock_timeout;
