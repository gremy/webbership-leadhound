-- +goose Up
-- A suppression recorded by mistake can be lifted by the operator: one suppression_lift row,
-- append-only like suppression itself (UPDATE revoked, no DELETE). The suppression row is never
-- changed. suppression_active is every suppression with no lift; everything that decides
-- whether an address or domain may be written to reads it.
-- This supersedes 00003's "never lifted": a suppression is lifted only by the operator, by id,
-- with a reason (FO6), and a lifted one stays in suppression as it was.
CREATE TABLE suppression_lift (
  id bigserial PRIMARY KEY,
  suppression_id bigint NOT NULL REFERENCES suppression(id),
  reason text NOT NULL,                 -- why it was wrong, in the operator's words
  actor text NOT NULL,                  -- operator
  source text NOT NULL,                 -- cli | console
  lifted_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT suppression_lift_once UNIQUE (suppression_id),
  -- one line, and never an address: the reason is printed by `leads suppress list` and the console
  CONSTRAINT suppression_lift_reason_check CHECK (length(btrim(reason)) BETWEEN 3 AND 300 AND reason !~ '[\r\n@]'),
  CONSTRAINT suppression_lift_actor_check CHECK (actor IN ('operator')),
  CONSTRAINT suppression_lift_source_check CHECK (source IN ('cli', 'console'))
);

-- The app role's default privileges reach views too, and a simple view is auto-updatable with
-- the owner's rights on suppression: leadstore.Migrate and deploy/sql/grants.sql revoke INSERT
-- and UPDATE on it.
CREATE VIEW suppression_active AS
  SELECT s.* FROM suppression s -- reads lifted rows on purpose: this is the one definition of active
  WHERE NOT EXISTS (SELECT 1 FROM suppression_lift l WHERE l.suppression_id = s.id);

-- +goose Down
DROP VIEW suppression_active;
DROP TABLE suppression_lift;
