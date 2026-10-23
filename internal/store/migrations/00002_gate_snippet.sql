-- +goose Up
-- The words of the posting a gate's verdict rests on (at most 120 runes, from the cleaned
-- description), so the brief and the decider can quote them. Column privileges follow the
-- table grants, so the app role can INSERT it like any other gate_result column.
ALTER TABLE gate_result ADD COLUMN snippet text NOT NULL DEFAULT '';

-- +goose Down
-- Runs as the owner only; the app role cannot alter.
ALTER TABLE gate_result DROP COLUMN snippet;
