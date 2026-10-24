-- +goose Up
-- The leads migration set, versioned in goose_db_version_leads. It references the core
-- tables run and http_fetch and never alters them.
--
-- Facts are append-only. The only UPDATEs are on state pointers: site.last_probe_id and
-- site.state. The app role has no DELETE (deploy/sql/grants.sql), and leadstore.Migrate
-- revokes UPDATE on site_probe and fetch_header.

-- site: one row per registrable domain the site finder has listed.
CREATE TABLE site (
  id bigserial PRIMARY KEY,
  domain text NOT NULL,               -- registrable domain (eTLD+1), lower case
  country text NOT NULL DEFAULT '',   -- ISO 3166-1 alpha-2 from the TLD, '' when generic
  origin text NOT NULL,               -- majestic | tranco | httparchive | agency | manual
  list_rank int,                      -- Tranco rank when known
  added_at timestamptz NOT NULL DEFAULT now(),
  state text NOT NULL DEFAULT 'new',
  last_probe_id bigint,               -- FK added below
  CONSTRAINT site_domain_key UNIQUE (domain),
  CONSTRAINT site_origin_check CHECK (origin IN ('majestic', 'tranco', 'httparchive', 'agency', 'manual')),
  CONSTRAINT site_state_check CHECK (state IN ('new', 'probed', 'retired'))
);

-- site_probe: one row per probe of a site, with the Drupal signals it saw.
CREATE TABLE site_probe (
  id bigserial PRIMARY KEY,
  site_id bigint NOT NULL REFERENCES site(id),
  run_id bigint NOT NULL REFERENCES run(id),
  outcome text NOT NULL,              -- ok | bot_check | robots_disallowed | not_html | http_error | transport_error
  final_host text NOT NULL DEFAULT '',
  is_drupal boolean NOT NULL DEFAULT false,
  major int NOT NULL DEFAULT 0,       -- 0 = unknown or not Drupal
  exact_version text NOT NULL DEFAULT '',
  ambiguous text NOT NULL DEFAULT '', -- e.g. '10-or-11' when only agg-include was seen
  signals jsonb NOT NULL DEFAULT '[]',-- [{signal, value, fetch_id}]
  fetch_ids bigint[] NOT NULL,
  site_name text NOT NULL DEFAULT '', -- og:site_name when present (else '')
  derived_by text NOT NULL,           -- 'drupal@1'
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT site_probe_outcome_check CHECK (outcome IN ('ok', 'bot_check', 'robots_disallowed', 'not_html', 'http_error', 'transport_error'))
);

ALTER TABLE site ADD CONSTRAINT site_last_probe_fk FOREIGN KEY (last_probe_id) REFERENCES site_probe(id);

-- fetch_header: the allowlisted response headers of one fetch (x-generator, x-drupal-cache,
-- x-drupal-dynamic-cache, expires, content-type, server, cf-mitigated).
CREATE TABLE fetch_header (
  fetch_id bigint PRIMARY KEY REFERENCES http_fetch(id),
  headers jsonb NOT NULL
);

-- The probe selects sites by state, in list rank order.
CREATE INDEX site_state_rank_idx ON site (state, list_rank);

-- A site's probes, newest first.
CREATE INDEX site_probe_site_idx ON site_probe (site_id, created_at DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE fetch_header;
ALTER TABLE site DROP CONSTRAINT site_last_probe_fk;
DROP TABLE site_probe;
DROP TABLE site;
