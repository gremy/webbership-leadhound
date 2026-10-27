-- +goose Up
-- A relevance number per lead from one question to a decider, and the evals that say whether
-- the number can be trusted to sort by.
--
-- Both tables are append-only (UPDATE revoked from the app role, as on lead_priority).
-- lead_relevance: every rating of a lead is a row and the latest per lead (by id) wins. The
-- input hash covers the decider, the question and the lead's facts as asked, so a lead is
-- asked again only when one of them changed. score is round(sum of p(option) x weight).
-- lead_relevance_eval: every eval of a question over a labelled set is a row and the latest
-- per question (by id) wins; trusted is whether it passed the gate. exact and within_one are
-- shares (0 to 1) of the labelled leads.
CREATE TABLE lead_relevance (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  question text NOT NULL,             -- e.g. 'relevance@1'
  decider text NOT NULL,              -- jev | fake
  model text NOT NULL DEFAULT '',     -- the model the response named
  input_sha256 bytea NOT NULL,
  band text NOT NULL,                 -- strong | likely | maybe | unlikely | no
  score smallint NOT NULL,
  probabilities jsonb NOT NULL,       -- option -> probability
  input_tokens int NOT NULL DEFAULT 0,
  run_id bigint NOT NULL REFERENCES run(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_relevance_score_check CHECK (score BETWEEN 0 AND 100),
  CONSTRAINT lead_relevance_band_check CHECK (band IN ('strong', 'likely', 'maybe', 'unlikely', 'no')),
  CONSTRAINT lead_relevance_sha_check CHECK (length(input_sha256) = 32),
  CONSTRAINT lead_relevance_probabilities_check CHECK (jsonb_typeof(probabilities) = 'object'),
  CONSTRAINT lead_relevance_tokens_check CHECK (input_tokens >= 0)
);
CREATE INDEX lead_relevance_lead_idx ON lead_relevance (lead_id, id DESC);

CREATE TABLE lead_relevance_eval (
  id bigserial PRIMARY KEY,
  question text NOT NULL,
  model text NOT NULL DEFAULT '',
  n int NOT NULL,                     -- labelled leads
  exact double precision NOT NULL,    -- share whose band matched the label
  within_one double precision NOT NULL, -- share within one band of the label
  mae double precision NOT NULL,      -- mean absolute error against the label weights
  spearman double precision NOT NULL, -- Spearman's rho, the mean over the runs
  runs int NOT NULL,
  trusted boolean NOT NULL,
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_relevance_eval_n_check CHECK (n >= 0 AND runs >= 1),
  CONSTRAINT lead_relevance_eval_share_check CHECK (exact BETWEEN 0 AND 1 AND within_one BETWEEN 0 AND 1),
  CONSTRAINT lead_relevance_eval_mae_check CHECK (mae >= 0 AND mae <= 100),
  CONSTRAINT lead_relevance_eval_spearman_check CHECK (spearman BETWEEN -1 AND 1)
);
CREATE INDEX lead_relevance_eval_question_idx ON lead_relevance_eval (question, id DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE lead_relevance_eval;
DROP TABLE lead_relevance;
