-- +goose Up
-- Instagram signals on café leads: what a café's public Instagram profile shows, read through
-- the operator's own browser into a capture file and imported here, and Instagram as a manual
-- channel (one direct message, sent by hand).
--
-- A handle is stored lower case without its @ (ig_handle_check's shape, enrich.InstagramHandle).
--
-- ig_profile: one capture of one profile, append-only. A re-import of the same capture line
--   (the same handle and capture_sha256) stores nothing twice. outcome 'missing' is a handle
--   that no longer exists: stored and counted, with no text. source says how the profile was
--   reached; source_of is the list owner's handle for 'following', NULL otherwise.
--   source_lead_id is the capture's own lead hint, for the report only: it is never a match
--   and carries no foreign key, because dev and production ids differ.
-- ig_follow: one account kept from one following list, append-only. full_name is written only
--   for an account that is matched or lands in the review list; any other row holds the
--   handle alone.
-- ig_match: what a handle matches, append-only; the latest row of a handle (by id) decides.
--   tier handle and domain are strong (signals attach), name is weak (the review list), operator
--   is the operator's own call and none matches nothing. lead_id is the lead matched; NULL means
--   no lead: tier none, an operator's unmatch, or more than one candidate at the best tier, whose
--   leads are then in candidate_ids (two or more). A single weak candidate is lead_id itself.
-- ig_judgment: one answer on one profile, append-only: the question, the effective choice, and
--   for Jev the raw choice and confidence it gave (choice is 'unclear' when the confidence is
--   below the bar) with the SHA-256 of what Jev was asked. A rule row is a deterministic call
--   (a vendor seen in a link or a tag, or no text to judge).
-- ig_signal_hint: a weak signal about a handle, once per handle, kind and value; today only
--   followed_by_vendor:<vendor id>, which never counts as running a scheme.
-- ig_review_status: the review list, append-only; the latest row of a handle (by id) decides.
--
-- lead_channel gains instagram; contact_route kind instagram holds the one canonical value
-- https://www.instagram.com/<handle>/ (enrich.InstagramURL). A route written by the import
-- comes from no page and no place: derived_by 'instagram@1' is allowed neither, on an
-- instagram route only. Instagram routes are a business's own handle, never a person's, so
-- contact_route_person_check stays as it is, and research never hunts Instagram
-- (contact_research_want_check stays too). Every org_fact social row holding an Instagram
-- profile is copied into a route with its own fetch or place, as 00029 did for LinkedIn.
--
-- Every new table is append-only for the app: UPDATE, DELETE and TRUNCATE revoked here, and
-- UPDATE again by leadstore.Migrate and deploy/sql/grants.sql after every migrate. The owner
-- alone deletes from them, to erase an org.
SET LOCAL lock_timeout = '5s';

INSERT INTO lead_channel (name, label) VALUES ('instagram', 'Instagram');

ALTER TABLE contact_route DROP CONSTRAINT contact_route_kind_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_kind_check CHECK (kind IN ('email', 'form', 'linkedin', 'instagram'));
ALTER TABLE contact_route DROP CONSTRAINT contact_route_source_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_source_check CHECK (
  num_nonnulls(fetch_id, place_id) = 1
  OR (derived_by = 'operator@1' AND fetch_id IS NULL AND place_id IS NULL)
  OR (derived_by = 'instagram@1' AND kind = 'instagram' AND fetch_id IS NULL AND place_id IS NULL));

ALTER TABLE draft_generation DROP CONSTRAINT draft_generation_channel_check;
ALTER TABLE draft_generation ADD CONSTRAINT draft_generation_channel_check CHECK (channel IN ('email', 'form', 'linkedin', 'instagram'));
ALTER TABLE draft_request DROP CONSTRAINT draft_request_channel_check;
ALTER TABLE draft_request ADD CONSTRAINT draft_request_channel_check CHECK (channel IN ('email', 'form', 'linkedin', 'instagram'));
ALTER TABLE outreach_message DROP CONSTRAINT outreach_message_channel_check;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_channel_check
  CHECK (channel IN ('email', 'form', 'linkedin', 'phone', 'instagram'));

CREATE TABLE ig_profile (
  id bigserial PRIMARY KEY,
  handle text NOT NULL,
  outcome text NOT NULL DEFAULT 'ok',   -- ok | missing (the handle no longer exists)
  ig_user_id text,                      -- Instagram's numeric id, when the page gave it
  full_name text,
  category text,
  bio text,
  external_url text,
  followers integer,
  following integer,
  posts integer,
  is_private boolean,
  captions jsonb NOT NULL DEFAULT '[]', -- at most 12 strings
  source text NOT NULL,                 -- lead | following | search | manual
  source_of text,                       -- the list owner's handle for 'following'
  source_lead_id bigint,                -- the capture's lead hint; never a match, no foreign key
  capture_sha256 bytea NOT NULL,        -- SHA-256 of the capture line's canonical JSON
  captured_at timestamptz NOT NULL,
  imported_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ig_profile_handle_check CHECK (handle ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_profile_outcome_check CHECK (outcome IN ('ok', 'missing')),
  CONSTRAINT ig_profile_user_id_check CHECK (ig_user_id ~ '^[0-9]{1,30}$'),
  CONSTRAINT ig_profile_full_name_check CHECK (char_length(full_name) <= 150 AND full_name !~ '[\r\n]'),
  CONSTRAINT ig_profile_category_check CHECK (char_length(category) <= 100 AND category !~ '[\r\n]'),
  CONSTRAINT ig_profile_bio_check CHECK (char_length(bio) <= 600),
  CONSTRAINT ig_profile_external_url_check CHECK (char_length(external_url) <= 2048 AND external_url !~ '\s'),
  CONSTRAINT ig_profile_counts_check CHECK (followers >= 0 AND following >= 0 AND posts >= 0),
  CONSTRAINT ig_profile_captions_check CHECK (jsonb_typeof(captions) = 'array' AND jsonb_array_length(captions) <= 12
    AND NOT jsonb_path_exists(captions, '$[*] ? (@.type() != "string")')),
  CONSTRAINT ig_profile_source_check CHECK (source IN ('lead', 'following', 'search', 'manual')),
  CONSTRAINT ig_profile_source_of_check CHECK ((source = 'following') = (source_of IS NOT NULL)
    AND (source_of IS NULL OR source_of ~ '^[a-z0-9._]{1,30}$')),
  CONSTRAINT ig_profile_sha_check CHECK (octet_length(capture_sha256) = 32),
  CONSTRAINT ig_profile_capture_key UNIQUE (handle, capture_sha256)
);
-- A handle's latest capture.
CREATE INDEX ig_profile_handle_idx ON ig_profile (handle, id DESC);

CREATE TABLE ig_follow (
  id bigserial PRIMARY KEY,
  list_owner text NOT NULL,             -- the account whose following list it is
  handle text NOT NULL,                 -- the account it follows
  full_name text,                       -- only for a matched or reviewed account
  captured_at timestamptz NOT NULL,
  capture_sha256 bytea NOT NULL,
  imported_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ig_follow_list_owner_check CHECK (list_owner ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_follow_handle_check CHECK (handle ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_follow_full_name_check CHECK (char_length(full_name) <= 150 AND full_name !~ '[\r\n]'),
  CONSTRAINT ig_follow_sha_check CHECK (octet_length(capture_sha256) = 32),
  CONSTRAINT ig_follow_capture_key UNIQUE (list_owner, handle, capture_sha256)
);
CREATE INDEX ig_follow_handle_idx ON ig_follow (handle, id DESC);

CREATE TABLE ig_match (
  id bigserial PRIMARY KEY,
  handle text NOT NULL,
  lead_id bigint REFERENCES lead(id),   -- NULL: no lead (none, an unmatch, or several candidates)
  tier text NOT NULL,                   -- handle | domain | name | operator | none
  candidate_ids bigint[] NOT NULL DEFAULT '{}', -- the leads tied at the best tier, two or more
  detail text NOT NULL DEFAULT '',      -- what matched, in plain words
  actor text NOT NULL,                  -- as lead_event.actor
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ig_match_handle_check CHECK (handle ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_match_tier_check CHECK (tier IN ('handle', 'domain', 'name', 'operator', 'none')),
  -- One lead, or several candidates, or none at all; none and the operator name no candidates.
  CONSTRAINT ig_match_lead_check CHECK (
    CASE
      WHEN tier = 'none' THEN lead_id IS NULL AND cardinality(candidate_ids) = 0
      WHEN tier = 'operator' THEN cardinality(candidate_ids) = 0
      WHEN lead_id IS NOT NULL THEN cardinality(candidate_ids) = 0
      ELSE cardinality(candidate_ids) >= 2
    END AND array_position(candidate_ids, NULL) IS NULL),
  CONSTRAINT ig_match_operator_check CHECK (tier <> 'operator' OR actor = 'gremy'),
  CONSTRAINT ig_match_detail_check CHECK (detail !~ '[\r\n]' AND char_length(detail) <= 300),
  CONSTRAINT ig_match_actor_check CHECK (actor IN ('gremy', 'system'))
);
-- A handle's latest match, and the matches naming a lead (the lead page).
CREATE INDEX ig_match_handle_idx ON ig_match (handle, id DESC);
CREATE INDEX ig_match_lead_idx ON ig_match (lead_id, id DESC) WHERE lead_id IS NOT NULL;

CREATE TABLE ig_judgment (
  id bigserial PRIMARY KEY,
  profile_id bigint NOT NULL REFERENCES ig_profile(id),
  question text NOT NULL,               -- ig_place | ig_loyalty | ig_vendor
  choice text NOT NULL,                 -- the effective choice
  raw_choice text,                      -- Jev's choice as given; NULL for a rule
  confidence real,                      -- Jev's confidence; NULL for a rule
  source text NOT NULL,                 -- jev | rule
  model text,                           -- the model Jev ran; NULL for a rule
  detail text NOT NULL DEFAULT '',      -- for a rule, how it was seen (a link, a tag)
  input_sha256 bytea,                   -- what Jev was asked; Jev's rows only
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ig_judgment_question_check CHECK (question IN ('ig_place', 'ig_loyalty', 'ig_vendor')),
  CONSTRAINT ig_judgment_choice_check CHECK (choice ~ '^[a-z0-9_-]{1,40}$' AND raw_choice ~ '^[a-z0-9_-]{1,40}$'),
  CONSTRAINT ig_judgment_confidence_check CHECK (confidence >= 0 AND confidence <= 1),
  CONSTRAINT ig_judgment_source_check CHECK (source IN ('jev', 'rule')),
  CONSTRAINT ig_judgment_jev_check CHECK (
    CASE WHEN source = 'jev' THEN coalesce(octet_length(input_sha256), 0) = 32 AND raw_choice IS NOT NULL
      ELSE input_sha256 IS NULL AND raw_choice IS NULL AND confidence IS NULL END),
  CONSTRAINT ig_judgment_detail_check CHECK (detail !~ '[\r\n]' AND char_length(detail) <= 300),
  CONSTRAINT ig_judgment_model_check CHECK (char_length(model) <= 100)
);
-- A profile's latest answer on each question.
CREATE INDEX ig_judgment_profile_idx ON ig_judgment (profile_id, question, id DESC);

CREATE TABLE ig_signal_hint (
  id bigserial PRIMARY KEY,
  handle text NOT NULL,
  kind text NOT NULL,                   -- followed_by_vendor
  value text NOT NULL,                  -- the vendor id
  captured_at timestamptz NOT NULL,     -- when the hint was first seen
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ig_signal_hint_handle_check CHECK (handle ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_signal_hint_kind_check CHECK (kind IN ('followed_by_vendor')),
  CONSTRAINT ig_signal_hint_value_check CHECK (value ~ '^[a-z0-9_-]{1,40}$'),
  CONSTRAINT ig_signal_hint_key UNIQUE (handle, kind, value)
);

CREATE TABLE ig_review_status (
  id bigserial PRIMARY KEY,
  handle text NOT NULL,
  status text NOT NULL,                 -- new | dismissed | matched | wanted
  note text NOT NULL DEFAULT '',
  actor text NOT NULL,                  -- as lead_event.actor
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ig_review_status_handle_check CHECK (handle ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_review_status_status_check CHECK (status IN ('new', 'dismissed', 'matched', 'wanted')),
  CONSTRAINT ig_review_status_note_check CHECK (note !~ '[\r\n]' AND char_length(note) <= 200),
  CONSTRAINT ig_review_status_actor_check CHECK (actor IN ('gremy', 'system'))
);
CREATE INDEX ig_review_status_handle_idx ON ig_review_status (handle, id DESC);

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE, DELETE, TRUNCATE ON public.ig_profile FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.ig_follow FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.ig_match FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.ig_judgment FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.ig_signal_hint FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.ig_review_status FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- Every Instagram profile already read from an org's own site or map listing becomes a route,
-- with the fact's own fetch or place. enrich normalises a social link to
-- https://instagram.com/<path> in lower case; only a first path segment shaped like a handle
-- that is not one of Instagram's own pages is a profile.
INSERT INTO contact_route (org_id, kind, value, fetch_id, place_id, derived_by, found_at)
SELECT org_id, 'instagram', 'https://www.instagram.com/' || handle || '/', fetch_id, place_id, derived_by, found_at
FROM (SELECT f.id, f.org_id, f.fetch_id, f.place_id, f.derived_by, f.found_at,
        substring(f.normalized FROM '^https://instagram\.com/([^/?#]+)') AS handle
      FROM org_fact f
      WHERE f.kind = 'social' AND f.normalized LIKE 'https://instagram.com/%') s
WHERE handle ~ '^[a-z0-9._]{1,30}$' AND btrim(handle, '.') <> ''
  AND handle NOT IN ('p', 'reel', 'reels', 'explore', 'stories', 'tv', 'accounts')
ORDER BY id
ON CONFLICT DO NOTHING;

-- +goose Down
-- Runs as the owner only. An Instagram message, generation or draft request cannot go back
-- under the old CHECKs, and erasing one is a decision, not a side effect of a rollback: the
-- down refuses while any exists. Otherwise a lead on Instagram, or on an Instagram route,
-- falls back to no channel; every instagram route goes (the backfilled ones and the import's,
-- their marks with them by CASCADE), the old CHECKs and the lead_channel rows come back, and
-- the six tables go with their rows. The social facts the routes were copied from stay.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
DO $$
DECLARE
  n bigint;
BEGIN
  SELECT (SELECT count(*) FROM outreach_message WHERE channel = 'instagram')
       + (SELECT count(*) FROM draft_generation WHERE channel = 'instagram')
       + (SELECT count(*) FROM draft_request WHERE channel = 'instagram') INTO n;
  IF n > 0 THEN
    RAISE EXCEPTION 'leads 00031 down: % Instagram message, generation or draft request rows exist; erase or move them first', n;
  END IF;
END
$$;
-- +goose StatementEnd

UPDATE lead SET route_id = NULL, channel = 'none'
WHERE channel = 'instagram'
   OR route_id IN (SELECT id FROM contact_route WHERE kind = 'instagram');
DELETE FROM contact_route WHERE kind = 'instagram';
DELETE FROM lead_channel WHERE name = 'instagram';

ALTER TABLE contact_route DROP CONSTRAINT contact_route_source_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_source_check CHECK (
  num_nonnulls(fetch_id, place_id) = 1 OR (derived_by = 'operator@1' AND fetch_id IS NULL AND place_id IS NULL));
ALTER TABLE contact_route DROP CONSTRAINT contact_route_kind_check;
ALTER TABLE contact_route ADD CONSTRAINT contact_route_kind_check CHECK (kind IN ('email', 'form', 'linkedin'));
ALTER TABLE draft_generation DROP CONSTRAINT draft_generation_channel_check;
ALTER TABLE draft_generation ADD CONSTRAINT draft_generation_channel_check CHECK (channel IN ('email', 'form', 'linkedin'));
ALTER TABLE draft_request DROP CONSTRAINT draft_request_channel_check;
ALTER TABLE draft_request ADD CONSTRAINT draft_request_channel_check CHECK (channel IN ('email', 'form', 'linkedin'));
ALTER TABLE outreach_message DROP CONSTRAINT outreach_message_channel_check;
ALTER TABLE outreach_message ADD CONSTRAINT outreach_message_channel_check
  CHECK (channel IN ('email', 'form', 'linkedin', 'phone'));

DROP TABLE ig_review_status;
DROP TABLE ig_signal_hint;
DROP TABLE ig_judgment;
DROP TABLE ig_match;
DROP TABLE ig_follow;
DROP TABLE ig_profile;
