-- +goose Up
-- The triage of the Leads list: a café lead's place type, the switch that holds roasteries
-- out of café outreach, and the link from an operator's skip to the call it wrote.
--
-- lead_place_type is append-only (UPDATE revoked from the app role, as on lead_label): every
-- answer is a row, and a lead's effective type is read from the latest row of each source,
-- operator before jev before name, with 'cafe' when none speaks (effectivePlaceTypeSQL).
-- 'roastery' covers roasteries and online-only coffee stores: neither has a café to sit in.
-- 'unclear' is Jev's alone: it stands for "asked, no answer", so input_sha256 stops a rerun
-- asking again while the name or the default stands. detail is the matched word or the
-- question's name.
CREATE TABLE lead_place_type (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  place_type text NOT NULL,
  source text NOT NULL,
  detail text NOT NULL DEFAULT '',
  input_sha256 bytea,                 -- what Jev was asked; jev rows only
  actor text NOT NULL,                -- as lead_event.actor
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_place_type_type_check CHECK (place_type IN ('cafe', 'roastery', 'unclear')),
  CONSTRAINT lead_place_type_source_check CHECK (source IN ('name', 'jev', 'operator')),
  CONSTRAINT lead_place_type_unclear_check CHECK (place_type <> 'unclear' OR source = 'jev'),
  CONSTRAINT lead_place_type_detail_check CHECK (detail !~ '[\r\n]' AND length(detail) <= 300),
  CONSTRAINT lead_place_type_sha_check CHECK (
    CASE WHEN source = 'jev' THEN coalesce(octet_length(input_sha256), 0) = 32 ELSE input_sha256 IS NULL END),
  CONSTRAINT lead_place_type_actor_check CHECK (actor IN ('gremy', 'system'))
);
CREATE INDEX lead_place_type_lead_idx ON lead_place_type (lead_id, id DESC);

-- roastery_outreach_switch is append-only, as job_switch (00023): every switch on and off is a
-- row, the latest row (by id) of an account is its switch, and no row means off, so a roastery
-- is held out of café sends until the operator says otherwise.
CREATE TABLE roastery_outreach_switch (
  id bigserial PRIMARY KEY,
  account_id bigint NOT NULL DEFAULT 1 REFERENCES account(id),
  is_on boolean NOT NULL,
  actor text NOT NULL,                -- as lead_event.actor
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT roastery_outreach_switch_actor_check CHECK (actor IN ('gremy', 'system'))
);
CREATE INDEX roastery_outreach_switch_account_idx ON roastery_outreach_switch (account_id, id DESC);

-- The call an operator's skip wrote, set when the skip's lead_event is inserted, so an undo
-- clears that call only while it is still the lead's latest, and no marker sits in a reason
-- the operator reads. lead_event stays append-only: the column is written at INSERT, which
-- the app role holds on the whole table, and never updated.
ALTER TABLE lead_event ADD COLUMN skip_label_id bigint REFERENCES lead_label(id);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
ALTER TABLE lead_event DROP COLUMN skip_label_id;
DROP TABLE roastery_outreach_switch;
DROP TABLE lead_place_type;
