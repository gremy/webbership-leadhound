-- +goose Up
-- Phones (CP5): screening numbers against the do-not-call registers, whether a stored phone
-- number works, and where a researched number came from. All three tables are append-only
-- (UPDATE revoked from the app role; DELETE was never granted). Only the owner deletes, and
-- only to erase an org (leadstore.ForgetOrg, run with the owner DSN like migrate).
--
-- phone_screening: one result of screening one number against one register.
--   number: E.164 ("+" and 8 to 15 digits), the form the register files use; a phone fact
--     stored in national form is matched through dial.Number with its org's country.
--   register: a register code of internal/compliance/countries.yaml; the CHECK list below
--     and leadstore.ScreenRegisters are kept equal to that file's registers by a test.
--   result: clean (not on the register) | listed. A listed number also gets a phone
--     suppression (leadstore.RecordScreening, same transaction), which only the operator
--     lifts, so a later clean result does not make it callable again.
--   checked_at: when the register was consulted; the latest checked_at (then id) per number
--     and register wins. Never more than a day after the row was written.
--   source: the base name of the result file (never a path), or '' for none.
-- phone_status: whether a stored phone fact works: good | bad. The latest row per fact (by
--   id) wins. reason: wrong_number (a call outcome said so; call_outcome_id names it),
--   bounced_call (the line did not ring through), operator (set by hand) or corrected (a
--   later outcome on the same call replaced a wrong_number; good, call_outcome_id names it).
--   fact_id is ON DELETE CASCADE: a status says something about one fact's number only, so
--   once the fact is erased (the owner deletes it in leadstore.ForgetOrg, or a retention
--   trim does) the status has nothing left to describe. Keeping it with a NULL fact (SET
--   NULL) would leave rows that answer no question and that no reader could join back; the
--   erasure's hashed phone suppressions are what keep the number from being called again.
--   The app role has no DELETE on org_fact, so it can never set the cascade off.
-- phone_research: one verified number phone research stored (CP3b's org_fact kind phone,
--   derived_by claude-research@1): the line it is (direct | decision_maker | switchboard,
--   '' when the answer did not say), the role printed beside it (a job title, never a name,
--   at most 60 characters), the model and the priced spend (cost_micros, NULL when unknown;
--   claude.Capped returns no ai_usage row id, as on contact_research, 00018).
--   fact_id is ON DELETE SET NULL (the plan's rule): the research row is the spend ledger's
--   companion and outlives a fact trimmed for retention; ForgetOrg deletes the org's rows
--   outright.
CREATE TABLE phone_screening (
  id bigserial PRIMARY KEY,
  number text NOT NULL,                 -- E.164
  register text NOT NULL,
  result text NOT NULL,                 -- clean | listed
  checked_at timestamptz NOT NULL,
  source text NOT NULL DEFAULT '',      -- result file base name
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT phone_screening_number_check CHECK (number ~ '^\+[0-9]{8,15}$'),
  CONSTRAINT phone_screening_register_check
    CHECK (register IN ('tps', 'ctps', 'dncm', 'ndd', 'dncr', 'rpo', 'robinson', 'gr_art11')),
  CONSTRAINT phone_screening_result_check CHECK (result IN ('clean', 'listed')),
  CONSTRAINT phone_screening_checked_check CHECK (checked_at <= at + interval '1 day'),
  CONSTRAINT phone_screening_source_check
    CHECK (char_length(source) <= 200 AND source !~ '[\r\n/\\]'),
  CONSTRAINT phone_screening_actor_check CHECK (actor IN ('operator', 'system'))
);
-- A number's latest result per register (the callable check, the export, the erasure).
CREATE INDEX phone_screening_number_idx ON phone_screening (number, register, checked_at DESC, id DESC);

CREATE TABLE phone_status (
  id bigserial PRIMARY KEY,
  fact_id bigint NOT NULL REFERENCES org_fact(id) ON DELETE CASCADE,
  status text NOT NULL,                 -- good | bad
  reason text NOT NULL,                 -- wrong_number | bounced_call | operator | corrected
  call_outcome_id bigint REFERENCES call_outcome(id),
  actor text NOT NULL,                  -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT phone_status_status_check CHECK (status IN ('good', 'bad')),
  CONSTRAINT phone_status_reason_check CHECK (
    (reason IN ('wrong_number', 'bounced_call') AND status = 'bad')
    OR (reason = 'corrected' AND status = 'good')
    OR reason = 'operator'),
  -- A call's verdict names its outcome; the operator's own does not.
  CONSTRAINT phone_status_outcome_check CHECK (
    (reason IN ('wrong_number', 'corrected') AND call_outcome_id IS NOT NULL)
    OR (reason = 'operator' AND call_outcome_id IS NULL)
    OR reason = 'bounced_call'),
  CONSTRAINT phone_status_actor_check CHECK (actor IN ('operator', 'system'))
);
-- A fact's latest status.
CREATE INDEX phone_status_fact_idx ON phone_status (fact_id, id DESC);
CREATE INDEX phone_status_call_outcome_idx ON phone_status (call_outcome_id) WHERE call_outcome_id IS NOT NULL;

CREATE TABLE phone_research (
  id bigserial PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES org(id),
  fact_id bigint REFERENCES org_fact(id) ON DELETE SET NULL,
  line_kind text NOT NULL DEFAULT '',   -- direct | decision_maker | switchboard | ''
  role text NOT NULL DEFAULT '',        -- a job title, never a name
  model text NOT NULL,
  cost_micros bigint,                   -- NULL: unknown
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT phone_research_line_kind_check CHECK (line_kind IN ('direct', 'decision_maker', 'switchboard', '')),
  CONSTRAINT phone_research_role_check CHECK (char_length(role) <= 60 AND role !~ '[\r\n]'),
  CONSTRAINT phone_research_model_check CHECK (char_length(model) BETWEEN 1 AND 100 AND model !~ '[\r\n]'),
  CONSTRAINT phone_research_cost_check CHECK (cost_micros IS NULL OR cost_micros >= 0)
);
CREATE INDEX phone_research_org_idx ON phone_research (org_id, id DESC);
-- The ON DELETE SET NULL of a fact finds its rows.
CREATE INDEX phone_research_fact_idx ON phone_research (fact_id) WHERE fact_id IS NOT NULL;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE, DELETE, TRUNCATE ON public.phone_screening FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.phone_status FROM leadhound_app;
    REVOKE UPDATE, DELETE, TRUNCATE ON public.phone_research FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. Every screening, status and research row goes with its table. The
-- phone suppressions a listed screening wrote stay (a suppression is never deleted), and the
-- phone facts research stored stay, as any finder's do.
SET LOCAL lock_timeout = '5s';
DROP TABLE phone_research;
DROP TABLE phone_status;
DROP TABLE phone_screening;
