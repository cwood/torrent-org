-- +goose Up
-- Rules are independent: the most specific matching rule wins, so their
-- order no longer means anything.
ALTER TABLE rules DROP COLUMN priority;

-- +goose Down
ALTER TABLE rules ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
UPDATE rules SET priority = id;
