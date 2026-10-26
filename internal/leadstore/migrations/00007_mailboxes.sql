-- +goose Up
-- Sending mailboxes: any number of them, each with its own address, servers, DKIM selector
-- and sealed password, its own state history and its own guided checks.
--
-- Additive only: new tables, and new columns on outreach_message, mailbox_event,
-- mailbox_cursor and sender_check that are nullable or carry a default, so every existing
-- row and query keeps working. A later migration tightens the keys and the NOT NULLs.
--
-- sender_mailbox: one row per sending address, ever. The address (and so its domain) is
-- fixed; the app role may UPDATE only login, smtp_addr, imap_addr, dkim_selector, from_name,
-- password_sealed and sealed_by (leadstore.Migrate and deploy/sql/grants.sql). The password
-- is never stored in the clear: password_sealed holds it sealed with the operator's public
-- key, and sealed_by names that key's fingerprint.
-- mailbox_state is append-only (UPDATE revoked from the app role, as on send_pause): every
-- pause, resume and retire is a row, the latest row (by id) of a mailbox is its state, and
-- no row means active.
-- mailbox_check: one guided check of a mailbox, requested by the console or the command
-- line and filled in by the job that runs it; its result columns are its state pointers.
-- outreach_message.mailbox_id and .from_name are set at insert and stay fixed, so an
-- approved message can never be moved to another mailbox; .hard_bounce is a pointer.
-- New tables name their actor operator | system.
CREATE TABLE sender_mailbox (
  id bigserial PRIMARY KEY,
  address text NOT NULL,
  domain text NOT NULL,                 -- the registrable domain of the address
  login text NOT NULL,
  smtp_addr text NOT NULL,              -- host:port
  imap_addr text NOT NULL,              -- host:port
  dkim_selector text NOT NULL,
  from_name text NOT NULL DEFAULT '',   -- empty: leads.yaml sender.name
  password_sealed bytea NOT NULL,
  sealed_by text NOT NULL,              -- the fingerprint of the public key that sealed it
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT sender_mailbox_address_key UNIQUE (address),
  CONSTRAINT sender_mailbox_address_lower_check CHECK (address = lower(address)),
  CONSTRAINT sender_mailbox_smtp_port_check CHECK (smtp_addr ~ ':(465|587)$'),
  CONSTRAINT sender_mailbox_imap_port_check CHECK (imap_addr ~ ':993$'),
  CONSTRAINT sender_mailbox_from_name_check CHECK (from_name !~ '[\r\n]')
);

CREATE TABLE mailbox_state (
  id bigserial PRIMARY KEY,
  mailbox_id bigint NOT NULL REFERENCES sender_mailbox(id),
  state text NOT NULL,
  reason text NOT NULL DEFAULT '',
  scope text NOT NULL DEFAULT 'mailbox', -- mailbox | domain (one row per mailbox of the domain)
  actor text NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT mailbox_state_state_check CHECK (state IN ('active', 'paused', 'retired')),
  CONSTRAINT mailbox_state_scope_check CHECK (scope IN ('mailbox', 'domain')),
  CONSTRAINT mailbox_state_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX mailbox_state_mailbox_idx ON mailbox_state (mailbox_id, id DESC);

CREATE TABLE mailbox_check (
  id bigserial PRIMARY KEY,
  mailbox_id bigint NOT NULL REFERENCES sender_mailbox(id),
  requested_at timestamptz NOT NULL DEFAULT now(),
  actor text NOT NULL,
  checked_at timestamptz,
  smtp_ok boolean,                      -- each NULL until checked, or when that step never ran
  imap_ok boolean,
  folders_ok boolean,
  spf boolean,
  dkim boolean,
  dmarc boolean,
  fix text NOT NULL DEFAULT '',         -- what to do about a failure, in plain words
  outcome text NOT NULL DEFAULT 'requested',
  CONSTRAINT mailbox_check_actor_check CHECK (actor IN ('operator', 'system')),
  CONSTRAINT mailbox_check_outcome_check CHECK (outcome IN ('requested', 'passed', 'failed', 'did_not_run'))
);
CREATE INDEX mailbox_check_mailbox_idx ON mailbox_check (mailbox_id, id DESC);
CREATE INDEX mailbox_check_requested_idx ON mailbox_check (id) WHERE outcome = 'requested';

ALTER TABLE outreach_message ADD COLUMN mailbox_id bigint REFERENCES sender_mailbox(id);
ALTER TABLE outreach_message ADD COLUMN from_name text NOT NULL DEFAULT '';
ALTER TABLE outreach_message ADD COLUMN hard_bounce boolean NOT NULL DEFAULT false;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_from_name_check CHECK (from_name !~ '[\r\n]');
ALTER TABLE mailbox_event ADD COLUMN mailbox_id bigint REFERENCES sender_mailbox(id);
ALTER TABLE mailbox_cursor ADD COLUMN mailbox_id bigint REFERENCES sender_mailbox(id);
ALTER TABLE sender_check ADD COLUMN selector text NOT NULL DEFAULT '';
CREATE INDEX outreach_message_mailbox_idx ON outreach_message (mailbox_id, state, sent_at);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP INDEX outreach_message_mailbox_idx;
ALTER TABLE sender_check DROP COLUMN selector;
ALTER TABLE mailbox_cursor DROP COLUMN mailbox_id;
ALTER TABLE mailbox_event DROP COLUMN mailbox_id;
ALTER TABLE outreach_message DROP CONSTRAINT outreach_message_from_name_check;
ALTER TABLE outreach_message DROP COLUMN hard_bounce;
ALTER TABLE outreach_message DROP COLUMN from_name;
ALTER TABLE outreach_message DROP COLUMN mailbox_id;
DROP TABLE mailbox_check;
DROP TABLE mailbox_state;
DROP TABLE sender_mailbox;
