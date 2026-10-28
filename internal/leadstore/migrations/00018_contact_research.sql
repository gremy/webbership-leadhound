-- +goose Up
-- Contact research (CP3b): starring a lead whose org has no usable route queues a search
-- for a published contact, which the research job runs and finishes.
--
-- contact_research: one research attempt for one org, asked for through one of its leads.
--   trigger: what asked for it: star (the operator starred a lead with no usable route),
--     button (the lead page's "Look again") or cli (leads research request).
--   status: queued (asked, not yet taken) -> looking (the job took it; started_at set) ->
--     found | nothing | capped | error (finished_at set). capped: the spend cap refused
--     before any call; found: at least one candidate verified; nothing: none verified.
--   want: what to look for, a non-empty subset of {email, phone}, decided from the lead's
--     country rule by the caller (leadstore.WantFor).
--   model, cost_micros, candidates, verified: what the run used and found. cost_micros is
--     the run's priced spend (the ai_usage rows hold the per-lead ledger; claude.Capped
--     returns no usage row id, so none is linked); NULL until finished or when unknown.
-- At most one queued or looking row per org (contact_research_open_key). At most one
-- attempt per org per 90 days, counted from an attempt that ran: leadstore.RequestResearch
-- enforces that window (a queued row that never ran does not count).
--
-- The UPDATE-able columns (leadstore.Migrate and deploy/sql/grants.sql grant UPDATE on
-- these only):
--   contact_research.status, .model, .cost_micros, .candidates, .verified, .started_at,
--     .finished_at
-- org_id, lead_id, campaign_id, trigger, want, requested_at and actor are fixed at insert.
CREATE TABLE contact_research (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  lead_id bigint NOT NULL REFERENCES lead(id),
  campaign_id bigint NOT NULL REFERENCES campaign(id),
  trigger text NOT NULL,                -- star | button | cli
  status text NOT NULL DEFAULT 'queued',
  want text[] NOT NULL,                 -- subset of {email, phone}
  model text NOT NULL DEFAULT '',
  cost_micros bigint,                   -- NULL: not finished, or unknown
  candidates int NOT NULL DEFAULT 0,
  verified int NOT NULL DEFAULT 0,
  requested_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,               -- set when the job takes it
  finished_at timestamptz,              -- set when it ends
  actor text NOT NULL,                  -- operator | system
  CONSTRAINT contact_research_trigger_check CHECK (trigger IN ('star', 'button', 'cli')),
  CONSTRAINT contact_research_status_check
    CHECK (status IN ('queued', 'looking', 'found', 'nothing', 'capped', 'error')),
  CONSTRAINT contact_research_want_check
    CHECK (cardinality(want) BETWEEN 1 AND 2 AND want <@ ARRAY['email', 'phone']::text[]
      AND array_position(want, NULL) IS NULL),
  CONSTRAINT contact_research_model_check CHECK (char_length(model) <= 100 AND model !~ '[\r\n]'),
  CONSTRAINT contact_research_cost_check CHECK (cost_micros IS NULL OR cost_micros >= 0),
  CONSTRAINT contact_research_counts_check CHECK (candidates >= 0 AND verified BETWEEN 0 AND candidates),
  -- found means something verified, nothing means nothing did.
  CONSTRAINT contact_research_found_check CHECK (
    (status <> 'found' OR verified > 0) AND (status <> 'nothing' OR verified = 0)),
  -- The times follow the status: queued has neither, looking has started_at only, a
  -- finished row has finished_at (and started_at, which the job set when it took it).
  CONSTRAINT contact_research_times_check CHECK (
    (status = 'queued' AND started_at IS NULL AND finished_at IS NULL)
    OR (status = 'looking' AND started_at IS NOT NULL AND finished_at IS NULL)
    OR (status IN ('found', 'nothing', 'capped', 'error') AND started_at IS NOT NULL
      AND finished_at IS NOT NULL AND finished_at >= started_at)),
  CONSTRAINT contact_research_actor_check CHECK (actor IN ('operator', 'system'))
);

-- A queued or looking row is never duplicated for one org.
CREATE UNIQUE INDEX contact_research_open_key ON contact_research (org_id) WHERE status IN ('queued', 'looking');
-- The latest attempts of an org (the 90-day window, the lead page).
CREATE INDEX contact_research_org_idx ON contact_research (org_id, id DESC);
-- The job's queue, oldest first.
CREATE INDEX contact_research_queued_idx ON contact_research (requested_at, id) WHERE status = 'queued';
CREATE INDEX contact_research_lead_idx ON contact_research (lead_id);

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE ON public.contact_research FROM leadhound_app;
    GRANT UPDATE (status, model, cost_micros, candidates, verified, started_at, finished_at)
      ON public.contact_research TO leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. Every research attempt goes with its table; the routes and facts
-- a run stored (derived_by 'claude-research@1') stay, as any finder's do.
SET LOCAL lock_timeout = '5s';
DROP TABLE contact_research;
