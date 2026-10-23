-- +goose Up
-- board: the registry of ATS job boards the collector may read, one row per (ats, slug,
-- region). A dead board is retired, never deleted: the app role has INSERT, UPDATE and
-- SELECT here through the owner's default privileges, and no DELETE (deploy/sql/grants.sql).
--
-- The ats check is a named constraint so a later migration can widen it for another ATS
-- (ALTER TABLE board DROP CONSTRAINT board_ats_check, ADD CONSTRAINT board_ats_check ...).
CREATE TABLE board (
  id bigserial PRIMARY KEY,
  ats text NOT NULL,
  slug text NOT NULL,               -- as the ATS spells it: Lever's API is case-sensitive
  region text NOT NULL DEFAULT '',  -- '' or 'eu' (Lever's EU API)
  name text NOT NULL DEFAULT '',    -- company name when known
  origin text NOT NULL,             -- how the board entered: manual > found > seed-list, commoncrawl
  state text NOT NULL DEFAULT 'unverified',
  added_at timestamptz NOT NULL DEFAULT now(),
  checked_at timestamptz,           -- last request that got an answer or a failure
  last_ok_at timestamptz,           -- last answer that parsed
  misses int NOT NULL DEFAULT 0,    -- consecutive not_found/error outcomes, at most one per local day
  last_miss_day date,               -- local day of the last miss counted
  postings_last int,                -- postings the last ok answer listed
  CONSTRAINT board_ats_check CHECK (ats IN ('greenhouse', 'lever', 'ashby')),
  CONSTRAINT board_slug_check CHECK (slug <> ''),
  CONSTRAINT board_region_check CHECK (region IN ('', 'eu')),
  CONSTRAINT board_origin_check CHECK (origin IN ('manual', 'seed-list', 'commoncrawl', 'found')),
  CONSTRAINT board_state_check CHECK (state IN ('unverified', 'ok', 'not_found', 'error', 'retired')),
  CONSTRAINT board_misses_check CHECK (misses >= 0)
);

-- One board per ats, slug and region, ignoring case: the slug keeps its spelling (an API
-- may need it), but two spellings are one board. Import's ON CONFLICT names this index.
CREATE UNIQUE INDEX board_key_idx ON board (ats, lower(slug), region);

-- collect and check select boards by state and ats.
CREATE INDEX board_state_ats_idx ON board (state, ats);

-- An ATS job's source_job_id is "<board>/<id>" (Lever EU: "eu:<slug>/<id>"); expiring one
-- board's missing postings (store.ExpireMissing) reads that board's open jobs through this,
-- instead of every open job of the source once per board.
CREATE INDEX job_open_board_idx ON job (source, split_part(source_job_id, '/', 1)) WHERE listing_status = 'open';

-- A board answer's body is not kept (thousands of boards a day); its size and hash are.
-- store.AttachFetchBody keeps one when its parser broke. NULL: a row from before this column.
ALTER TABLE http_fetch ADD COLUMN body_size int;

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
ALTER TABLE http_fetch DROP COLUMN body_size;
DROP INDEX job_open_board_idx;
DROP TABLE board;
