-- +goose Up
-- Grounded Claude drafting (CP4): every draft Claude writes, the operator's review of it, and
-- the evals the trust gate reads. All three tables are append-only (UPDATE revoked from the
-- app role; DELETE was never granted).
--
-- draft_generation: one attempt at writing one lead's sequence.
--   attempt: 1 for the first try, 2 for the one retry with the failure codes.
--   model: the model the response named (the fake decider names its own).
--   fact_sha256, facts: the lead's fact sheet as the model saw it (outreach.FactSheet,
--     JSON) and the hex SHA-256 of its canonical bytes; a batch reuses a stored generation
--     only when the hash matches (leadstore.LatestGenerationForLead).
--   output: the model's answer, NULL when it was not valid JSON (check_ok is then false).
--   check_ok, check_failures: the grounding check's verdict; the failures (outreach.Failure,
--     a JSON array) are empty exactly when check_ok is true.
--   cost_micros: the attempt's priced spend; NULL when unknown. claude.Capped returns no
--     ai_usage row id, so none is linked (the ai_usage rows hold the ledger, as on
--     contact_research, 00018).
--   purpose: batch (drafted for a batch), sample (leads drafts sample, review only) or eval
--     (leads drafts eval).
-- draft_review: the operator's verdict on one generation; the latest per generation (by id)
--   wins. A reject carries at least one reason from the closed set, an approve none; the
--   reason other needs a note. Only a generation that passed the check may be reviewed
--   (leadstore.RecordReview refuses the rest).
-- draft_eval: one recorded eval of a campaign's drafting, the latest per campaign (by id)
--   wins. first_try_pass and after_retry_pass are shares (0 to 1) of the runs; failures
--   maps a grounding code to its count, mean_words a step to its mean words.
CREATE TABLE draft_generation (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  campaign_id bigint NOT NULL REFERENCES campaign(id),
  attempt smallint NOT NULL,
  model text NOT NULL,
  fact_sha256 text NOT NULL,
  facts jsonb NOT NULL,
  output jsonb,                         -- NULL: the answer was not valid JSON
  check_ok boolean NOT NULL,
  check_failures jsonb NOT NULL DEFAULT '[]',
  cost_micros bigint,                   -- NULL: unknown
  purpose text NOT NULL,                -- batch | sample | eval
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT draft_generation_attempt_check CHECK (attempt BETWEEN 1 AND 2),
  CONSTRAINT draft_generation_model_check
    CHECK (char_length(model) BETWEEN 1 AND 100 AND model !~ '[\r\n]'),
  CONSTRAINT draft_generation_fact_sha_check CHECK (fact_sha256 ~ '^[0-9a-f]{64}$'),
  CONSTRAINT draft_generation_facts_check CHECK (jsonb_typeof(facts) = 'object'),
  CONSTRAINT draft_generation_failures_check CHECK (jsonb_typeof(check_failures) = 'array'),
  -- Passing means no failures, and a passing answer was valid JSON.
  CONSTRAINT draft_generation_ok_check CHECK (
    check_ok = (jsonb_array_length(check_failures) = 0) AND (output IS NOT NULL OR NOT check_ok)),
  CONSTRAINT draft_generation_cost_check CHECK (cost_micros IS NULL OR cost_micros >= 0),
  CONSTRAINT draft_generation_purpose_check CHECK (purpose IN ('batch', 'sample', 'eval'))
);
-- A lead's attempts, newest first (the lead page, the batch's reuse by fact hash).
CREATE INDEX draft_generation_lead_idx ON draft_generation (lead_id, campaign_id, id DESC);
-- A campaign's samples and evals, newest first (the Drafts tab).
CREATE INDEX draft_generation_campaign_idx ON draft_generation (campaign_id, purpose, id DESC);

CREATE TABLE draft_review (
  id bigserial PRIMARY KEY,
  generation_id bigint NOT NULL REFERENCES draft_generation(id),
  verdict text NOT NULL,                -- approve | reject
  reasons text[] NOT NULL DEFAULT '{}', -- subset of the closed set; empty on approve
  note text NOT NULL DEFAULT '',
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT draft_review_verdict_check CHECK (verdict IN ('approve', 'reject')),
  CONSTRAINT draft_review_reasons_check CHECK (
    reasons <@ ARRAY['wrong_tone', 'too_long', 'unclear', 'factual', 'other']::text[]
    AND array_position(reasons, NULL) IS NULL
    AND ((verdict = 'approve' AND cardinality(reasons) = 0)
      OR (verdict = 'reject' AND cardinality(reasons) BETWEEN 1 AND 5))),
  CONSTRAINT draft_review_note_check CHECK (char_length(note) <= 500
    AND (NOT ('other' = ANY (reasons)) OR length(btrim(note)) > 0)),
  CONSTRAINT draft_review_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX draft_review_generation_idx ON draft_review (generation_id, id DESC);

CREATE TABLE draft_eval (
  id bigserial PRIMARY KEY,
  campaign_id bigint NOT NULL REFERENCES campaign(id),
  runs int NOT NULL,
  reviewed int NOT NULL,                -- reviewed leads regenerated
  first_try_pass double precision NOT NULL,
  after_retry_pass double precision NOT NULL,
  failures jsonb NOT NULL DEFAULT '{}', -- grounding code -> count
  mean_words jsonb NOT NULL DEFAULT '{}', -- step -> mean words
  tokens bigint NOT NULL DEFAULT 0,
  cost_micros bigint NOT NULL DEFAULT 0,
  decider text NOT NULL,                -- claude | fake
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT draft_eval_counts_check CHECK (runs >= 1 AND reviewed >= 0 AND tokens >= 0 AND cost_micros >= 0),
  CONSTRAINT draft_eval_share_check CHECK (first_try_pass BETWEEN 0 AND 1 AND after_retry_pass BETWEEN 0 AND 1
    AND after_retry_pass >= first_try_pass),
  CONSTRAINT draft_eval_json_check CHECK (jsonb_typeof(failures) = 'object' AND jsonb_typeof(mean_words) = 'object'),
  CONSTRAINT draft_eval_decider_check CHECK (decider IN ('claude', 'fake'))
);
CREATE INDEX draft_eval_campaign_idx ON draft_eval (campaign_id, id DESC);

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE, DELETE, TRUNCATE ON public.draft_generation FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.draft_review FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.draft_eval FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. Every generation, review and eval goes with its table; a message
-- drafted from a generation keeps its text and its slots (which name the generation id).
SET LOCAL lock_timeout = '5s';
DROP TABLE draft_eval;
DROP TABLE draft_review;
DROP TABLE draft_generation;
