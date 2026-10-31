-- name: ListRules :many
SELECT * FROM rules ORDER BY priority, id;

-- name: ListConditions :many
SELECT * FROM rule_conditions ORDER BY rule_id, id;

-- name: CreateRule :one
INSERT INTO rules (name, color, priority, mode, path)
VALUES (?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateRule :execrows
UPDATE rules
SET name = ?, color = ?, priority = ?, mode = ?, path = ?
WHERE id = ?;

-- name: DeleteRule :exec
DELETE FROM rules WHERE id = ?;

-- name: CreateCondition :exec
INSERT INTO rule_conditions (rule_id, field, op, value) VALUES (?, ?, ?, ?);

-- name: DeleteConditions :exec
DELETE FROM rule_conditions WHERE rule_id = ?;

-- name: ListTrackers :many
SELECT * FROM trackers ORDER BY host;

-- name: UpsertTracker :exec
INSERT INTO trackers (host, name) VALUES (?, ?)
ON CONFLICT (host) DO UPDATE SET name = excluded.name;

-- name: DeleteTracker :exec
DELETE FROM trackers WHERE host = ?;

-- name: ListAssignments :many
SELECT * FROM assignments;

-- name: UpsertAssignment :exec
INSERT INTO assignments (hash, rule_id, manual) VALUES (?, ?, ?)
ON CONFLICT (hash) DO UPDATE SET rule_id = excluded.rule_id, manual = excluded.manual;

-- name: DeleteAssignment :exec
DELETE FROM assignments WHERE hash = ?;

-- name: HasBaseline :one
SELECT EXISTS (SELECT 1 FROM baseline);

-- name: RecordBaseline :exec
INSERT OR IGNORE INTO baseline (id) VALUES (1);

-- name: SetRulePriority :exec
UPDATE rules SET priority = ? WHERE id = ?;
