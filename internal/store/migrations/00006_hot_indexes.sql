-- +goose NO TRANSACTION
-- +goose Up
-- Indexes for the hot lookups: the score queue's gate check by observation, the funnel's
-- date filters on gate_result, decision and job, the decision join by job and observation,
-- and the bounded Tracked list (status events past the system's 'new').
-- Each is built concurrently, so a live database keeps taking writes while it builds; that
-- cannot run in a transaction, hence NO TRANSACTION and one statement per line. A concurrent
-- build that fails leaves an invalid index behind, so each CREATE is preceded by a DROP of the
-- same name and a retry rebuilds it. The lock timeout stops a migrate that cannot get its
-- lock from queueing every reader of the table behind it; it fails, and a rerun retries.
SET lock_timeout = '5s';

DROP INDEX CONCURRENTLY IF EXISTS gate_result_observation_idx;
CREATE INDEX CONCURRENTLY gate_result_observation_idx ON gate_result (observation_id);

DROP INDEX CONCURRENTLY IF EXISTS gate_result_created_idx;
CREATE INDEX CONCURRENTLY gate_result_created_idx ON gate_result (created_at);

DROP INDEX CONCURRENTLY IF EXISTS decision_created_idx;
CREATE INDEX CONCURRENTLY decision_created_idx ON decision (created_at);

DROP INDEX CONCURRENTLY IF EXISTS decision_job_observation_idx;
CREATE INDEX CONCURRENTLY decision_job_observation_idx ON decision (job_id, observation_id);

DROP INDEX CONCURRENTLY IF EXISTS job_first_seen_idx;
CREATE INDEX CONCURRENTLY job_first_seen_idx ON job (first_seen_at);

DROP INDEX CONCURRENTLY IF EXISTS status_event_active_idx;
CREATE INDEX CONCURRENTLY status_event_active_idx ON status_event (job_id, at DESC) WHERE status <> 'new';

-- The session goes back to the pool after the migration, so it does not keep the timeout.
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS status_event_active_idx;
DROP INDEX CONCURRENTLY IF EXISTS job_first_seen_idx;
DROP INDEX CONCURRENTLY IF EXISTS decision_job_observation_idx;
DROP INDEX CONCURRENTLY IF EXISTS decision_created_idx;
DROP INDEX CONCURRENTLY IF EXISTS gate_result_created_idx;
DROP INDEX CONCURRENTLY IF EXISTS gate_result_observation_idx;
