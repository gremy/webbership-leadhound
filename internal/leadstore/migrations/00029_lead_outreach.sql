-- +goose Up
-- Outreach by hand: LinkedIn as a channel, the operator's own contacts, the people behind
-- them, edits to drafts, a lead's sector, proof, proposals and a queued "Draft it".
--
-- A LinkedIn company page or person profile is a contact_route of kind linkedin; value is the
-- one canonical form (https://linkedin.com/company/<slug>, /school/<slug> or /in/<slug>, no
-- www, slug lower-cased), so the org's UNIQUE (org_id, kind, value) de-duplicates it.
-- contact_route.person_id names the person a linkedin or email route belongs to, fixed at
-- insert like the rest of the row. lead_channel gains linkedin; every org_fact social row with
-- a LinkedIn company or school page is copied into a route with its own fetch or place.
--
-- A row the operator typed in came from no page and no place: derived_by 'operator@1' is the
-- one marker allowed neither, on contact_route, org_fact and org_person.
--
-- contact_mark: append-only marks on a route, a fact or a person, because each value exists
--   once per org and those rows are fixed at insert. The latest mark (by id) decides: claim
--   is the operator's own and carries his note, retire is gone. A changed value is a new or
--   claimed row plus a retire on the old one, naming its replacement. The app has no DELETE;
--   the owner's erase of a route, fact or person takes its marks with it (CASCADE) and only
--   empties a replacement pointer (SET NULL).
-- org_person: a person at an org, with a role, and a name only if one was printed or typed.
-- message_edit: one saved edit of a draft, the text before and after, append-only. A form or
--   LinkedIn edit leaves outreach_message alone and the latest body_after is the effective
--   text. An email edit also moves outreach_message.subject and .body_text and re-hashes
--   outreach_batch.content_sha256 while the batch is unapproved, so those three columns join
--   the app's pointers: the operator approves the hash he is shown, and the send-time
--   approved_sha256 recheck stays the backstop.
-- org_sector: the operator's (or a rule's) sector of an org, append-only, latest operator row
--   first. proof: a line of proof for one or more sectors, fixed at insert; proof_retire takes
--   one out of use, once. The closed sector list is leadview.Sectors.
-- lead_proposal and lead_proposal_status: a proposal sent to a lead and its statuses, both
--   append-only, latest status wins. The owner blanks title and note to erase an org.
-- draft_request: one "Draft it" on one lead, written by the console and run by the drafting
--   service. status: queued -> drafting (started_at set) -> done | failed (finished_at set).
--   At most one queued or drafting row per lead (draft_request_open_key).
--
-- draft_generation.channel keeps manual drafts out of the email gate; outreach_batch.origin
-- says what made a batch (batch, send_now, draft_it), fixed at insert, and existing manual
-- batches become send_now. contact_research.want may name linkedin.
--
-- Every new table is append-only for the app (UPDATE revoked: leadstore.Migrate and
-- deploy/sql/grants.sql). The UPDATE-able columns they grant:
--   draft_request.status, .reason, .batch_id, .started_at, .finished_at
--   outreach_message.subject, .body_text
--   outreach_batch.content_sha256
SET LOCAL lock_timeout = '5s';

INSERT INTO lead_channel (name, label) VALUES ('linkedin', 'LinkedIn');

CREATE TABLE org_person (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  name text,                            -- NULL unless printed beside the link, or typed by the operator
  role text NOT NULL DEFAULT '',
  derived_by text NOT NULL,             -- 'claude-research@1' | 'operator@1'
  fetch_id bigint REFERENCES http_fetch(id), -- the page it was read from; NULL only for operator@1
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_person_name_check CHECK (char_length(name) <= 120 AND name !~ '[\r\n]'),
  CONSTRAINT org_person_role_check CHECK (char_length(role) <= 60 AND role !~ '[\r\n]'),
  CONSTRAINT org_person_named_check CHECK (btrim(role) <> '' OR btrim(coalesce(name, '')) <> ''),
  CONSTRAINT org_person_source_check CHECK (fetch_id IS NOT NULL OR derived_by = 'operator@1'),
  CONSTRAINT org_person_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX org_person_org_idx ON org_person (org_id, id DESC);

ALTER TABLE contact_route DROP CONSTRAINT contact_route_kind_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_kind_check CHECK (kind IN ('email', 'form', 'linkedin'));
ALTER TABLE contact_route ADD COLUMN person_id bigint REFERENCES org_person(id);
ALTER TABLE contact_route ADD CONSTRAINT contact_route_person_check CHECK (person_id IS NULL OR kind IN ('linkedin', 'email'));
CREATE INDEX contact_route_person_idx ON contact_route (person_id) WHERE person_id IS NOT NULL;

ALTER TABLE contact_route DROP CONSTRAINT contact_route_source_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_source_check CHECK (
  num_nonnulls(fetch_id, place_id) = 1 OR (derived_by = 'operator@1' AND fetch_id IS NULL AND place_id IS NULL));
ALTER TABLE org_fact DROP CONSTRAINT org_fact_source_check;
ALTER TABLE org_fact ADD CONSTRAINT org_fact_source_check CHECK (
  num_nonnulls(fetch_id, place_id) = 1 OR (derived_by = 'operator@1' AND fetch_id IS NULL AND place_id IS NULL));

CREATE TABLE contact_mark (
  id bigserial PRIMARY KEY,
  route_id bigint REFERENCES contact_route(id) ON DELETE CASCADE,
  fact_id bigint REFERENCES org_fact(id) ON DELETE CASCADE,
  person_id bigint REFERENCES org_person(id) ON DELETE CASCADE,
  mark text NOT NULL,                   -- claim | retire
  note text,                            -- the operator's words; NULL when none
  replaced_by_route_id bigint REFERENCES contact_route(id) ON DELETE SET NULL,
  replaced_by_fact_id bigint REFERENCES org_fact(id) ON DELETE SET NULL,
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_mark_target_check CHECK (num_nonnulls(route_id, fact_id, person_id) = 1),
  CONSTRAINT contact_mark_mark_check CHECK (mark IN ('claim', 'retire')),
  CONSTRAINT contact_mark_note_check CHECK (char_length(note) <= 200),
  -- A replacement is named when a route or fact is retired, and is of its own sort.
  CONSTRAINT contact_mark_replaced_check CHECK (
    (replaced_by_route_id IS NULL OR (mark = 'retire' AND route_id IS NOT NULL))
    AND (replaced_by_fact_id IS NULL OR (mark = 'retire' AND fact_id IS NOT NULL))),
  CONSTRAINT contact_mark_actor_check CHECK (actor IN ('operator', 'system'))
);
-- The latest mark of a route, a fact or a person.
CREATE INDEX contact_mark_route_idx ON contact_mark (route_id, id DESC) WHERE route_id IS NOT NULL;
CREATE INDEX contact_mark_fact_idx ON contact_mark (fact_id, id DESC) WHERE fact_id IS NOT NULL;
CREATE INDEX contact_mark_person_idx ON contact_mark (person_id, id DESC) WHERE person_id IS NOT NULL;

ALTER TABLE draft_generation ADD COLUMN channel text NOT NULL DEFAULT 'email';
ALTER TABLE draft_generation ADD CONSTRAINT draft_generation_channel_check CHECK (channel IN ('email', 'form', 'linkedin'));

CREATE TABLE message_edit (
  id bigserial PRIMARY KEY,
  message_id bigint NOT NULL REFERENCES outreach_message(id),
  subject_before text NOT NULL DEFAULT '',
  body_before text NOT NULL,
  subject_after text NOT NULL DEFAULT '',
  body_after text NOT NULL,
  actor text NOT NULL,                  -- operator
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT message_edit_actor_check CHECK (actor = 'operator')
);
-- A message's latest edit.
CREATE INDEX message_edit_message_idx ON message_edit (message_id, id DESC);

CREATE TABLE org_sector (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  sector text NOT NULL,
  source text NOT NULL,                 -- rule | operator
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT org_sector_sector_check CHECK (sector IN ('education', 'government', 'healthcare', 'nonprofit',
    'agency', 'hospitality', 'retail', 'media', 'other')),
  CONSTRAINT org_sector_source_check CHECK (source IN ('rule', 'operator')),
  CONSTRAINT org_sector_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX org_sector_org_idx ON org_sector (org_id, id DESC);

CREATE TABLE proof (
  id bigserial PRIMARY KEY,
  account_id bigint NOT NULL REFERENCES account(id),
  title text NOT NULL,
  line text NOT NULL,                   -- the sentence a draft may use, verbatim
  url text,                             -- https; never carried into a draft unless public
  sectors text[] NOT NULL,              -- a non-empty subset of the closed list
  public boolean NOT NULL,
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT proof_title_check CHECK (length(btrim(title)) BETWEEN 1 AND 80 AND title !~ '[\r\n]'),
  CONSTRAINT proof_line_check CHECK (length(btrim(line)) BETWEEN 1 AND 240 AND line !~ '[\r\n]'),
  CONSTRAINT proof_url_check CHECK (url ~ '^https://[^\s/?#]+[^\s]*$' AND char_length(url) <= 500),
  CONSTRAINT proof_sectors_check CHECK (cardinality(sectors) >= 1 AND sectors <@ ARRAY['education', 'government',
    'healthcare', 'nonprofit', 'agency', 'hospitality', 'retail', 'media', 'other']::text[]
      AND array_position(sectors, NULL) IS NULL),
  CONSTRAINT proof_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX proof_account_idx ON proof (account_id, id DESC);

CREATE TABLE proof_retire (
  proof_id bigint PRIMARY KEY REFERENCES proof(id),
  reason text NOT NULL DEFAULT '',
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT proof_retire_reason_check CHECK (char_length(reason) <= 200)
);

CREATE TABLE lead_proposal (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  title text NOT NULL,
  url text,                             -- https; NULL when none
  value_cents bigint,                   -- NULL when no value is set
  currency text NOT NULL DEFAULT 'GBP',
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_proposal_title_check CHECK (length(btrim(title)) BETWEEN 1 AND 120 AND title !~ '[\r\n]'),
  CONSTRAINT lead_proposal_url_check CHECK (url ~ '^https://[^\s/?#]+[^\s]*$' AND char_length(url) <= 500),
  CONSTRAINT lead_proposal_value_check CHECK (value_cents IS NULL OR value_cents >= 0),
  CONSTRAINT lead_proposal_currency_check CHECK (currency IN ('GBP', 'EUR', 'RON', 'USD')),
  CONSTRAINT lead_proposal_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX lead_proposal_lead_idx ON lead_proposal (lead_id, id DESC);

CREATE TABLE lead_proposal_status (
  id bigserial PRIMARY KEY,
  proposal_id bigint NOT NULL REFERENCES lead_proposal(id),
  status text NOT NULL,                 -- sent | accepted | declined | withdrawn
  note text NOT NULL DEFAULT '',
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_proposal_status_status_check CHECK (status IN ('sent', 'accepted', 'declined', 'withdrawn')),
  CONSTRAINT lead_proposal_status_note_check CHECK (char_length(note) <= 300),
  CONSTRAINT lead_proposal_status_actor_check CHECK (actor IN ('operator', 'system'))
);
-- A proposal's latest status.
CREATE INDEX lead_proposal_status_proposal_idx ON lead_proposal_status (proposal_id, id DESC);

CREATE TABLE draft_request (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  channel text,                         -- email | form | linkedin; NULL: the lead's own channel
  status text NOT NULL DEFAULT 'queued',
  reason text,                          -- why it failed, in plain words
  batch_id bigint REFERENCES outreach_batch(id), -- the batch it made
  requested_at timestamptz NOT NULL DEFAULT now(),
  started_at timestamptz,               -- set when the service takes it
  finished_at timestamptz,              -- set when it ends
  CONSTRAINT draft_request_channel_check CHECK (channel IN ('email', 'form', 'linkedin')),
  CONSTRAINT draft_request_status_check CHECK (status IN ('queued', 'drafting', 'done', 'failed')),
  CONSTRAINT draft_request_reason_check CHECK (char_length(reason) <= 200),
  -- The times follow the status: queued has neither, drafting has started_at only, a finished
  -- row has both.
  CONSTRAINT draft_request_times_check CHECK (
    (status = 'queued' AND started_at IS NULL AND finished_at IS NULL)
    OR (status = 'drafting' AND started_at IS NOT NULL AND finished_at IS NULL)
    OR (status IN ('done', 'failed') AND started_at IS NOT NULL
      AND finished_at IS NOT NULL AND finished_at >= started_at))
);
-- A queued or drafting row is never duplicated for one lead.
CREATE UNIQUE INDEX draft_request_open_key ON draft_request (lead_id) WHERE status IN ('queued', 'drafting');
-- A lead's latest request (the lead page).
CREATE INDEX draft_request_lead_idx ON draft_request (lead_id, id DESC);
-- The service's queue, oldest first.
CREATE INDEX draft_request_queued_idx ON draft_request (requested_at, id) WHERE status = 'queued';

ALTER TABLE outreach_batch ADD COLUMN origin text NOT NULL DEFAULT 'batch';
ALTER TABLE outreach_batch ADD CONSTRAINT outreach_batch_origin_check CHECK (origin IN ('batch', 'send_now', 'draft_it'));
UPDATE outreach_batch SET origin = 'send_now' WHERE manual;

ALTER TABLE contact_research DROP CONSTRAINT contact_research_want_check;
ALTER TABLE contact_research ADD CONSTRAINT contact_research_want_check
  CHECK (cardinality(want) BETWEEN 1 AND 3 AND want <@ ARRAY['email', 'phone', 'linkedin']::text[]
    AND array_position(want, NULL) IS NULL);

-- Every LinkedIn company or school page already read from an org's own site becomes a route.
INSERT INTO contact_route (org_id, kind, value, fetch_id, place_id, derived_by, found_at)
SELECT org_id, 'linkedin', normalized, fetch_id, place_id, derived_by, found_at
FROM org_fact
WHERE kind = 'social' AND (normalized LIKE 'https://linkedin.com/company/%' OR normalized LIKE 'https://linkedin.com/school/%')
ORDER BY id
ON CONFLICT DO NOTHING;

-- +goose Down
-- Runs as the owner only. Every new table's rows go with it. So that the old CHECKs and the
-- lead_channel row can come back with rows present: a lead on LinkedIn, or on a route that is
-- going, falls back to no channel; every linkedin route and every row the operator typed in
-- goes (their marks with them, by CASCADE); a research attempt loses linkedin from want, and
-- one that wanted only linkedin goes. The routes and facts found on pages or places stay.
SET LOCAL lock_timeout = '5s';

UPDATE lead SET route_id = NULL, channel = 'none'
WHERE channel = 'linkedin'
   OR route_id IN (SELECT id FROM contact_route WHERE kind = 'linkedin' OR num_nonnulls(fetch_id, place_id) = 0);
DELETE FROM contact_route WHERE kind = 'linkedin' OR num_nonnulls(fetch_id, place_id) = 0;
DELETE FROM org_fact WHERE num_nonnulls(fetch_id, place_id) = 0;
DELETE FROM lead_channel WHERE name = 'linkedin';

DELETE FROM contact_research WHERE want <@ ARRAY['linkedin']::text[];
UPDATE contact_research SET want = array_remove(want, 'linkedin') WHERE 'linkedin' = ANY(want);
ALTER TABLE contact_research DROP CONSTRAINT contact_research_want_check;
ALTER TABLE contact_research ADD CONSTRAINT contact_research_want_check
  CHECK (cardinality(want) BETWEEN 1 AND 2 AND want <@ ARRAY['email', 'phone']::text[]
    AND array_position(want, NULL) IS NULL);

ALTER TABLE outreach_batch DROP COLUMN origin;
DROP TABLE draft_request;
DROP TABLE lead_proposal_status;
DROP TABLE lead_proposal;
DROP TABLE proof_retire;
DROP TABLE proof;
DROP TABLE org_sector;
DROP TABLE message_edit;
ALTER TABLE draft_generation DROP COLUMN channel;
DROP TABLE contact_mark;

ALTER TABLE org_fact DROP CONSTRAINT org_fact_source_check;
ALTER TABLE org_fact ADD CONSTRAINT org_fact_source_check CHECK (num_nonnulls(fetch_id, place_id) = 1);
ALTER TABLE contact_route DROP CONSTRAINT contact_route_source_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_source_check CHECK (num_nonnulls(fetch_id, place_id) = 1);
ALTER TABLE contact_route DROP COLUMN person_id;
ALTER TABLE contact_route DROP CONSTRAINT contact_route_kind_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_kind_check CHECK (kind IN ('email', 'form'));
DROP TABLE org_person;
