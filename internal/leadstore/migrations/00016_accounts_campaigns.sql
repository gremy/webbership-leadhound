-- +goose Up
-- Accounts and campaigns: the system holds many campaigns, each owned by one account, and
-- today's Drupal flow is campaign 1 of account 1, unchanged.
--
-- account: who owns campaigns, leads, suppressions, mailboxes, batches and settings. Seeded
-- with (1, 'operator', 'Operator'). Rows are fixed: UPDATE revoked from the app role.
-- campaign: one campaign of one account, slug unique per account. Seeded with
-- (1, 1, 'drupal-eol', 'Drupal end-of-life'). Its one state pointer is retired_at.
-- campaign_version: the campaign's spec, append-only (UPDATE revoked), the latest row (by
-- id) wins; a version is written only when the spec's sha256 changed.
--
-- The UPDATE-able columns of the new tables (leadstore.Migrate and deploy/sql/grants.sql
-- swap the table-wide UPDATE for these):
--   campaign.retired_at
-- account and campaign_version take no UPDATE at all. The new account_id and campaign_id
-- columns below are fixed at insert: none of them joins its table's UPDATE-able columns.
--
-- lead.campaign_id (default 1, every existing lead backfilled to 1): the same business can
-- be a lead in two campaigns, so lead_org_id_key UNIQUE (org_id) becomes
-- lead_campaign_org_key UNIQUE (campaign_id, org_id).
-- account_id (default 1) on suppression, sender_mailbox, outreach_batch and lead_setting.
-- Suppression is checked per account across all its campaigns. sender_mailbox.address
-- stays globally unique.
-- org, site, site_probe, org_fact and finding stay shared facts about businesses.
CREATE TABLE account (
  id bigserial PRIMARY KEY,
  slug text NOT NULL,
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT account_slug_key UNIQUE (slug),
  CONSTRAINT account_slug_check CHECK (slug ~ '^[a-z0-9-]{2,40}$'),
  CONSTRAINT account_name_check CHECK (length(btrim(name)) BETWEEN 1 AND 200 AND name !~ '[\r\n]')
);
INSERT INTO account (id, slug, name) VALUES (1, 'operator', 'Operator');
SELECT setval(pg_get_serial_sequence('account', 'id'), (SELECT max(id) FROM account));

CREATE TABLE campaign (
  id bigserial PRIMARY KEY,
  account_id bigint NOT NULL REFERENCES account(id),
  slug text NOT NULL,
  name text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  retired_at timestamptz,               -- NULL: active
  CONSTRAINT campaign_account_slug_key UNIQUE (account_id, slug),
  CONSTRAINT campaign_slug_check CHECK (slug ~ '^[a-z0-9-]{2,40}$'),
  CONSTRAINT campaign_name_check CHECK (length(btrim(name)) BETWEEN 1 AND 200 AND name !~ '[\r\n]')
);
INSERT INTO campaign (id, account_id, slug, name) VALUES (1, 1, 'drupal-eol', 'Drupal end-of-life');
SELECT setval(pg_get_serial_sequence('campaign', 'id'), (SELECT max(id) FROM campaign));

CREATE TABLE campaign_version (
  id bigserial PRIMARY KEY,
  campaign_id bigint NOT NULL REFERENCES campaign(id),
  spec jsonb NOT NULL,
  spec_sha256 text NOT NULL,            -- hex, over the spec file's bytes as registered
  source text NOT NULL,                 -- file | console
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT campaign_version_spec_check CHECK (jsonb_typeof(spec) = 'object'),
  CONSTRAINT campaign_version_sha_check CHECK (spec_sha256 ~ '^[0-9a-f]{64}$'),
  CONSTRAINT campaign_version_source_check CHECK (source IN ('file', 'console')),
  CONSTRAINT campaign_version_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX campaign_version_campaign_idx ON campaign_version (campaign_id, id DESC);

-- Every lead so far is campaign 1's (the default backfills them).
ALTER TABLE lead ADD COLUMN campaign_id bigint NOT NULL DEFAULT 1;
ALTER TABLE lead ADD CONSTRAINT lead_campaign_fk FOREIGN KEY (campaign_id) REFERENCES campaign(id);
ALTER TABLE lead DROP CONSTRAINT lead_org_id_key;
ALTER TABLE lead ADD CONSTRAINT lead_campaign_org_key UNIQUE (campaign_id, org_id);
-- org_id alone lost its unique index; the joins from org to its leads keep one.
CREATE INDEX lead_org_idx ON lead (org_id);

ALTER TABLE suppression ADD COLUMN account_id bigint NOT NULL DEFAULT 1 REFERENCES account(id);
ALTER TABLE sender_mailbox ADD COLUMN account_id bigint NOT NULL DEFAULT 1 REFERENCES account(id);
ALTER TABLE outreach_batch ADD COLUMN account_id bigint NOT NULL DEFAULT 1 REFERENCES account(id);
ALTER TABLE lead_setting ADD COLUMN account_id bigint NOT NULL DEFAULT 1 REFERENCES account(id);
CREATE INDEX suppression_account_value_idx ON suppression (account_id, kind, value);

-- suppression_active is SELECT s.* (00009), and Postgres froze its column list when it was
-- made, so it is recreated to carry account_id. A recreated view gets the owner's default
-- privileges again, so its INSERT and UPDATE are revoked again here (leadstore.Migrate and
-- deploy/sql/grants.sql do the same after every run).
DROP VIEW suppression_active;
CREATE VIEW suppression_active AS
  SELECT s.* FROM suppression s -- reads lifted rows on purpose: this is the one definition of active
  WHERE NOT EXISTS (SELECT 1 FROM suppression_lift l WHERE l.suppression_id = s.id);

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE INSERT, UPDATE ON public.suppression_active FROM leadhound_app;
    REVOKE UPDATE ON public.account FROM leadhound_app;
    REVOKE UPDATE ON public.campaign FROM leadhound_app;
    GRANT UPDATE (retired_at) ON public.campaign TO leadhound_app;
    REVOKE UPDATE ON public.campaign_version FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. What cannot narrow back:
--   - an org with leads in two campaigns cannot go back to one lead per org: restoring
--     lead_org_id_key fails and the whole step rolls back, nothing changed;
--   - every suppression, mailbox, batch and setting of every account becomes one global
--     list again (account_id is dropped), so an opt-out recorded for one account then
--     blocks every send. That is the safe direction and is not undone.
SET LOCAL lock_timeout = '5s';
DROP VIEW suppression_active;
DROP INDEX suppression_account_value_idx;
ALTER TABLE lead_setting DROP COLUMN account_id;
ALTER TABLE outreach_batch DROP COLUMN account_id;
ALTER TABLE sender_mailbox DROP COLUMN account_id;
ALTER TABLE suppression DROP COLUMN account_id;
CREATE VIEW suppression_active AS
  SELECT s.* FROM suppression s
  WHERE NOT EXISTS (SELECT 1 FROM suppression_lift l WHERE l.suppression_id = s.id);
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE INSERT, UPDATE ON public.suppression_active FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX lead_org_idx;
ALTER TABLE lead DROP CONSTRAINT lead_campaign_org_key;
ALTER TABLE lead ADD CONSTRAINT lead_org_id_key UNIQUE (org_id);
ALTER TABLE lead DROP CONSTRAINT lead_campaign_fk;
ALTER TABLE lead DROP COLUMN campaign_id;
DROP TABLE campaign_version;
DROP TABLE campaign;
DROP TABLE account;
