-- +goose Up
-- Three more ATSs in the board registry: SmartRecruiters, Teamtailor and Personio. A
-- Personio board names its host's region: '' for .jobs.personio.de, 'com' for
-- .jobs.personio.com (Lever's 'eu' stays).
ALTER TABLE board
  DROP CONSTRAINT board_ats_check,
  ADD CONSTRAINT board_ats_check CHECK (ats IN ('greenhouse', 'lever', 'ashby', 'smartrecruiters', 'teamtailor', 'personio')),
  DROP CONSTRAINT board_region_check,
  ADD CONSTRAINT board_region_check CHECK (region IN ('', 'eu', 'com'));

-- +goose Down
-- Runs as the owner only. It fails while a board of the three ATSs, or one in region
-- 'com', is still in the table: those rows are the operator's to move first.
ALTER TABLE board
  DROP CONSTRAINT board_region_check,
  ADD CONSTRAINT board_region_check CHECK (region IN ('', 'eu')),
  DROP CONSTRAINT board_ats_check,
  ADD CONSTRAINT board_ats_check CHECK (ats IN ('greenhouse', 'lever', 'ashby'));
