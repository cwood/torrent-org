-- name: ListRules :many
SELECT * FROM rules ORDER BY priority, id;

-- name: CreateRule :one
INSERT INTO rules (name, color, priority, path, conditions)
VALUES (?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateRule :execrows
UPDATE rules
SET name = ?, color = ?, priority = ?, path = ?, conditions = ?
WHERE id = ?;

-- name: DeleteRule :exec
DELETE FROM rules WHERE id = ?;

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
