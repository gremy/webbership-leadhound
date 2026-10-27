-- +goose Up
-- The operator's own call on a lead: the band he would give it, the answer key the relevance
-- eval grades the decider against (D9's trust gate).
--
-- lead_label is append-only (UPDATE revoked from the app role, as on lead_relevance): every
-- call is a row and the latest per lead (by id) wins. band NULL is a cleared call, and then
-- input is NULL too. input is the lead's facts exactly as the relevance question read them
-- when the call was made (leadrate.Input as JSON), so the eval asks about what the operator
-- saw, not what the lead says today.
CREATE TABLE lead_label (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  band text,                          -- strong | likely | maybe | unlikely | no; NULL: cleared
  input jsonb,                        -- leadrate.Input at the time of the call; NULL when cleared
  actor text NOT NULL,                -- operator | system
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_label_band_check CHECK (band IS NULL OR band IN ('strong', 'likely', 'maybe', 'unlikely', 'no')),
  CONSTRAINT lead_label_input_check CHECK ((band IS NULL) = (input IS NULL)
    AND (input IS NULL OR jsonb_typeof(input) = 'object')),
  CONSTRAINT lead_label_actor_check CHECK (actor IN ('operator', 'system'))
);
CREATE INDEX lead_label_lead_idx ON lead_label (lead_id, id DESC);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE lead_label;
