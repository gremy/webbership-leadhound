-- +goose Up
-- The email check before sending: what the verifier (Reoon) answered about an address.
--
-- email_check is append-only (UPDATE revoked from the app role, as on job_switch): every
-- check is a row, and the latest row (by id) of an account's address is that address's
-- result. A result older than 90 days counts as unchecked (the store reads only the fresh
-- ones), so an address is checked again rather than trusted forever. The address is stored
-- lower-cased, as suppression stores one, so one address is one history. status is the
-- verifier's own word (safe, invalid, catch_all, ...): which of them may enter a live batch
-- is decided in code (internal/verify Verdict), so a new word from the verifier never needs a
-- migration and never sends by accident (an unknown word is not sendable).
CREATE TABLE email_check (
  id bigserial PRIMARY KEY,
  account_id bigint NOT NULL DEFAULT 1 REFERENCES account(id),
  address text NOT NULL,
  provider text NOT NULL,
  status text NOT NULL,
  score int NULL,                     -- the verifier's overall score (0..100), when it gave one
  catch_all boolean NULL,             -- its is_catch_all, when it gave one
  role boolean NULL,                  -- its is_role_account, when it gave one
  checked_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT email_check_address_check CHECK (address = lower(address) AND address <> '' AND length(address) <= 320),
  CONSTRAINT email_check_provider_check CHECK (provider IN ('reoon')),
  CONSTRAINT email_check_status_check CHECK (status ~ '^[a-z_]{1,40}$')
);
CREATE INDEX email_check_account_address_idx ON email_check (account_id, address, id DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE email_check;
