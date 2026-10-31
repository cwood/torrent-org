-- +goose Up
-- Conditions become a tree stored as JSON on the rule: a top-level group with
-- a mode, conditions and nested groups. It is always read and written whole
-- with its rule, so a column is simpler than a table of groups.
ALTER TABLE rules ADD COLUMN conditions TEXT NOT NULL DEFAULT '{"mode":"all"}'
  CHECK (json_valid(conditions) AND json_extract(conditions, '$.mode') IN ('all', 'any', 'none'));

UPDATE rules SET conditions = json_object(
  'mode', mode,
  'conditions', json((
    SELECT json_group_array(json_object('field', field, 'op', op, 'value', value))
    FROM (SELECT field, op, value FROM rule_conditions c WHERE c.rule_id = rules.id ORDER BY c.id)
  ))
);

DROP TABLE rule_conditions;
ALTER TABLE rules DROP COLUMN mode;

-- +goose Down
-- Nested groups and "none" have no flat equivalent; only the top-level
-- conditions survive, and a "none" rule becomes "all".
ALTER TABLE rules ADD COLUMN mode TEXT NOT NULL DEFAULT 'all' CHECK (mode IN ('all', 'any'));
UPDATE rules SET mode = iif(json_extract(conditions, '$.mode') = 'any', 'any', 'all');

CREATE TABLE rule_conditions (
  id      INTEGER PRIMARY KEY,
  rule_id INTEGER NOT NULL REFERENCES rules (id) ON DELETE CASCADE,
  field   TEXT    NOT NULL CHECK (field IN ('name', 'tracker', 'extension')),
  op      TEXT    NOT NULL CHECK (op IN ('is', 'contains', 'begins_with', 'ends_with')),
  value   TEXT    NOT NULL
);
CREATE INDEX rule_conditions_rule_id ON rule_conditions (rule_id);

INSERT INTO rule_conditions (rule_id, field, op, value)
SELECT r.id, json_extract(c.value, '$.field'), json_extract(c.value, '$.op'), json_extract(c.value, '$.value')
FROM rules r, json_each(r.conditions, '$.conditions') c
ORDER BY r.id, c.key;

ALTER TABLE rules DROP COLUMN conditions;
