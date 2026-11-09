-- +goose Up
-- What a lead's business is: Jev's pick from a closed list (question business-kind@<n>), for a
-- line in the Leads list. It reaches no email and no model.
--
-- lead_business_kind: one answer per asked input, append-only; the latest row of a lead (by id)
--   decides, and a lead with no row has no kind. confidence is Jev's, input_sha256 the hash of
--   what Jev was asked, so an unchanged input is not asked again. The first seven kinds are for
--   a café lead, the next eight for a Drupal lead, and unclear is for both.
SET LOCAL lock_timeout = '5s';

CREATE TABLE lead_business_kind (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  kind text NOT NULL,
  confidence real NOT NULL,
  question text NOT NULL,
  input_sha256 bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_business_kind_kind_check CHECK (kind IN (
    'specialty_coffee', 'neighbourhood_cafe', 'bakery_cafe', 'restaurant_cafe', 'bar_lounge', 'venue_cafe', 'roastery_shop',
    'education', 'government', 'healthcare', 'nonprofit', 'company', 'media', 'agency', 'personal',
    'unclear')),
  CONSTRAINT lead_business_kind_question_check CHECK (question ~ '^business-kind@[0-9]+$'),
  CONSTRAINT lead_business_kind_confidence_check CHECK (confidence BETWEEN 0 AND 1)
);
CREATE INDEX lead_business_kind_lead_idx ON lead_business_kind (lead_id, id DESC);

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    REVOKE UPDATE, DELETE, TRUNCATE ON public.lead_business_kind FROM leadhound_app;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Runs as the owner only. The rows are derived and the next rate run rebuilds them, so the down
-- drops the table with no refusal.
SET LOCAL lock_timeout = '5s';

DROP TABLE lead_business_kind;
