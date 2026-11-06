-- +goose Up
-- Instagram becomes a fourth source of a café lead's place type (lead_place_type, 00030).
--
-- A strongly matched profile shows whether a café has a place to sit: Instagram's place call
-- (ig_place, 00031) reads that from the business's own posts, which says more than a roastery
-- word in its name or Jev's read of its website. So an instagram row ranks over jev and name,
-- and under the operator, whose own call always wins (placeTypeWinnerSQL).
--
-- The rows are written by one deterministic sync (leadstore.SyncIGPlaceTypes), never by a
-- model call: actor system, no input hash (lead_place_type_sha_check already asks NULL of
-- every source but jev). An 'unclear' row from instagram means "Instagram is silent now": the
-- profile was unmatched, its call is unclear or not coffee, or no real answer stands. The table
-- is append-only, so a newer unclear row is the only way for Instagram to stop deciding, and
-- the next source speaks, exactly as Jev's "asked, no answer" already works.
--
-- The ALTERs take an exclusive lock on a table every queue read uses, so they wait at most 5s.
SET LOCAL lock_timeout = '5s';

ALTER TABLE lead_place_type
  DROP CONSTRAINT lead_place_type_source_check,
  ADD CONSTRAINT lead_place_type_source_check CHECK (source IN ('name', 'jev', 'instagram', 'operator'));
ALTER TABLE lead_place_type
  DROP CONSTRAINT lead_place_type_unclear_check,
  ADD CONSTRAINT lead_place_type_unclear_check CHECK (place_type <> 'unclear' OR source IN ('jev', 'instagram'));

-- +goose Down
-- Runs as the owner only. An instagram row cannot go back under the old CHECKs, and erasing
-- one is a decision, not a side effect of a rollback: the down refuses while any exists.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
DO $$
DECLARE
  n bigint;
BEGIN
  SELECT count(*) FROM lead_place_type WHERE source = 'instagram' INTO n;
  IF n > 0 THEN
    RAISE EXCEPTION 'leads 00032 down: % instagram place type rows exist; erase or move them first', n;
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE lead_place_type
  DROP CONSTRAINT lead_place_type_unclear_check,
  ADD CONSTRAINT lead_place_type_unclear_check CHECK (place_type <> 'unclear' OR source = 'jev');
ALTER TABLE lead_place_type
  DROP CONSTRAINT lead_place_type_source_check,
  ADD CONSTRAINT lead_place_type_source_check CHECK (source IN ('name', 'jev', 'operator'));
