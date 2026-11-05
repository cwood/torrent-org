-- +goose Up
ALTER TABLE rules ADD COLUMN private BOOLEAN NOT NULL DEFAULT FALSE;

-- The one passcode that reveals private rules, as a bcrypt hash.
CREATE TABLE passcode (
  id   INTEGER PRIMARY KEY CHECK (id = 1),
  hash TEXT NOT NULL
);

-- +goose Down
DROP TABLE passcode;
ALTER TABLE rules DROP COLUMN private;
