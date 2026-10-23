-- +goose Up
-- A retry resends the stored brief verbatim, so the HTML copy is kept with the text.
-- send_key is the Idempotency-Key the stored payload goes out under ('' reads as
-- brief-<day>). send_state is what the tries of that payload prove: 'pending' (not tried
-- yet), 'maybe_sent' (a try began and may have delivered: it is only ever resent as it
-- is) or 'refused' (its only try was refused by the provider, so nothing went out and the
-- day may be composed afresh under a new key). Column privileges follow the table grants.
ALTER TABLE brief_log
  ADD COLUMN body_html text NOT NULL DEFAULT '',
  ADD COLUMN send_key text NOT NULL DEFAULT '',
  ADD COLUMN send_state text NOT NULL DEFAULT 'pending'
    CHECK (send_state IN ('pending', 'maybe_sent', 'refused'));
-- A day left unsent before this column existed may have been tried: never recompose it.
UPDATE brief_log SET send_state = 'maybe_sent' WHERE sent_at IS NULL;

-- +goose Down
-- Runs as the owner only; the app role cannot alter.
ALTER TABLE brief_log DROP COLUMN send_state, DROP COLUMN send_key, DROP COLUMN body_html;
