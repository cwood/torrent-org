// Package store persists rules, tracker aliases and torrent assignments in
// SQLite. Torrent state itself is never stored; Transmission owns it.
package store

//go:generate go tool sqlc generate

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/cwood/torrent-org/internal/rules"
	"github.com/cwood/torrent-org/internal/store/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when updating a row that does not exist.
var ErrNotFound = errors.New("not found")

// Store is the SQLite-backed persistence layer.
type Store struct {
	db *sql.DB
	q  *db.Queries
}

// Assignment records which rule placed a torrent. RuleID is 0 when the
// torrent went to the default location. Manual assignments were chosen by
// the user and are never re-evaluated.
type Assignment struct {
	Hash   string
	RuleID int64
	Manual bool
}

// Open opens or creates the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database dir: %w", err)
	}
	// Pragmas in the DSN are applied to every pooled connection; foreign_keys
	// is per-connection and off by default in SQLite.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := migrate(ctx, sqlDB); err != nil {
		return nil, errors.Join(err, sqlDB.Close())
	}
	return &Store{db: sqlDB, q: db.New(sqlDB)}, nil
}

// provider runs the embedded migrations against sqlDB.
func provider(sqlDB *sql.DB) (*goose.Provider, error) {
	fsys, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	p, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, fsys)
	if err != nil {
		return nil, fmt.Errorf("create migration provider: %w", err)
	}
	return p, nil
}

func migrate(ctx context.Context, sqlDB *sql.DB) error {
	p, err := provider(sqlDB)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Rules returns all rules with their conditions, ordered by name.
func (s *Store) Rules(ctx context.Context) ([]rules.Rule, error) {
	rows, err := s.q.ListRules(ctx)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	out := make([]rules.Rule, 0, len(rows))
	for _, r := range rows {
		rule := rules.Rule{ID: r.ID, Name: r.Name, Color: r.Color, Path: r.Path, Private: r.Private}
		if err := json.Unmarshal([]byte(r.Conditions), &rule.Match); err != nil {
			return nil, fmt.Errorf("rule %d conditions: %w", r.ID, err)
		}
		out = append(out, rule)
	}
	return out, nil
}

// SaveRule creates r when r.ID is 0 and updates it otherwise, replacing its
// conditions. It returns the rule's ID.
func (s *Store) SaveRule(ctx context.Context, r rules.Rule) (int64, error) {
	conds, err := json.Marshal(r.Match)
	if err != nil {
		return 0, fmt.Errorf("encode conditions: %w", err)
	}
	if r.ID == 0 {
		id, err := s.q.CreateRule(ctx, db.CreateRuleParams{
			Name: r.Name, Color: r.Color, Path: r.Path, Conditions: string(conds), Private: r.Private,
		})
		if err != nil {
			return 0, fmt.Errorf("create rule: %w", err)
		}
		return id, nil
	}
	n, err := s.q.UpdateRule(ctx, db.UpdateRuleParams{
		Name: r.Name, Color: r.Color, Path: r.Path, Conditions: string(conds), Private: r.Private, ID: r.ID,
	})
	if err != nil {
		return 0, fmt.Errorf("update rule: %w", err)
	}
	if n == 0 {
		return 0, fmt.Errorf("rule %d: %w", r.ID, ErrNotFound)
	}
	return r.ID, nil
}

// DeleteRule deletes a rule and its conditions. Torrents it placed fall back
// to the default location in their assignments but are not moved.
func (s *Store) DeleteRule(ctx context.Context, id int64) error {
	return s.q.DeleteRule(ctx, id)
}

// Passcode returns the bcrypt hash of the passcode that reveals private rules,
// and whether one is set.
func (s *Store) Passcode(ctx context.Context) (string, bool, error) {
	hash, err := s.q.GetPasscode(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get passcode: %w", err)
	}
	return hash, true, nil
}

// SetPasscode stores the bcrypt hash of the passcode.
func (s *Store) SetPasscode(ctx context.Context, hash string) error {
	return s.q.SetPasscode(ctx, hash)
}

// ClearPasscode removes the passcode, so the next unlock chooses a new one.
// Private rules stay private.
func (s *Store) ClearPasscode(ctx context.Context) error {
	return s.q.ClearPasscode(ctx)
}

// Trackers returns the announce host to directory name mapping.
func (s *Store) Trackers(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.ListTrackers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list trackers: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Host] = r.Name
	}
	return out, nil
}

// SetTracker maps an announce host to a directory name.
func (s *Store) SetTracker(ctx context.Context, host, name string) error {
	return s.q.UpsertTracker(ctx, db.UpsertTrackerParams{Host: host, Name: name})
}

// DeleteTracker removes a host mapping.
func (s *Store) DeleteTracker(ctx context.Context, host string) error {
	return s.q.DeleteTracker(ctx, host)
}

// Assignments returns all assignments keyed by torrent hash.
func (s *Store) Assignments(ctx context.Context) (map[string]Assignment, error) {
	rows, err := s.q.ListAssignments(ctx)
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}
	out := make(map[string]Assignment, len(rows))
	for _, r := range rows {
		out[r.Hash] = Assignment{Hash: r.Hash, RuleID: r.RuleID.Int64, Manual: r.Manual}
	}
	return out, nil
}

// Assign records or replaces a torrent's assignment.
func (s *Store) Assign(ctx context.Context, a Assignment) error {
	return s.q.UpsertAssignment(ctx, db.UpsertAssignmentParams{
		Hash:   a.Hash,
		RuleID: sql.NullInt64{Int64: a.RuleID, Valid: a.RuleID != 0},
		Manual: a.Manual,
	})
}

// Unassign forgets a torrent, typically after it is removed from
// Transmission.
func (s *Store) Unassign(ctx context.Context, hash string) error {
	return s.q.DeleteAssignment(ctx, hash)
}

// HasBaseline reports whether [Store.RecordBaseline] has run.
func (s *Store) HasBaseline(ctx context.Context) (bool, error) {
	return s.q.HasBaseline(ctx)
}

// RecordBaseline assigns hashes to the default location without placing
// them and marks the baseline as taken, atomically.
func (s *Store) RecordBaseline(ctx context.Context, hashes []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rollback after a successful Commit returns sql.ErrTxDone and is harmless.
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)

	for _, h := range hashes {
		if err := q.UpsertAssignment(ctx, db.UpsertAssignmentParams{Hash: h}); err != nil {
			return fmt.Errorf("record %s: %w", h, err)
		}
	}
	if err := q.RecordBaseline(ctx); err != nil {
		return fmt.Errorf("record baseline: %w", err)
	}
	return tx.Commit()
}
