-- +goose Up
-- The mixed sequence: up to ten steps per lead, call steps next to email steps, the
-- outcome of every call, and the email consent a call can win.
--
-- outreach_message:
--   step 0..9 (was 0..2), channel gains 'phone' (a call step; leads send never sends one).
--   condition: when the step runs, one of always | has_callable_phone | call_not_connected
--     | email_consent (default 'always'; every existing row is 'always').
--   phone_fact_id: the org_fact phone number a call step dials; only on phone rows, and
--     ON DELETE SET NULL so the erasure sweep can delete a phone fact.
--   sequence_name: which named sequence of its batch this row follows (default |
--     call_first | ...). NULL on rows made before 00017 (the legacy 3-step rule).
--   A cancelled step's reason already lives in cancel_reason (00003, NOT NULL DEFAULT ''),
--     which is UPDATE-able: no new column. New reasons (condition, call_connected,
--     wrong_number, call_skipped, no_consent, ...) are plain values in it.
-- outreach_batch:
--   sequence: the campaign's sequences frozen at batch time, a JSON object mapping a
--     sequence name to its steps ({"default": [...], "call_first": [...]}). NULL = a batch
--     made before 00017, which follows the legacy 3-step rule. Every new batch stores it,
--     the legacy campaign's three email steps included, so a later spec change never
--     changes a batch already approved.
--   campaign_id: the campaign the batch drafts for (default 1, every existing batch
--     backfilled to 1).
--
-- Per-lead sequence choice (the design picked): a batch may hold leads on different named
-- sequences, so each outreach_message row carries its sequence_name and the batch carries
-- the sequences themselves. "Which sequence does this lead's row follow" is then
-- outreach_batch.sequence -> outreach_message.sequence_name, read from the row alone; all
-- rows of one lead in one batch carry the same name (the batch writer's rule). This was
-- chosen over a per-batch lead_id -> name map because a row answers it without a second
-- lookup, and a row's name is fixed at insert like its step and channel.
--
-- call_outcome: what happened on one call step, append-only (UPDATE revoked), the latest
--   row per message (by id) wins. email_consent and consent_address go together: ticked
--   only on a connected call, and then the address they gave or confirmed.
-- email_consent: an org agreed on a call to get email from one campaign, append-only. The
--   wording is the fixed sentence the call card showed the operator to say. A consent is
--   withdrawn by any opt-out (a suppression), never by changing this row.
--
-- suppression.kind gains 'phone' and reason gains do_not_call | tps | ctps (phone only).
-- A phone suppression's value is 'sha256:' + the lower-case hex sha256 of the normalised
-- number (org_fact.normalized: + and 8 to 15 digits), so a suppression never holds the
-- number itself; lookups hash the candidate the same way. suppression_active (SELECT s.*)
-- keeps its columns, so it is not recreated.
--
-- The UPDATE-able pointer columns (leadstore.Migrate and deploy/sql/grants.sql grant UPDATE
-- on these only; unchanged from 00003, 00004 and 00007):
--   outreach_message.state, .due_at, .sent_at, .smtp_reply, .cancel_reason,
--     .approved_sha256, .hard_bounce
--   outreach_batch.approved_at, .approved_hash, .discarded_at
-- Every column added here (outreach_message.condition, .phone_fact_id, .sequence_name,
-- outreach_batch.sequence, .campaign_id) is fixed at insert and joins no UPDATE grant;
-- phone_fact_id is cleared only by its foreign key's ON DELETE SET NULL, run as the owner.
-- call_outcome and email_consent take no UPDATE at all.

ALTER TABLE outreach_message DROP CONSTRAINT outreach_message_step_check;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_step_check CHECK (step BETWEEN 0 AND 9);
ALTER TABLE outreach_message DROP CONSTRAINT outreach_message_channel_check;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_channel_check
  CHECK (channel IN ('email', 'form', 'linkedin', 'phone'));
ALTER TABLE outreach_message ADD COLUMN condition text NOT NULL DEFAULT 'always';
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_condition_check
  CHECK (condition IN ('always', 'has_callable_phone', 'call_not_connected', 'email_consent'));
ALTER TABLE outreach_message ADD COLUMN phone_fact_id bigint REFERENCES org_fact(id) ON DELETE SET NULL;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_phone_fact_check
  CHECK (phone_fact_id IS NULL OR channel = 'phone');
ALTER TABLE outreach_message ADD COLUMN sequence_name text;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_sequence_name_check
  CHECK (sequence_name IS NULL OR sequence_name ~ '^[a-z0-9_-]{1,40}$');
-- The erasure sweep finds the call rows of a phone fact it deletes.
CREATE INDEX outreach_message_phone_fact_idx ON outreach_message (phone_fact_id) WHERE phone_fact_id IS NOT NULL;

ALTER TABLE outreach_batch ADD COLUMN sequence jsonb;
ALTER TABLE outreach_batch ADD CONSTRAINT outreach_batch_sequence_check
  CHECK (sequence IS NULL OR jsonb_typeof(sequence) = 'object');
-- Every batch so far is campaign 1's (the default backfills them).
ALTER TABLE outreach_batch ADD COLUMN campaign_id bigint NOT NULL DEFAULT 1 REFERENCES campaign(id);
CREATE INDEX outreach_batch_campaign_idx ON outreach_batch (campaign_id, id DESC);

CREATE TABLE call_outcome (
  id bigserial PRIMARY KEY,
  message_id bigint NOT NULL REFERENCES outreach_message(id),
  outcome text NOT NULL,
  note text NOT NULL DEFAULT '',
  email_consent boolean NOT NULL DEFAULT false,
  consent_address text,                 -- lower case; set only with email_consent
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT call_outcome_outcome_check
    CHECK (outcome IN ('connected', 'no_answer', 'voicemail', 'wrong_number', 'do_not_call', 'skipped')),
  CONSTRAINT call_outcome_note_check CHECK (char_length(note) <= 500),
  CONSTRAINT call_outcome_consent_check CHECK (
    (email_consent AND outcome = 'connected' AND consent_address IS NOT NULL)
    OR (NOT email_consent AND consent_address IS NULL)),
  CONSTRAINT call_outcome_consent_address_check CHECK (consent_address IS NULL OR (
    char_length(consent_address) BETWEEN 3 AND 320 AND consent_address ~ '^[^@\s]+@[^@\s]+$'
    AND consent_address = lower(consent_address))),
  CONSTRAINT call_outcome_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX call_outcome_message_idx ON call_outcome (message_id, id DESC);

CREATE TABLE email_consent (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  campaign_id bigint NOT NULL REFERENCES campaign(id),
  address text NOT NULL,                -- lower case
  call_outcome_id bigint NOT NULL REFERENCES call_outcome(id),
  wording text NOT NULL,                -- the sentence the operator said
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT email_consent_address_check CHECK (
    char_length(address) BETWEEN 3 AND 320 AND address ~ '^[^@\s]+@[^@\s]+$' AND address = lower(address)),
  CONSTRAINT email_consent_wording_check CHECK (length(btrim(wording)) BETWEEN 1 AND 500 AND wording !~ '[\r\n]'),
  CONSTRAINT email_consent_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX email_consent_org_campaign_idx ON email_consent (org_id, campaign_id, id DESC);
CREATE INDEX email_consent_call_outcome_idx ON email_consent (call_outcome_id);

ALTER TABLE suppression DROP CONSTRAINT suppression_kind_check;
ALTER TABLE suppression ADD CONSTRAINT suppression_kind_check CHECK (kind IN ('address', 'domain', 'phone'));
ALTER TABLE suppression DROP CONSTRAINT suppression_reason_check;
ALTER TABLE suppression ADD CONSTRAINT suppression_reason_check
  CHECK (reason IN ('opt_out', 'bounce', 'replied_no', 'manual', 'do_not_call', 'tps', 'ctps'));
-- A phone suppression holds a hash, never the number; the phone reasons are phone-only.
ALTER TABLE suppression ADD CONSTRAINT suppression_phone_value_check
  CHECK (kind <> 'phone' OR value ~ '^sha256:[0-9a-f]{64}$');
ALTER TABLE suppression ADD CONSTRAINT suppression_phone_reason_check
  CHECK (reason NOT IN ('do_not_call', 'tps', 'ctps') OR kind = 'phone');

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE ON public.call_outcome FROM leadhound_app;
    REVOKE UPDATE ON public.email_consent FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. What cannot narrow back:
--   - the step and channel CHECKs: once a row with step 3..9 or channel 'phone' exists,
--     restoring step 0..2 or the old channel list fails and the whole step rolls back,
--     nothing changed;
--   - the suppression kind and reason CHECKs: once a phone suppression (or a do_not_call,
--     tps or ctps reason) exists, restoring them fails the same way. A phone suppression is
--     never deleted to make the Down pass; that would let a do-not-call number be called.
--   - every call outcome and email consent is dropped with its table, and each batch's
--     frozen sequences, its campaign and each row's sequence name go with their columns.
SET LOCAL lock_timeout = '5s';
ALTER TABLE suppression DROP CONSTRAINT suppression_phone_reason_check;
ALTER TABLE suppression DROP CONSTRAINT suppression_phone_value_check;
ALTER TABLE suppression DROP CONSTRAINT suppression_reason_check;
ALTER TABLE suppression ADD CONSTRAINT suppression_reason_check CHECK (reason IN ('opt_out', 'bounce', 'replied_no', 'manual'));
ALTER TABLE suppression DROP CONSTRAINT suppression_kind_check;
ALTER TABLE suppression ADD CONSTRAINT suppression_kind_check CHECK (kind IN ('address', 'domain'));
DROP TABLE email_consent;
DROP TABLE call_outcome;
DROP INDEX outreach_batch_campaign_idx;
ALTER TABLE outreach_batch DROP COLUMN campaign_id;
ALTER TABLE outreach_batch DROP COLUMN sequence;
DROP INDEX outreach_message_phone_fact_idx;
ALTER TABLE outreach_message DROP COLUMN sequence_name;
ALTER TABLE outreach_message DROP COLUMN phone_fact_id;
ALTER TABLE outreach_message DROP COLUMN condition;
ALTER TABLE outreach_message DROP CONSTRAINT outreach_message_channel_check;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_channel_check CHECK (channel IN ('email', 'form', 'linkedin'));
ALTER TABLE outreach_message DROP CONSTRAINT outreach_message_step_check;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_step_check CHECK (step BETWEEN 0 AND 2);
