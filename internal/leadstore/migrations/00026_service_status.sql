-- +goose Up
-- What the console's Status page and Dashboard read about the paid services, and the Claude
-- top-ups the spend since the last one is counted from.
--
-- service_status: one row per provider (a closed set: reoon, claude), written by whatever
-- reads the provider. data and fetched_at are the last good reading (a JSON object, as the
-- provider answered it, and when it was taken); error and error_at the last failure (one
-- line). A success never clears the last error and a failure never clears the last reading:
-- the reader compares error_at with fetched_at to tell which is newer. Both pairs are only
-- ever moved forward in time (leadstore.RecordServiceOK and RecordServiceError), so a late
-- writer never puts an older reading back. The row is a pointer, so the app keeps
-- table-wide UPDATE on it (the default privileges give it; no revoke).
--
-- claude_topup: one row per top-up of the Claude account the operator records, amount in
-- cents, at when it was made; the latest by at (then id) is the one spend is counted from.
-- No revoke either: a mistyped top-up is put right by recording the right one later.
CREATE TABLE service_status (
  provider text PRIMARY KEY,
  data jsonb,                           -- the last good reading; NULL before the first
  fetched_at timestamptz,               -- when data was read; NULL before the first
  error text NOT NULL DEFAULT '',       -- the last failure, one line; '' when none ever
  error_at timestamptz,                 -- when it failed; NULL when it never has
  CONSTRAINT service_status_provider_check CHECK (provider IN ('reoon', 'claude')),
  CONSTRAINT service_status_data_check CHECK ((data IS NULL) = (fetched_at IS NULL)
    AND (data IS NULL OR jsonb_typeof(data) = 'object')),
  CONSTRAINT service_status_error_check CHECK ((error = '') = (error_at IS NULL)
    AND char_length(error) <= 500 AND error !~ '[\r\n]')
);

CREATE TABLE claude_topup (
  id bigserial PRIMARY KEY,
  amount_cents bigint NOT NULL,
  at timestamptz NOT NULL,              -- when the top-up was made
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT claude_topup_amount_check CHECK (amount_cents > 0)
);
CREATE INDEX claude_topup_at_idx ON claude_topup (at DESC, id DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop. Every reading and top-up goes with them.
DROP TABLE claude_topup;
DROP TABLE service_status;
