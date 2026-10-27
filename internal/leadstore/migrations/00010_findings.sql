-- +goose Up
-- Findings are general and a lead points at one; lead kinds and channels are rows, not CHECKs.
--
-- lead_kind and lead_channel are reference rows. They change only by migration: the app role
-- may read them only (leadstore.Migrate and deploy/sql/grants.sql revoke INSERT, UPDATE and
-- DELETE), and the test reset keeps them. lead_kind.outreach says whether a batch may draft
-- a lead of that kind.
-- finding is append-only (UPDATE revoked from the app role): one row per thing a detector
-- saw that makes an org a lead, with the source row it was read from (UNIQUE, so a source
-- row gives one finding however often a build runs) and its evidence [{name, value,
-- fetch_id}]. UNIQUE (id, org_id) lets lead point at a finding of its own org only.
-- The app role's UPDATE on lead is its state pointers only:
--   state, state_at, kind, probe_id, agency_finding_id, finding_id, channel, route_id
-- finding_id follows the probe or agency pointer while the lead is new (RefreshLead); probe_id
-- and agency_finding_id stay, because drafting and sending read them.
--
-- Later kinds (designed, not built here), each one INSERT INTO lead_kind and finding rows:
--   a job ad for an automatable role: kind automation, detector jobad@1, source_table job, subject {job_id, roles, systems}
--   a shop with broken tracking: detector tracking@1 on a site fetch, subject {platform, pixels, gap}
--   a new company: detector registry@1, subject {register, number, incorporated}
--   a business with no website: org.domain becomes nullable with an org_ref (source, ref) unique key; lead is not altered
-- The table needs nothing more for a later kind; its Go side needs its own source in findingSource (findings.go).
SET LOCAL lock_timeout = '5s';

CREATE TABLE lead_kind (
  name text PRIMARY KEY,
  label text NOT NULL,
  description text NOT NULL DEFAULT '',
  outreach boolean NOT NULL           -- false: never drafted (selftest, hand)
);
INSERT INTO lead_kind (name, label, description, outreach) VALUES
  ('unsupported', 'Unsupported Drupal', 'A site on Drupal 6 to 9, past its security support', true),
  ('d10', 'Drupal 10', 'A site on Drupal 10, whose security support ends on a known date', true),
  ('agency_partner', 'Agency partner', 'An agency whose own page sells Drupal upgrades', true),
  ('selftest', 'Self-test', 'A test message to the operator''s own address', false),
  ('hand', 'Contacted by hand', 'An org the operator reached outside LeadHound', false);

CREATE TABLE lead_channel (
  name text PRIMARY KEY,
  label text NOT NULL
);
INSERT INTO lead_channel (name, label) VALUES
  ('email', 'Email'),
  ('form', 'Contact form'),
  ('none', 'No channel yet');

CREATE TABLE finding (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  kind text NOT NULL REFERENCES lead_kind(name),
  detector text NOT NULL,             -- the source's derived_by, e.g. 'drupal@1'
  subject jsonb NOT NULL DEFAULT '{}',
  evidence jsonb NOT NULL DEFAULT '[]', -- [{name, value, fetch_id}]
  source_table text,                  -- site_probe | agency_finding | a later source; NULL without a source row
  source_id bigint,
  observed_at timestamptz NOT NULL,   -- when the source saw it
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT finding_source_key UNIQUE (source_table, source_id),
  CONSTRAINT finding_id_org_key UNIQUE (id, org_id),
  CONSTRAINT finding_source_check CHECK ((source_table IS NULL) = (source_id IS NULL)),
  CONSTRAINT finding_subject_check CHECK (jsonb_typeof(subject) = 'object'),
  CONSTRAINT finding_evidence_check CHECK (jsonb_typeof(evidence) = 'array')
);
CREATE INDEX finding_org_idx ON finding (org_id, id DESC);

-- One finding per source row a lead points at: the probe's signals become its evidence, an
-- agency finding's matched words its one piece. A lead's kind is its finding's.
INSERT INTO finding (org_id, kind, detector, subject, evidence, source_table, source_id, observed_at)
SELECT DISTINCT ON (p.id) l.org_id, l.kind, p.derived_by,
       jsonb_build_object('major', p.major, 'exact_version', p.exact_version, 'ambiguous', p.ambiguous),
       coalesce((SELECT jsonb_agg(jsonb_build_object('name', e.v->>'signal', 'value', e.v->'value', 'fetch_id', e.v->'fetch_id') ORDER BY e.i)
                 FROM jsonb_array_elements(p.signals) WITH ORDINALITY AS e(v, i)), '[]'::jsonb),
       'site_probe', p.id, p.created_at
FROM lead l JOIN site_probe p ON p.id = l.probe_id
ORDER BY p.id, l.id;

INSERT INTO finding (org_id, kind, detector, subject, evidence, source_table, source_id, observed_at)
SELECT DISTINCT ON (af.id) l.org_id, l.kind, af.derived_by,
       jsonb_build_object('url', af.url, 'phrase', af.phrase),
       jsonb_build_array(jsonb_build_object('name', 'phrase', 'value', af.phrase, 'fetch_id', af.fetch_id)),
       'agency_finding', af.id, af.created_at
FROM lead l JOIN agency_finding af ON af.id = l.agency_finding_id
ORDER BY af.id, l.id;

ALTER TABLE lead ADD COLUMN finding_id bigint;
ALTER TABLE lead ADD CONSTRAINT lead_finding_fk FOREIGN KEY (finding_id, org_id) REFERENCES finding (id, org_id);
UPDATE lead l SET finding_id = f.id FROM finding f
WHERE f.source_table = 'site_probe' AND f.source_id = l.probe_id;
UPDATE lead l SET finding_id = f.id FROM finding f
WHERE l.finding_id IS NULL AND f.source_table = 'agency_finding' AND f.source_id = l.agency_finding_id;

-- Kinds and channels are checked against their rows; today's three kinds keep their exact
-- pointer rule, and a later kind needs only its finding.
ALTER TABLE lead DROP CONSTRAINT lead_kind_check;
ALTER TABLE lead DROP CONSTRAINT lead_channel_check;
ALTER TABLE lead ADD CONSTRAINT lead_kind_fk FOREIGN KEY (kind) REFERENCES lead_kind(name);
ALTER TABLE lead ADD CONSTRAINT lead_channel_fk FOREIGN KEY (channel) REFERENCES lead_channel(name);
ALTER TABLE lead DROP CONSTRAINT lead_finding_check;
ALTER TABLE lead ADD CONSTRAINT lead_finding_check CHECK (kind IN ('selftest', 'hand') OR (finding_id IS NOT NULL
  AND (kind NOT IN ('unsupported', 'd10', 'agency_partner') OR ((probe_id IS NOT NULL) <> (agency_finding_id IS NOT NULL)))));

-- +goose Down
-- Runs as the owner only. A lead of a kind the old CHECK does not name cannot go back: the
-- CHECK restore fails and the whole step rolls back, nothing changed.
SET LOCAL lock_timeout = '5s';
ALTER TABLE lead DROP CONSTRAINT lead_finding_check;
ALTER TABLE lead DROP CONSTRAINT lead_channel_fk;
ALTER TABLE lead DROP CONSTRAINT lead_kind_fk;
ALTER TABLE lead DROP CONSTRAINT lead_finding_fk;
ALTER TABLE lead DROP COLUMN finding_id;
ALTER TABLE lead ADD CONSTRAINT lead_kind_check CHECK (kind IN ('unsupported', 'd10', 'agency_partner', 'selftest', 'hand'));
ALTER TABLE lead ADD CONSTRAINT lead_channel_check CHECK (channel IN ('email', 'form', 'none'));
ALTER TABLE lead ADD CONSTRAINT lead_finding_check CHECK (kind IN ('selftest', 'hand') OR ((probe_id IS NOT NULL) <> (agency_finding_id IS NOT NULL)));
DROP TABLE finding;
DROP TABLE lead_channel;
DROP TABLE lead_kind;
