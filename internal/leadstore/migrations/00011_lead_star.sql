-- +goose Up
-- Stars and the operator's outreach order.
--
-- lead_star is append-only (UPDATE revoked from the app role, as on lead_priority): every
-- star, unstar and move of a lead is a row and the latest per lead (by id) wins. A starred
-- lead's position orders it among the starred, smallest first; an unstar carries none.
-- leadstore's star writes run one at a time under a transaction-level advisory lock, so the
-- starred leads' positions stay distinct.
--
-- The lead_priority pin is retired: every lead whose latest lead_priority row is a pin
-- becomes a star at the top, the newest pin first, and gets a clear row, both by the system.
-- lead_priority keeps its defers.
CREATE TABLE lead_star (
  id bigserial PRIMARY KEY,
  lead_id bigint NOT NULL REFERENCES lead(id),
  action text NOT NULL,               -- star | unstar | place
  position bigint,                    -- NULL for an unstar only
  actor text NOT NULL,                -- operator | system
  at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT lead_star_action_check CHECK (action IN ('star', 'unstar', 'place')),
  CONSTRAINT lead_star_position_check CHECK ((action = 'unstar') = (position IS NULL)),
  CONSTRAINT lead_star_actor_check CHECK (actor IN ('operator', 'system'))
);

-- The latest row per lead.
CREATE INDEX lead_star_lead_idx ON lead_star (lead_id, id DESC);

-- One statement: both inserts read the pins as they stood before it.
WITH pinned AS (
  SELECT pr.lead_id, row_number() OVER (ORDER BY pr.id DESC) AS position
  FROM (SELECT DISTINCT ON (lead_id) lead_id, id, action FROM lead_priority ORDER BY lead_id, id DESC) pr
  WHERE pr.action = 'pin'
), stars AS (
  INSERT INTO lead_star (lead_id, action, position, actor)
  SELECT lead_id, 'star', position, 'system' FROM pinned ORDER BY position
)
INSERT INTO lead_priority (lead_id, action, actor)
SELECT lead_id, 'clear', 'system' FROM pinned ORDER BY position;

-- +goose Down
-- Runs as the owner only. Every starred lead is pinned again, the top star as the newest
-- pin; the stars go.
INSERT INTO lead_priority (lead_id, action, actor)
SELECT s.lead_id, 'pin', 'system'
FROM (SELECT DISTINCT ON (lead_id) lead_id, action, position FROM lead_star ORDER BY lead_id, id DESC) s
WHERE s.action <> 'unstar'
ORDER BY s.position DESC, s.lead_id DESC;
DROP TABLE lead_star;
