-- +goose Up
-- Append-only where facts live. The only UPDATEs are on state pointers: company.linkedin_slug,
-- job.last_seen_at, job.listing_status, job.status_checked_at, rule.disabled_at (+
-- disabled_reason), run.finished_at plus its counters and stop_reason/note, and
-- brief_log.sent_at + provider_id. The app role has no DELETE (deploy/sql/grants.sql).

-- run: one row per command invocation; collector health for the brief.
CREATE TABLE run (
  id bigserial PRIMARY KEY,
  command text NOT NULL,            -- collect|score|brief|recheck|canary
  source text NOT NULL DEFAULT '',  -- linkedin|greenhouse|lever|ashby|''
  started_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz,
  requests int NOT NULL DEFAULT 0,
  failures int NOT NULL DEFAULT 0,
  stop_reason text NOT NULL DEFAULT '' CHECK (stop_reason IN ('', 'throttled', 'blocked', 'failure_rate', 'budget', 'parser_broke', 'spend_cap')),
  note text NOT NULL DEFAULT ''
);

-- http_fetch: every HTTP response kept as it arrived (FETCH alone is a reserved word in Postgres).
CREATE TABLE http_fetch (
  id bigserial PRIMARY KEY,
  run_id bigint NOT NULL REFERENCES run(id),
  source text NOT NULL,
  kind text NOT NULL CHECK (kind IN ('search', 'detail', 'board', 'canary', 'company', 'site', 'robots')),
  url text NOT NULL,
  final_url text NOT NULL DEFAULT '', -- after redirects ('' = same as url)
  status int NOT NULL,              -- 0 = transport error
  fetched_at timestamptz NOT NULL DEFAULT now(),
  body bytea NOT NULL DEFAULT ''::bytea, -- exactly as it arrived; parsers decode
  content_type text NOT NULL DEFAULT '',
  body_sha256 bytea,
  error text NOT NULL DEFAULT ''
);

CREATE TABLE company (
  id bigserial PRIMARY KEY,
  name text NOT NULL,
  norm_name text NOT NULL UNIQUE,   -- lower, trimmed, punctuation-collapsed
  source_url text NOT NULL DEFAULT '',
  linkedin_slug text NOT NULL DEFAULT '', -- from the card/detail company href
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE job (
  id bigserial PRIMARY KEY,
  source text NOT NULL,
  source_job_id text NOT NULL,
  company_id bigint NOT NULL REFERENCES company(id),
  title text NOT NULL,
  url text NOT NULL,                -- canonical, query stripped, www host
  first_seen_at timestamptz NOT NULL DEFAULT now(),
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  listing_status text NOT NULL DEFAULT 'open' CHECK (listing_status IN ('open', 'expired', 'unreachable')),
  status_checked_at timestamptz,
  UNIQUE (source, source_job_id)
);

-- job_observation: facts derived from one fetch (provenance = fetch_id + derived_by).
CREATE TABLE job_observation (
  id bigserial PRIMARY KEY,
  job_id bigint NOT NULL REFERENCES job(id),
  fetch_id bigint NOT NULL REFERENCES http_fetch(id),
  observed_at timestamptz NOT NULL DEFAULT now(),
  derived_by text NOT NULL,         -- e.g. 'linkedin-parser@1'
  title text NOT NULL,
  location_text text NOT NULL DEFAULT '',
  description_clean text NOT NULL DEFAULT '',
  guard_findings jsonb NOT NULL DEFAULT '{}',
  lang text NOT NULL DEFAULT '',
  lang_confidence real NOT NULL DEFAULT 0,
  seniority text NOT NULL DEFAULT '',
  employment_type text NOT NULL DEFAULT '',
  industries text NOT NULL DEFAULT '',
  applicant_count int,              -- NULL when unknown; 200 for "Over 200"; 25 for "Be among the first 25"
  applicant_text text NOT NULL DEFAULT '',
  posted_text text NOT NULL DEFAULT '',
  posted_at_est timestamptz,
  apply_type text NOT NULL DEFAULT '' CHECK (apply_type IN ('', 'offsite', 'onsite', 'simple', 'ats')),
  apply_url text NOT NULL DEFAULT '',
  closed boolean NOT NULL DEFAULT false
);

CREATE TABLE gate_result (
  id bigserial PRIMARY KEY,
  job_id bigint NOT NULL REFERENCES job(id),
  observation_id bigint NOT NULL REFERENCES job_observation(id),
  gate text NOT NULL CHECK (gate IN ('language', 'location', 'work_rights', 'workplace', 'rule')),
  verdict text NOT NULL CHECK (verdict IN ('pass', 'fail', 'flag')),
  reason text NOT NULL,
  rule_id bigint,                   -- set when a stored rule decided it
  derived_by text NOT NULL,         -- 'gates@1'
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE decision (
  id bigserial PRIMARY KEY,
  job_id bigint NOT NULL REFERENCES job(id),
  observation_id bigint NOT NULL REFERENCES job_observation(id),
  decider text NOT NULL,            -- 'jev-1.13.0' or 'fake'
  criterion text NOT NULL CHECK (criterion IN ('title', 'stack', 'seniority', 'location', 'work_rights')),
  verdict text NOT NULL CHECK (verdict IN ('pass', 'fail', 'unclear')),
  confidence real NOT NULL,
  probabilities jsonb NOT NULL,
  reason text NOT NULL,             -- code-built
  input_sha256 bytea NOT NULL,      -- hash of the exact state sent
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE rule (
  id bigserial PRIMARY KEY,
  kind text NOT NULL CHECK (kind IN ('skip', 'flag')),
  field text NOT NULL CHECK (field IN ('company', 'title', 'description', 'location')),
  pattern text NOT NULL,            -- Go RE2, case-insensitive, validated on insert
  reason text NOT NULL,
  created_from_job_id bigint REFERENCES job(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  disabled_at timestamptz,
  disabled_reason text NOT NULL DEFAULT ''
);

CREATE TABLE status_event (
  id bigserial PRIMARY KEY,
  job_id bigint NOT NULL REFERENCES job(id),
  status text NOT NULL CHECK (status IN ('new', 'shortlisted', 'applied', 'interviewing', 'offer', 'rejected', 'skipped', 'withdrawn')),
  reason text NOT NULL DEFAULT '',
  rule_id bigint REFERENCES rule(id),
  actor text NOT NULL CHECK (actor IN ('gremy', 'system')),
  at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ai_usage (
  id bigserial PRIMARY KEY,
  provider text NOT NULL, model text NOT NULL,
  day date NOT NULL,                -- in LEADHOUND_TZ
  calls int NOT NULL, input_tokens int NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE brief_log (
  id bigserial PRIMARY KEY,
  day date NOT NULL UNIQUE,         -- one brief per day, enforced
  sent_at timestamptz,
  provider_id text NOT NULL DEFAULT '',
  subject text NOT NULL,
  body_text text NOT NULL,
  job_ids bigint[] NOT NULL
);

CREATE INDEX job_listing_status_idx ON job (listing_status);
CREATE INDEX job_observation_job_observed_idx ON job_observation (job_id, observed_at DESC);
CREATE INDEX decision_job_created_idx ON decision (job_id, created_at DESC);
CREATE INDEX status_event_job_at_idx ON status_event (job_id, at DESC);
CREATE INDEX http_fetch_run_idx ON http_fetch (run_id);
CREATE INDEX gate_result_job_created_idx ON gate_result (job_id, created_at DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE brief_log;
DROP TABLE ai_usage;
DROP TABLE status_event;
DROP TABLE rule;
DROP TABLE decision;
DROP TABLE gate_result;
DROP TABLE job_observation;
DROP TABLE job;
DROP TABLE company;
DROP TABLE http_fetch;
DROP TABLE run;
