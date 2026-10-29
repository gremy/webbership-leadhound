-- +goose Up
-- A place's map-listing emails and phones become routes and phone facts (`leads places
-- import overture`), after the same filters the contact search and research apply (a role
-- address on the café's own domain, not a public body, a phone in E.164 form, nothing
-- suppressed). Such a row was read from no page: it names the place row it came from
-- (place_id) instead of a fetch, and its derived_by is overture@1, so the console says where
-- it came from. Every other row keeps its fetch: exactly one of the two is set.
-- Both tables stay append-only (no UPDATE grant); the new column is fixed at insert.
SET LOCAL lock_timeout = '5s';

ALTER TABLE contact_route ALTER COLUMN fetch_id DROP NOT NULL;
ALTER TABLE contact_route ADD COLUMN place_id bigint REFERENCES place(id);
ALTER TABLE contact_route ADD CONSTRAINT contact_route_source_check CHECK (num_nonnulls(fetch_id, place_id) = 1);

ALTER TABLE org_fact ALTER COLUMN fetch_id DROP NOT NULL;
ALTER TABLE org_fact ADD COLUMN place_id bigint REFERENCES place(id);
ALTER TABLE org_fact ADD CONSTRAINT org_fact_source_check CHECK (num_nonnulls(fetch_id, place_id) = 1);

-- +goose Down
-- Runs as the owner only. A route or fact read from a place has no fetch: the NOT NULL
-- restore fails while one exists and the whole step rolls back, nothing changed.
SET LOCAL lock_timeout = '5s';
ALTER TABLE org_fact DROP CONSTRAINT org_fact_source_check;
ALTER TABLE org_fact DROP COLUMN place_id;
ALTER TABLE org_fact ALTER COLUMN fetch_id SET NOT NULL;
ALTER TABLE contact_route DROP CONSTRAINT contact_route_source_check;
ALTER TABLE contact_route DROP COLUMN place_id;
ALTER TABLE contact_route ALTER COLUMN fetch_id SET NOT NULL;
