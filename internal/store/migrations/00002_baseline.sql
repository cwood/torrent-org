-- +goose Up
-- A row means torrents present on first run were recorded in place. Later
-- unassigned torrents are new and get placed by the rules.
CREATE TABLE baseline (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  recorded_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE baseline;
