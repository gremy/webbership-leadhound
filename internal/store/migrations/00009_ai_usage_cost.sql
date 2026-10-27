-- +goose Up
-- ai_usage records Claude calls next to Jev's: each Claude row carries its own tokens, web
-- searches and its cost in micro-dollars (cost_micros, from internal/claude's price table),
-- the account it was spent for, the lead it was spent on (if any) and whether it ran through
-- the Batch API. Jev rows keep cost_micros = 0 and the console still prices them from
-- input_tokens. Every default is a constant, so adding the columns rewrites nothing; the new
-- purpose check reads the table once.
-- account_id and lead_id carry no foreign key: account and lead live in the leads schema
-- set (internal/leadstore, goose_db_version_leads), which migrates after this core set and
-- requires it first, so neither table exists when this file runs on an empty database.
-- account_id defaults to 1, the account every row before accounts belonged to.
-- Grants: none change. ai_usage keeps the owner's default privileges (SELECT, INSERT,
-- UPDATE for leadhound_app, from deploy/sql/grants.sql); ADD COLUMN inherits the table's
-- grants and there is no column-level grant or revoke on ai_usage to carry over.
-- The lock timeout stops a migrate that cannot get its lock from queueing every capped call
-- behind it; it fails, and a rerun retries. SET LOCAL ends with the migration's transaction.
SET LOCAL lock_timeout = '5s';
ALTER TABLE ai_usage
  ADD COLUMN output_tokens int NOT NULL DEFAULT 0,
  ADD COLUMN cache_read_tokens int NOT NULL DEFAULT 0,
  ADD COLUMN cache_write_tokens int NOT NULL DEFAULT 0,
  ADD COLUMN web_searches int NOT NULL DEFAULT 0,
  ADD COLUMN cost_micros bigint NOT NULL DEFAULT 0,
  ADD COLUMN account_id bigint NOT NULL DEFAULT 1,
  ADD COLUMN lead_id bigint,
  ADD COLUMN batch boolean NOT NULL DEFAULT false,
  DROP CONSTRAINT ai_usage_purpose_check,
  ADD CONSTRAINT ai_usage_purpose_check
    CHECK (purpose IN ('jobs', 'leads', 'drafting', 'research', 'drafts_eval'));
-- The Claude cap reads one account's day per purpose, and one lead's total.
CREATE INDEX ai_usage_claude_day ON ai_usage (provider, account_id, day, purpose);
CREATE INDEX ai_usage_claude_lead ON ai_usage (lead_id, purpose) WHERE lead_id IS NOT NULL;

-- +goose Down
-- Runs as the owner only. Every Claude-purpose row (drafting, research, drafts_eval) is
-- deleted, because the old check cannot hold it, and with it the record of what Claude cost;
-- every remaining row loses its output/cache tokens, web searches, cost, account, lead and
-- batch flag. Jev's jobs and leads rows and their counts are untouched.
SET LOCAL lock_timeout = '5s';
DROP INDEX IF EXISTS ai_usage_claude_lead;
DROP INDEX IF EXISTS ai_usage_claude_day;
DELETE FROM ai_usage WHERE purpose NOT IN ('jobs', 'leads');
ALTER TABLE ai_usage
  DROP CONSTRAINT ai_usage_purpose_check,
  ADD CONSTRAINT ai_usage_purpose_check CHECK (purpose IN ('jobs', 'leads')),
  DROP COLUMN IF EXISTS batch,
  DROP COLUMN IF EXISTS lead_id,
  DROP COLUMN IF EXISTS account_id,
  DROP COLUMN IF EXISTS cost_micros,
  DROP COLUMN IF EXISTS web_searches,
  DROP COLUMN IF EXISTS cache_write_tokens,
  DROP COLUMN IF EXISTS cache_read_tokens,
  DROP COLUMN IF EXISTS output_tokens;
