-- +goose Up
-- Organisations (a site's owner, an agency) and the ways to reach them.
--
-- Facts are append-only. The one UPDATEd state pointer is org.site_id, set once when the
-- org's site is known: the app role holds UPDATE on that column only. leadstore.Migrate and
-- deploy/sql/grants.sql revoke UPDATE on the append-only tables contact_route,
-- agency_finding and contact_attempt. The app role has no DELETE.

-- org: one row per registrable domain; the first kind recorded for a domain stays.
CREATE TABLE org (
  id bigserial PRIMARY KEY,
  kind text NOT NULL,                 -- site_owner | agency | selftest
  domain text NOT NULL,               -- registrable domain; one org per domain ('selftest-<n>' for selftest)
  name text NOT NULL DEFAULT '',      -- agency name from the file, or site_name; never a person
  country text NOT NULL DEFAULT '',
  site_id bigint REFERENCES site(id),
  source text NOT NULL,               -- probe | agencies-file | selftest
  source_fetch_id bigint REFERENCES http_fetch(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_domain_key UNIQUE (domain),
  CONSTRAINT org_kind_check CHECK (kind IN ('site_owner', 'agency', 'selftest')),
  CONSTRAINT org_source_check CHECK (source IN ('probe', 'agencies-file', 'selftest'))
);

-- contact_route: a role address or a contact form seen on one of the org's pages.
CREATE TABLE contact_route (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  kind text NOT NULL,                 -- email | form
  value text NOT NULL,                -- lower-case address, or the form page URL
  fetch_id bigint NOT NULL REFERENCES http_fetch(id), -- the page it was seen on (its body is kept)
  mx_ok boolean NOT NULL DEFAULT false,
  derived_by text NOT NULL,           -- 'contacts@1'
  found_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_route_org_kind_value_key UNIQUE (org_id, kind, value),
  CONSTRAINT contact_route_kind_check CHECK (kind IN ('email', 'form'))
);

-- agency_finding: an agency page that sells Drupal upgrades, and the words matched on it.
CREATE TABLE agency_finding (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  url text NOT NULL,                  -- the agency's page that sells upgrades
  phrase text NOT NULL,               -- the matched words, at most 120 characters
  fetch_id bigint NOT NULL REFERENCES http_fetch(id), -- its body is kept
  derived_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT agency_finding_phrase_check CHECK (char_length(phrase) <= 120)
);

-- contact_attempt: one route search for an org, written whether or not it found a route,
-- so an org searched recently is not searched again.
CREATE TABLE contact_attempt (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  run_id bigint NOT NULL REFERENCES run(id),
  routes_found int NOT NULL DEFAULT 0,
  attempted_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_attempt_routes_found_check CHECK (routes_found >= 0)
);

-- An org's routes; an org's latest attempts.
CREATE INDEX contact_route_org_idx ON contact_route (org_id, found_at DESC);
CREATE INDEX contact_attempt_org_idx ON contact_attempt (org_id, attempted_at DESC);
CREATE INDEX agency_finding_org_idx ON agency_finding (org_id);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE contact_attempt;
DROP TABLE agency_finding;
DROP TABLE contact_route;
DROP TABLE org;
