-- +goose Up
-- The CRM after a reply: where each lead stands in the pipeline, the operator's note and next
-- action on it, the deal it is worth, the stage changes for its timeline, the reply's opening
-- lines and the date a call is to be made again.
--
-- lead_crm: at most one row per lead, written by the operator through the console. stage is
-- NULL until the operator sets one (a lead that replied or had a connected call reads as
-- talking without a row: leadstore.GetLeadCRM). lost_reason is required when the stage is
-- lost and cleared when it leaves lost; tenant_url, the lead's Webbership tenant, is an https
-- URL and is held only while the stage is won. Every column is a pointer the operator moves,
-- so the app keeps table-wide UPDATE on it (the default privileges give it; no revoke). The
-- owner alone deletes a row, to erase an org (leadstore.ForgetOrg).
--
-- lead_stage_event: one row per stage change, append-only (UPDATE revoked from the app role:
-- leadstore.Migrate and deploy/sql/grants.sql), for the lead's timeline and the funnel's won
-- count. The owner alone deletes them, to erase an org.
--
-- mailbox_event.snippet: the first plain-text lines of a matched reply, at most 300
-- characters, written once with the event (mailbox_event stays append-only). NULL for every
-- other kind, and for a reply that matched no lead. ForgetOrg blanks it.
--
-- call_outcome.call_back_on: the day the operator means to call the lead again, written once
-- with the outcome (call_outcome stays append-only); NULL when none.
CREATE TABLE lead_crm (
  lead_id bigint PRIMARY KEY REFERENCES lead(id),
  stage text,                           -- talking | proposal | won | lost; NULL before any
  lost_reason text NOT NULL DEFAULT '', -- why it was lost; required when stage is lost
  note text NOT NULL DEFAULT '',
  next_action text NOT NULL DEFAULT '',
  next_action_on date,                  -- the operator's local day the next action is due
  deal_value_cents bigint,              -- NULL when no value is set
  currency text NOT NULL DEFAULT 'GBP',
  tenant_url text,                      -- the won lead's Webbership tenant; NULL otherwise
  snoozed_until timestamptz,            -- hidden from the inbox and Needs me until then
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_crm_stage_check CHECK (stage IN ('talking', 'proposal', 'won', 'lost')),
  CONSTRAINT lead_crm_lost_reason_check CHECK (char_length(lost_reason) <= 200 AND lost_reason !~ '[\r\n]'
    AND (stage IS DISTINCT FROM 'lost' OR btrim(lost_reason) <> '')),
  CONSTRAINT lead_crm_note_check CHECK (char_length(note) <= 2000),
  CONSTRAINT lead_crm_next_action_check CHECK (char_length(next_action) <= 200 AND next_action !~ '[\r\n]'),
  CONSTRAINT lead_crm_deal_value_check CHECK (deal_value_cents IS NULL OR deal_value_cents >= 0),
  CONSTRAINT lead_crm_currency_check CHECK (currency IN ('GBP', 'EUR', 'RON', 'USD')),
  CONSTRAINT lead_crm_tenant_url_check CHECK (tenant_url IS NULL OR (coalesce(stage, '') = 'won'
    AND tenant_url ~ '^https://[^\s/?#]+[^\s]*$' AND char_length(tenant_url) <= 500))
);
CREATE INDEX lead_crm_stage_idx ON lead_crm (stage) WHERE stage IS NOT NULL;

CREATE TABLE lead_stage_event (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  stage text NOT NULL,
  reason text NOT NULL DEFAULT '',      -- the lost reason when stage is lost; '' otherwise
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_stage_event_stage_check CHECK (stage IN ('talking', 'proposal', 'won', 'lost')),
  CONSTRAINT lead_stage_event_reason_check CHECK (char_length(reason) <= 200)
);
CREATE INDEX lead_stage_event_lead_idx ON lead_stage_event (lead_id, at DESC, id DESC);
CREATE INDEX lead_stage_event_at_idx ON lead_stage_event (at) WHERE stage = 'won';

ALTER TABLE mailbox_event ADD COLUMN snippet text
  CONSTRAINT mailbox_event_snippet_check CHECK (char_length(snippet) <= 300);
CREATE INDEX mailbox_event_reply_lead_idx ON mailbox_event (lead_id, seen_at DESC)
  WHERE kind = 'reply' AND lead_id IS NOT NULL;

ALTER TABLE call_outcome ADD COLUMN call_back_on date;

-- +goose Down
-- Runs as the owner only; the app role cannot drop. Every CRM row, stage change, reply snippet
-- and call-back date goes with them.
ALTER TABLE call_outcome DROP COLUMN call_back_on;
DROP INDEX mailbox_event_reply_lead_idx;
ALTER TABLE mailbox_event DROP COLUMN snippet;
DROP TABLE lead_stage_event;
DROP TABLE lead_crm;
