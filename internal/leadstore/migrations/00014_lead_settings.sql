-- +goose Up
-- The leads settings the operator changes in the console, over leads.yaml.
--
-- lead_setting is append-only (UPDATE revoked from the app role, as on priority_profile):
-- every save is a row and the latest (by id) wins. settings is a partial document of the
-- knobs changed (leadconfig types it); a NULL is a reset to the file's values, as is no row
-- at all. The never-contact countries and the fallback order are not here: they stay in
-- priority_profile, one store per knob.
CREATE TABLE lead_setting (
  id bigserial PRIMARY KEY,
  settings jsonb,                     -- NULL: back to the file's values
  actor text NOT NULL,                -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_setting_actor_check CHECK (actor IN ('operator', 'system')),
  CONSTRAINT lead_setting_object_check CHECK (settings IS NULL OR jsonb_typeof(settings) = 'object')
);

-- +goose Down
-- Runs as the owner only; the app role cannot drop.
DROP TABLE lead_setting;
