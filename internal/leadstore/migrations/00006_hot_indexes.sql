-- +goose NO TRANSACTION
-- +goose Up
-- Indexes for the lead tables' date and batch lookups: the probe summary and the brief's
-- probe line by when a probe was made, the brief's replies and other mail by when a message
-- was seen, and a batch's messages by batch (the unique key leads with the lead, so a batch
-- read scanned the whole index).
-- Each is built concurrently, so a live database keeps taking writes while it builds; that
-- cannot run in a transaction, hence NO TRANSACTION and one statement per line. A concurrent
-- build that fails leaves an invalid index behind, so each CREATE is preceded by a DROP of the
-- same name and a retry rebuilds it. The lock timeout stops a migrate that cannot get its
-- lock from queueing every reader of the table behind it; it fails, and a rerun retries.
SET lock_timeout = '5s';

DROP INDEX CONCURRENTLY IF EXISTS site_probe_created_idx;
CREATE INDEX CONCURRENTLY site_probe_created_idx ON site_probe (created_at);

DROP INDEX CONCURRENTLY IF EXISTS mailbox_event_seen_idx;
CREATE INDEX CONCURRENTLY mailbox_event_seen_idx ON mailbox_event (seen_at);

DROP INDEX CONCURRENTLY IF EXISTS outreach_message_batch_idx;
CREATE INDEX CONCURRENTLY outreach_message_batch_idx ON outreach_message (batch_id);

-- The session goes back to the pool after the migration, so it does not keep the timeout.
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS outreach_message_batch_idx;
DROP INDEX CONCURRENTLY IF EXISTS mailbox_event_seen_idx;
DROP INDEX CONCURRENTLY IF EXISTS site_probe_created_idx;
