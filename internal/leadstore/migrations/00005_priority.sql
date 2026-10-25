-- +goose Up
-- The operator's lead priorities: the profile the queue is scored by, and hand steering of
-- single leads.
--
-- Both tables are append-only (UPDATE revoked from the app role, as on send_pause).
-- priority_profile: every saved version of the profile is a row and the latest (by id)
-- wins; a NULL profile is a reset to leads.yaml's defaults, as is no row at all.
-- lead_priority: every pin, defer and clear of a lead is a row and the latest per lead (by
-- id) wins; a defer hides the lead through its until date.
CREATE TABLE priority_profile (
  id bigserial PRIMARY KEY,
  profile jsonb,                      -- leadconfig.Profile; NULL: back to the file's defaults
  actor text NOT NULL,                -- as lead_event.actor
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT priority_profile_actor_check CHECK (actor IN ('gremy', 'system')),
  CONSTRAINT priority_profile_object_check CHECK (profile IS NULL OR jsonb_typeof(profile) = 'object')
);

CREATE TABLE lead_priority (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  action text NOT NULL,               -- pin | defer | clear
  until date,                         -- defer only: hidden through this day
  actor text NOT NULL,                -- as lead_event.actor
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_priority_action_check CHECK (action IN ('pin', 'defer', 'clear')),
  CONSTRAINT lead_priority_until_check CHECK ((action = 'defer') = (until IS NOT NULL)),
  CONSTRAINT lead_priority_actor_check CHECK (actor IN ('gremy', 'system'))
);

-- The latest row per lead, read once per candidate.
CREATE INDEX lead_priority_lead_idx ON lead_priority (lead_id, id DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE lead_priority;
DROP TABLE priority_profile;
