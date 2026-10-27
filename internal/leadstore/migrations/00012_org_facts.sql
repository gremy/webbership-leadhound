-- +goose Up
-- Facts read from an org's own pages (enrichment): its phone numbers, its description and its
-- social profiles, each with the fetch it came from.
--
-- org_fact and enrich_attempt are append-only (UPDATE revoked from the app role, as on
-- contact_route and contact_attempt). An org keeps one row per fact (kind and normalized
-- value); a fact seen again is not written again. A phone number is a fact and never a
-- contact_route: routes are what drafting and sending write to.
-- enrich_attempt is one enrichment of an org, written whether or not it found a fact, so an
-- org enriched recently is not enriched again.
CREATE TABLE org_fact (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  kind text NOT NULL,                 -- phone | description | social
  value text NOT NULL,                -- as shown on the page, cleaned
  normalized text NOT NULL,           -- phone: + and 8 to 15 digits; social: the profile URL; description: the value
  fetch_id bigint NOT NULL REFERENCES http_fetch(id), -- the page it was read from
  derived_by text NOT NULL,           -- 'enrich@1'
  found_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_fact_org_kind_normalized_key UNIQUE (org_id, kind, normalized),
  CONSTRAINT org_fact_kind_check CHECK (kind IN ('phone', 'description', 'social')),
  CONSTRAINT org_fact_value_check CHECK (char_length(value) BETWEEN 1 AND 300),
  CONSTRAINT org_fact_normalized_check CHECK (char_length(normalized) BETWEEN 1 AND 300),
  CONSTRAINT org_fact_phone_check CHECK (kind <> 'phone' OR normalized ~ '^\+?[0-9]{8,15}$')
);

CREATE TABLE enrich_attempt (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  run_id bigint NOT NULL REFERENCES run(id),
  facts_found int NOT NULL DEFAULT 0,
  attempted_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT enrich_attempt_facts_found_check CHECK (facts_found >= 0)
);

-- An org's facts and its latest attempts.
CREATE INDEX org_fact_org_idx ON org_fact (org_id, id DESC);
CREATE INDEX enrich_attempt_org_idx ON enrich_attempt (org_id, id DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE enrich_attempt;
DROP TABLE org_fact;
