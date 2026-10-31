-- +goose Up
-- Manual send: a batch the operator builds from chosen leads and sends now, with the warnings
-- the confirm page showed overridden by name.
--
-- outreach_batch.manual marks such a batch. It is a state pointer the app sets once, on a batch
-- neither approved nor discarded (leadstore.MarkBatchManual), so the app role's column UPDATE on
-- outreach_batch widens to it (leadstore.Migrate and deploy/sql/grants.sql). A manual batch never
-- blocks a new batch (UnapprovedBatch leaves it out), and one left unconfirmed is discarded once
-- it is older than the confirm window (DiscardStaleManualBatches), dated by created_at.
--
-- send_request: one row per confirm of a manual send, written by the console. The send service
-- reads its batch id from the request file, then takes the newest unused row for that batch
-- (ClaimSendRequest), so the override codes and the operator come from here, never from the
-- file; shown keeps the warnings with their numbers as the confirm page showed them (a JSON
-- array), so each send_override row can carry them. used_at is the one pointer: set once, when the row is claimed; the app keeps UPDATE on
-- it only.
--
-- send_override: one row per message sent with any warning overridden, append-only (UPDATE
-- revoked from the app role). run_id names the core run table, which belongs to the other
-- migration set, so it takes no foreign key.
--
-- codes on both are a subset of the closed set of overridable warnings; every hard stop stays
-- outside it.
ALTER TABLE outreach_batch ADD COLUMN manual boolean NOT NULL DEFAULT false;

CREATE TABLE send_request (
  id bigserial PRIMARY KEY,
  batch_id bigint NOT NULL REFERENCES outreach_batch(id),
  codes text[] NOT NULL DEFAULT '{}',
  hash_prefix text NOT NULL,            -- the batch hash prefix the confirm page showed
  operator text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  used_at timestamptz,                  -- NULL until the send service claims it; set once
  shown jsonb NOT NULL DEFAULT '[]',    -- the warnings, with their numbers, the confirm page showed
  CONSTRAINT send_request_shown_check CHECK (jsonb_typeof(shown) = 'array'),
  CONSTRAINT send_request_codes_check CHECK (codes <@ ARRAY['window', 'business_hours', 'ramp',
    'domain_daily', 'spacing_jitter', 'schedule']::text[])
);
CREATE INDEX send_request_batch_idx ON send_request (batch_id, created_at DESC, id DESC);

CREATE TABLE send_override (
  id bigserial PRIMARY KEY,
  run_id bigint,                        -- the core run; no FK (the other migration set)
  batch_id bigint NOT NULL,
  lead_id bigint NOT NULL,
  message_id bigint,
  codes text[] NOT NULL,
  detail jsonb NOT NULL DEFAULT '{}',   -- the numbers each overridden warning carried
  operator text NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT send_override_codes_check CHECK (codes <@ ARRAY['window', 'business_hours', 'ramp',
    'domain_daily', 'spacing_jitter', 'schedule']::text[])
);
CREATE INDEX send_override_batch_idx ON send_override (batch_id);

-- +goose Down
-- Runs as the owner only; the app role cannot drop. Every request and override row goes with them.
DROP TABLE send_override;
DROP TABLE send_request;
ALTER TABLE outreach_batch DROP COLUMN manual;
