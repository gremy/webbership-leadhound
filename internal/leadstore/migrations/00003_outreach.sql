-- +goose Up
-- Leads, outreach batches and messages, suppression, and the outreach mailbox's state.
--
-- Facts are append-only. The UPDATEd state pointers, the only columns the app role may
-- UPDATE (leadstore.Migrate and deploy/sql/grants.sql swap the table-wide UPDATE for these):
--   lead.state, lead.state_at
--   lead.kind, .probe_id, .agency_finding_id, .channel, .route_id: the finding, channel and
--     route follow the latest facts while the lead is still new (RefreshLead), never after
--   outreach_batch.approved_at, outreach_batch.approved_hash
--   outreach_message.state, .due_at, .sent_at, .smtp_reply, .cancel_reason, .approved_sha256
--   mailbox_cursor.* (one row per mailbox, moved forward by every sync)
-- Append-only (UPDATE revoked from the app role): lead_event, suppression, mailbox_event,
-- sender_check. The app role has no DELETE, so a suppression is never lifted.

-- lead: one per organisation, ever. state is its pointer; lead_event holds the history.
CREATE TABLE lead (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  kind text NOT NULL,                 -- unsupported | d10 | agency_partner | selftest | hand
  probe_id bigint REFERENCES site_probe(id),              -- the finding (site leads)
  agency_finding_id bigint REFERENCES agency_finding(id), -- the finding (agency leads)
  channel text NOT NULL,              -- email | form | none
  route_id bigint REFERENCES contact_route(id),
  state text NOT NULL DEFAULT 'new',
  state_at timestamptz NOT NULL DEFAULT now(),
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_org_id_key UNIQUE (org_id),
  CONSTRAINT lead_kind_check CHECK (kind IN ('unsupported', 'd10', 'agency_partner', 'selftest', 'hand')),
  CONSTRAINT lead_channel_check CHECK (channel IN ('email', 'form', 'none')),
  CONSTRAINT lead_state_check CHECK (state IN ('new', 'batched', 'contacted', 'replied', 'bounced', 'opted_out', 'suppressed', 'skipped', 'closed')),
  -- a real lead has exactly one finding; selftest and hand-contacted leads have none
  CONSTRAINT lead_finding_check CHECK (kind IN ('selftest', 'hand') OR ((probe_id IS NOT NULL) <> (agency_finding_id IS NOT NULL)))
);

-- lead_event: every state a lead has entered, and who moved it.
CREATE TABLE lead_event (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  state text NOT NULL,
  reason text NOT NULL DEFAULT '',
  actor text NOT NULL,                -- gremy | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_event_actor_check CHECK (actor IN ('gremy', 'system'))
);

-- outreach_batch: one set of drafts the operator reads and approves by its content hash.
CREATE TABLE outreach_batch (
  id bigserial PRIMARY KEY,
  shadow boolean NOT NULL,            -- a shadow batch never changes lead.state and never goes live
  created_at timestamptz NOT NULL DEFAULT now(),
  content_sha256 bytea NOT NULL,      -- over every draft in the batch, canonical order
  approved_at timestamptz,
  approved_hash bytea,                -- equals content_sha256 at approval (the --hash prefix matched it)
  templates_sha256 bytea NOT NULL     -- provenance only
);

-- outreach_message: one step of one lead's sequence in one batch.
CREATE TABLE outreach_message (
  id bigserial PRIMARY KEY,
  batch_id bigint NOT NULL REFERENCES outreach_batch(id),
  lead_id bigint NOT NULL REFERENCES lead(id),
  step int NOT NULL,
  channel text NOT NULL,                -- email | form | linkedin
  to_address text NOT NULL DEFAULT '',  -- email channel only
  subject text NOT NULL DEFAULT '',
  body_text text NOT NULL,
  message_id text NOT NULL DEFAULT '',  -- generated at draft time for email; stable across retries
  in_reply_to text NOT NULL DEFAULT '',
  slots jsonb NOT NULL,                 -- slot name -> {value, source table, source id}
  approved_sha256 bytea,                -- sha256(to, subject, body, in_reply_to) at approval; send re-checks it
  state text NOT NULL DEFAULT 'draft',
  due_at timestamptz,
  sent_at timestamptz,
  smtp_reply text NOT NULL DEFAULT '',
  cancel_reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT outreach_message_lead_step_batch_key UNIQUE (lead_id, step, batch_id),
  CONSTRAINT outreach_message_step_check CHECK (step BETWEEN 0 AND 2),
  CONSTRAINT outreach_message_channel_check CHECK (channel IN ('email', 'form', 'linkedin')),
  CONSTRAINT outreach_message_state_check CHECK (state IN ('draft', 'approved', 'skipped', 'cancelled', 'sending', 'sent', 'shadow_sent', 'refused', 'maybe_sent', 'done'))
);

-- suppression: an address or a domain never written to again. Never lifted.
CREATE TABLE suppression (
  id bigserial PRIMARY KEY,
  kind text NOT NULL,                   -- address | domain
  value text NOT NULL,                  -- lower case
  reason text NOT NULL,                 -- opt_out | bounce | replied_no | manual
  source text NOT NULL DEFAULT '',      -- e.g. 'mailbox:INBOX:<uid>', 'cli', 'hand-contacted'
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT suppression_kind_check CHECK (kind IN ('address', 'domain')),
  CONSTRAINT suppression_reason_check CHECK (reason IN ('opt_out', 'bounce', 'replied_no', 'manual'))
);

-- mailbox_event: one message seen in the outreach mailbox; the domain only, no address, no body.
CREATE TABLE mailbox_event (
  id bigserial PRIMARY KEY,
  mailbox text NOT NULL,                -- 'INBOX' | the \Junk mailbox's name (UIDs are per mailbox)
  uidvalidity bigint NOT NULL,
  uid bigint NOT NULL,
  kind text NOT NULL,                   -- reply | auto_reply | bounce | opt_out | unmatched
  from_domain text NOT NULL DEFAULT '', -- the registrable domain only
  matched_message_id bigint REFERENCES outreach_message(id),
  lead_id bigint REFERENCES lead(id),
  seen_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT mailbox_event_mailbox_uid_key UNIQUE (mailbox, uidvalidity, uid),
  CONSTRAINT mailbox_event_kind_check CHECK (kind IN ('reply', 'auto_reply', 'bounce', 'opt_out', 'unmatched'))
);

-- mailbox_cursor: how far each mailbox has been read, one row each.
CREATE TABLE mailbox_cursor (
  mailbox text PRIMARY KEY,             -- 'INBOX' and the \Junk mailbox
  uidvalidity bigint NOT NULL,
  last_uid bigint NOT NULL,
  synced_at timestamptz NOT NULL
);

-- sender_check: one SPF/DKIM/DMARC check of the sending domain.
CREATE TABLE sender_check (
  id bigserial PRIMARY KEY,
  domain_sha256 bytea NOT NULL,         -- the sending domain stays out of rows that could be dumped into a fixture
  spf boolean NOT NULL,
  dkim boolean NOT NULL,
  dmarc boolean NOT NULL,
  note text NOT NULL DEFAULT '',
  checked_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX outreach_message_due_idx ON outreach_message (state, due_at);
CREATE INDEX suppression_value_idx ON suppression (kind, value);
CREATE INDEX lead_state_idx ON lead (state);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE sender_check;
DROP TABLE mailbox_cursor;
DROP TABLE mailbox_event;
DROP TABLE suppression;
DROP TABLE outreach_message;
DROP TABLE outreach_batch;
DROP TABLE lead_event;
DROP TABLE lead;
