-- +goose Up
-- The operator's pause switch for sending, and a discarded mark on outreach batches.
--
-- send_pause is append-only (UPDATE revoked from the app role, as on site_probe): every
-- pause and resume is a row, the latest row (by id) is the switch, and no row means not
-- paused. leads send reads it at the start of a run and before every message.
--
-- outreach_batch.discarded_at is a state pointer: a batch the operator threw away before
-- approving it. The app role's column UPDATE on outreach_batch widens to it (leadstore.Migrate
-- and deploy/sql/grants.sql).
CREATE TABLE send_pause (
  id bigserial PRIMARY KEY,
  paused boolean NOT NULL,
  reason text NOT NULL DEFAULT '',
  actor text NOT NULL,                -- as lead_event.actor
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT send_pause_actor_check CHECK (actor IN ('gremy', 'system'))
);

ALTER TABLE outreach_batch ADD COLUMN discarded_at timestamptz;

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
ALTER TABLE outreach_batch DROP COLUMN discarded_at;
DROP TABLE send_pause;
