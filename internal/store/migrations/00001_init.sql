-- +goose Up
CREATE TABLE trackers (
  host TEXT PRIMARY KEY,
  name TEXT NOT NULL
);

CREATE TABLE rules (
  id       INTEGER PRIMARY KEY,
  name     TEXT    NOT NULL UNIQUE,
  color    TEXT    NOT NULL,
  priority INTEGER NOT NULL,
  mode     TEXT    NOT NULL CHECK (mode IN ('all', 'any')),
  path     TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE rule_conditions (
  id      INTEGER PRIMARY KEY,
  rule_id INTEGER NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
  field   TEXT    NOT NULL CHECK (field IN ('name', 'tracker', 'extension')),
  op      TEXT    NOT NULL CHECK (op IN ('is', 'contains', 'begins_with', 'ends_with')),
  value   TEXT    NOT NULL
);

CREATE INDEX rule_conditions_rule_id ON rule_conditions (rule_id);

-- A row means the torrent has been placed; rule_id is NULL when it landed in
-- the default location. Manual assignments are never re-evaluated.
CREATE TABLE assignments (
  hash    TEXT PRIMARY KEY,
  rule_id INTEGER REFERENCES rules (id) ON DELETE SET NULL,
  manual  BOOLEAN NOT NULL DEFAULT FALSE
);

-- +goose Down
DROP TABLE assignments;
DROP TABLE rule_conditions;
DROP TABLE rules;
DROP TABLE trackers;
