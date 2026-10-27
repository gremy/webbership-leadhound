-- +goose Up
-- ai_usage counts each billed call against one purpose, so the jobs scorer and the lead
-- rating keep separate daily caps. Every row already there is a jobs call, and a writer that
-- names no purpose still writes one. The default is a constant, so adding the column rewrites
-- nothing; the check reads the table once.
-- The lock timeout stops a migrate that cannot get its lock from queueing every capped call
-- behind it; it fails, and a rerun retries. SET LOCAL ends with the migration's transaction.
SET LOCAL lock_timeout = '5s';
ALTER TABLE ai_usage ADD COLUMN purpose text NOT NULL DEFAULT 'jobs'
  CONSTRAINT ai_usage_purpose_check CHECK (purpose IN ('jobs', 'leads'));

-- +goose Down
-- Runs as the owner only. Each row's purpose is lost, and every count reads all calls again.
SET LOCAL lock_timeout = '5s';
ALTER TABLE ai_usage DROP COLUMN IF EXISTS purpose;
