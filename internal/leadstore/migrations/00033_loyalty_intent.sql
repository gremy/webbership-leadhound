-- +goose Up
-- Loyalty intent on café leads: what a loyalty vendor's own Instagram shows about a café (who
-- follows the vendor, who comments on or is tagged in its posts), and the class LeadHound
-- derives from that, with the operator's own call over it.
--
-- A handle is stored lower case without its @ (ig_follow's shape, enrich.InstagramHandle).
--
-- ig_follow gains its direction: 'following' is the list_owner following handle (every row so
--   far), 'followers' is handle following the list_owner. The capture key takes the direction,
--   so one account on both lists of one owner in one capture is two rows.
-- ig_vendor_touch: one dated contact between a vendor's account and another account,
--   append-only. kind comment: handle commented on the vendor's post (ref is the comment id);
--   usertag / mention: the vendor's post (ref is its code) tags or @-names handle; tagged:
--   handle's own post (ref is its code) tags the vendor. at is the comment's created_at or the
--   post's taken_at. text is the comment's own text, at most 300, kept only for a strongly
--   matched account; full_name only for a matched or reviewed account (ig_follow's rule).
--   A re-import of the same touch stores nothing twice.
-- lead_intent: the class derived for a café lead (rules intent@1), append-only; one sync writes
--   a row only when it changes; the latest row of a lead (by id) decides, and a lead with no
--   row reads as cold. overlap counts the distinct loyalty vendors the café follows, score
--   orders leads inside a class (higher first), signal_at dates the deciding signal (NULL when
--   undated) and held_until holds a just_chose lead until signal_at + 60 days.
-- lead_intent_call: the operator's own call on a lead's class, append-only; class NULL clears
--   it; the latest row of a lead (by id) decides.
--
-- The ALTERs take an exclusive lock on ig_follow, which every import reads, so they wait at
-- most 5s.
SET LOCAL lock_timeout = '5s';

ALTER TABLE ig_follow ADD COLUMN direction text NOT NULL DEFAULT 'following'
  CONSTRAINT ig_follow_direction_check CHECK (direction IN ('following', 'followers'));
ALTER TABLE ig_follow DROP CONSTRAINT ig_follow_capture_key;
ALTER TABLE ig_follow ADD CONSTRAINT ig_follow_capture_key UNIQUE (list_owner, direction, handle, capture_sha256);

CREATE TABLE ig_vendor_touch (
  id bigserial PRIMARY KEY,
  vendor_handle text NOT NULL,          -- the vendor's own account
  handle text NOT NULL,                 -- the other account
  kind text NOT NULL,                   -- comment | usertag | mention | tagged
  ref text NOT NULL,                    -- the comment id, or the post code
  post_code text NOT NULL,
  at timestamptz NOT NULL,              -- the comment's created_at, or the post's taken_at
  text text,                            -- comment text only, for a strongly matched account
  full_name text,                       -- only for a matched or reviewed account
  capture_sha256 bytea NOT NULL,
  captured_at timestamptz NOT NULL,
  imported_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ig_vendor_touch_vendor_handle_check CHECK (vendor_handle ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_vendor_touch_handle_check CHECK (handle ~ '^[a-z0-9._]{1,30}$'),
  CONSTRAINT ig_vendor_touch_kind_check CHECK (kind IN ('comment', 'usertag', 'mention', 'tagged')),
  CONSTRAINT ig_vendor_touch_ref_check CHECK (ref ~ '^[A-Za-z0-9_-]{1,40}$'),
  CONSTRAINT ig_vendor_touch_post_code_check CHECK (post_code ~ '^[A-Za-z0-9_-]{1,40}$'),
  CONSTRAINT ig_vendor_touch_text_kind_check CHECK (kind = 'comment' OR text IS NULL),
  CONSTRAINT ig_vendor_touch_text_check CHECK (char_length(text) <= 300),
  CONSTRAINT ig_vendor_touch_full_name_check CHECK (char_length(full_name) <= 150 AND full_name !~ '[\r\n]'),
  CONSTRAINT ig_vendor_touch_sha_check CHECK (octet_length(capture_sha256) = 32),
  CONSTRAINT ig_vendor_touch_key UNIQUE (vendor_handle, kind, handle, ref)
);
CREATE INDEX ig_vendor_touch_handle_idx ON ig_vendor_touch (handle, at DESC);

CREATE TABLE lead_intent (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  class text NOT NULL,                  -- shopping | paper | just_chose | switch | curious | cold | out
  overlap integer NOT NULL,             -- distinct loyalty vendors the café follows
  score integer NOT NULL,               -- order inside the class, higher first
  reason text NOT NULL,                 -- plain words
  signal_at timestamptz,                -- the date of the deciding signal; NULL when undated
  held_until timestamptz,               -- just_chose: signal_at + 60 days; else NULL
  rules text NOT NULL DEFAULT 'intent@1',
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_intent_class_check CHECK (class IN ('shopping', 'paper', 'just_chose', 'switch', 'curious', 'cold', 'out')),
  CONSTRAINT lead_intent_overlap_check CHECK (overlap >= 0),
  CONSTRAINT lead_intent_reason_check CHECK (reason !~ '[\r\n]' AND char_length(reason) <= 300),
  CONSTRAINT lead_intent_rules_check CHECK (rules ~ '^intent@[0-9]+$')
);
CREATE INDEX lead_intent_lead_idx ON lead_intent (lead_id, id DESC);

CREATE TABLE lead_intent_call (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  class text,                           -- one of the seven, or NULL (cleared)
  held_until timestamptz,               -- a just_chose call: created_at + 60 days; else NULL
  note text NOT NULL DEFAULT '',
  actor text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_intent_call_class_check CHECK (class IN ('shopping', 'paper', 'just_chose', 'switch', 'curious', 'cold', 'out')),
  CONSTRAINT lead_intent_call_note_check CHECK (note !~ '[\r\n]' AND char_length(note) <= 200),
  CONSTRAINT lead_intent_call_actor_check CHECK (actor = 'gremy')
);
CREATE INDEX lead_intent_call_lead_idx ON lead_intent_call (lead_id, id DESC);

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE, DELETE, TRUNCATE ON public.ig_vendor_touch FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.lead_intent FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.lead_intent_call FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. A call of yours, a vendor touch or a followers row cannot go back
-- (the old key cannot tell a followers row from a following one), and erasing one is a
-- decision, not a side effect of a rollback: the down refuses while any exists. Otherwise the
-- three tables go (lead_intent is derived and rebuilt by the next sync), the old capture key
-- comes back and the direction column goes.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
DO $$
DECLARE
  n bigint;
BEGIN
  SELECT (SELECT count(*) FROM lead_intent_call)
       + (SELECT count(*) FROM ig_vendor_touch)
       + (SELECT count(*) FROM ig_follow WHERE direction = 'followers') INTO n;
  IF n > 0 THEN
    RAISE EXCEPTION 'leads 00033 down: % intent call, vendor touch or followers rows exist; erase or move them first', n;
  END IF;
END
$$;
-- +goose StatementEnd

DROP TABLE lead_intent_call;
DROP TABLE lead_intent;
DROP TABLE ig_vendor_touch;

ALTER TABLE ig_follow DROP CONSTRAINT ig_follow_capture_key;
ALTER TABLE ig_follow ADD CONSTRAINT ig_follow_capture_key UNIQUE (list_owner, handle, capture_sha256);
ALTER TABLE ig_follow DROP COLUMN direction;
