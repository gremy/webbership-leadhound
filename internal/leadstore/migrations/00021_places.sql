-- +goose Up
-- Places (CP7): the café campaign's source. The operator exports Overture Maps Places rows to
-- NDJSON with DuckDB (README "Café places") and `leads places import overture` stores them.
-- Both new tables are append-only (UPDATE revoked from the app role; DELETE was never
-- granted). Only the owner changes a row, to erase an org (leadstore.ForgetOrg blanks the
-- phones and emails of its places).
--
-- place_import: one import of one file into one campaign, with places.Stats counts and the
--   malformed line numbers only (never a line's text: a row may hold a person's phone), and
--   what the store did: places stored, places already stored for that release, chains
--   skipped, orgs and sites made.
-- place: one Overture place of one release (UNIQUE (source, source_id, release): a release
--   is imported once; a later release adds its own rows, so a place's history is kept).
--   region is Overture's subdivision code as given; region_iso its ISO 3166-2 form ("US-IL",
--   '' when unknown). websites, phones, socials and emails are as Overture lists them: a
--   phone here is a map listing's, not checked (the call card says so; it becomes an
--   org_fact only after the same check as research). chain_rule is the chain check's rule
--   (list | brand | name | domain, '' for an independent café) and chain_detail its plain
--   words; a chain place gets no org and no lead (org_id NULL). An independent place points
--   at its org (org_id): one org per registrable domain of its own website, or, with no
--   website of its own, one org of its own (domain 'overture:<source_id>', no site), so two
--   places on one website share one org.
--
-- org.region: the org's state, province or territory in ISO 3166-2 form ("US-IL"), '' when
-- unknown, fixed at insert (no UPDATE grant): the call rule reads it (compliance OrgFacts
-- Region) for the countries that need one (US, CA, AU). Only a place import sets it.
-- org.source and site.origin gain 'place'.
--
-- Café lead kinds (cafe@1): every one may be drafted once a lead exists (outreach true); a
-- campaign turns a kind on by listing it in its spec's kinds, and the example lists only
-- cafe_no_digital_loyalty (the other two are off by default).
SET LOCAL lock_timeout = '5s';

INSERT INTO lead_kind (name, label, description, outreach) VALUES
  ('cafe_no_digital_loyalty', 'Café, no digital loyalty', 'A café whose own website shows no digital loyalty (on by default)', true),
  ('cafe_has_loyalty', 'Café on a loyalty platform', 'A café whose own website shows a loyalty platform (off by default)', true),
  ('cafe_no_website', 'Café, no website', 'A café with no website of its own on its map listing (off by default)', true);

ALTER TABLE org DROP CONSTRAINT org_source_check;
ALTER TABLE org ADD CONSTRAINT org_source_check CHECK (source IN ('probe', 'agencies-file', 'selftest', 'place'));
ALTER TABLE org ADD COLUMN region text NOT NULL DEFAULT '';
ALTER TABLE org ADD CONSTRAINT org_region_check CHECK (region = '' OR region ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$');

ALTER TABLE site DROP CONSTRAINT site_origin_check;
ALTER TABLE site ADD CONSTRAINT site_origin_check CHECK (origin IN ('majestic', 'tranco', 'httparchive', 'agency', 'manual', 'place'));

CREATE TABLE place_import (
  id bigserial PRIMARY KEY,
  source text NOT NULL,                 -- overture
  release text NOT NULL,                -- Overture's release, e.g. 2026-10-21.0
  campaign_id bigint NOT NULL REFERENCES campaign(id),
  file_name text NOT NULL DEFAULT '',   -- the file's base name, never a path
  lines int NOT NULL,
  kept int NOT NULL,
  blank int NOT NULL,
  malformed int NOT NULL,
  not_cafe int NOT NULL,
  low_confidence int NOT NULL,
  other_country int NOT NULL,
  no_name int NOT NULL,
  malformed_lines int[] NOT NULL DEFAULT '{}',
  stored int NOT NULL,                  -- place rows written
  already int NOT NULL,                 -- places already stored for this release
  chains int NOT NULL,                  -- of the kept places, skipped as chains
  orgs int NOT NULL,                    -- orgs made
  sites int NOT NULL,                   -- sites made
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT place_import_source_check CHECK (source IN ('overture')),
  CONSTRAINT place_import_release_check CHECK (release ~ '^[0-9A-Za-z._-]{1,40}$'),
  CONSTRAINT place_import_file_check CHECK (char_length(file_name) <= 200 AND file_name !~ '[\r\n/\\]'),
  CONSTRAINT place_import_counts_check CHECK (lines >= 0 AND kept >= 0 AND blank >= 0 AND malformed >= 0
    AND not_cafe >= 0 AND low_confidence >= 0 AND other_country >= 0 AND no_name >= 0
    AND lines = kept + blank + malformed + not_cafe + low_confidence + other_country + no_name
    AND stored >= 0 AND already >= 0 AND chains >= 0 AND orgs >= 0 AND sites >= 0),
  CONSTRAINT place_import_malformed_lines_check CHECK (cardinality(malformed_lines) <= 50),
  CONSTRAINT place_import_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX place_import_campaign_idx ON place_import (campaign_id, id DESC);

CREATE TABLE place (
  id bigserial PRIMARY KEY,
  import_id bigint NOT NULL REFERENCES place_import(id),
  source text NOT NULL,                 -- overture
  source_id text NOT NULL,              -- Overture's GERS id
  release text NOT NULL,
  name text NOT NULL,
  category text NOT NULL,
  alternates jsonb NOT NULL DEFAULT '[]',
  country text NOT NULL DEFAULT '',     -- ISO 3166-1 alpha-2
  region text NOT NULL DEFAULT '',      -- as Overture gives it
  region_iso text NOT NULL DEFAULT '',  -- "US-IL"; '' when unknown
  locality text NOT NULL DEFAULT '',
  postcode text NOT NULL DEFAULT '',
  freeform text NOT NULL DEFAULT '',
  lon double precision,                 -- NULL without a point
  lat double precision,
  websites jsonb NOT NULL DEFAULT '[]',
  phones jsonb NOT NULL DEFAULT '[]',   -- a map listing's, not checked
  socials jsonb NOT NULL DEFAULT '[]',
  emails jsonb NOT NULL DEFAULT '[]',
  brand text NOT NULL DEFAULT '',
  brand_wikidata text NOT NULL DEFAULT '',
  sources jsonb NOT NULL DEFAULT '[]',  -- [{dataset, record_id}]
  confidence double precision NOT NULL,
  chain_rule text NOT NULL DEFAULT '',  -- list | brand | name | domain | ''
  chain_detail text NOT NULL DEFAULT '',
  org_id bigint REFERENCES org(id),     -- NULL for a chain
  imported_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT place_source_key UNIQUE (source, source_id, release),
  CONSTRAINT place_source_check CHECK (source IN ('overture')),
  CONSTRAINT place_confidence_check CHECK (confidence >= 0 AND confidence <= 1),
  CONSTRAINT place_region_iso_check CHECK (region_iso = '' OR region_iso ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'),
  CONSTRAINT place_chain_check CHECK (
    (chain_rule = '' AND chain_detail = '' AND org_id IS NOT NULL)
    OR (chain_rule IN ('list', 'brand', 'name', 'domain') AND chain_detail <> '' AND org_id IS NULL)),
  CONSTRAINT place_lists_check CHECK (jsonb_typeof(alternates) = 'array' AND jsonb_typeof(websites) = 'array'
    AND jsonb_typeof(phones) = 'array' AND jsonb_typeof(socials) = 'array' AND jsonb_typeof(emails) = 'array'
    AND jsonb_typeof(sources) = 'array'),
  CONSTRAINT place_point_check CHECK ((lon IS NULL) = (lat IS NULL))
);
CREATE INDEX place_org_idx ON place (org_id, id DESC) WHERE org_id IS NOT NULL;
CREATE INDEX place_import_idx ON place (import_id);

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE, DELETE, TRUNCATE ON public.place_import FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.place FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. An org or site made from a place cannot go back while it exists
-- (its source or origin is outside the old CHECK): the CHECK restore fails and the whole
-- step rolls back, nothing changed. A lead of a café kind does the same through its kind's
-- foreign key.
SET LOCAL lock_timeout = '5s';
DROP TABLE place;
DROP TABLE place_import;
ALTER TABLE site DROP CONSTRAINT site_origin_check;
ALTER TABLE site ADD CONSTRAINT site_origin_check CHECK (origin IN ('majestic', 'tranco', 'httparchive', 'agency', 'manual'));
ALTER TABLE org DROP CONSTRAINT org_region_check;
ALTER TABLE org DROP COLUMN region;
ALTER TABLE org DROP CONSTRAINT org_source_check;
ALTER TABLE org ADD CONSTRAINT org_source_check CHECK (source IN ('probe', 'agencies-file', 'selftest'));
DELETE FROM lead_kind WHERE name IN ('cafe_no_digital_loyalty', 'cafe_has_loyalty', 'cafe_no_website');
