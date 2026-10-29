-- +goose Up
-- The operator's on/off switch for each scheduled job, mirroring send_pause (00004).
--
-- job_switch is append-only (UPDATE revoked from the app role, as on send_pause): every
-- switch on and off is a row, the latest row (by id) of an account's job is that job's
-- switch, and no row means on. A job switched off exits 0 and records its run as switched
-- off; sending keeps send_pause as its switch. The column is is_on, not on: ON is a reserved
-- word in Postgres and would need quoting in every query.
CREATE TABLE job_switch (
  id bigserial PRIMARY KEY,
  account_id bigint NOT NULL DEFAULT 1 REFERENCES account(id),
  job text NOT NULL,
  is_on boolean NOT NULL,
  reason text NOT NULL DEFAULT '',
  actor text NOT NULL,                -- as send_pause.actor
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT job_switch_job_check CHECK (job IN ('batch', 'sync', 'probe', 'contacts', 'research', 'verify')),
  CONSTRAINT job_switch_reason_check CHECK (reason !~ '[\r\n]' AND length(reason) <= 300),
  CONSTRAINT job_switch_actor_check CHECK (actor IN ('gremy', 'system'))
);
CREATE INDEX job_switch_account_job_idx ON job_switch (account_id, job, id DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE job_switch;
