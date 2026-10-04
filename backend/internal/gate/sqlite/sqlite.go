// Package sqlite is the SQLite-backed storage for pull request state and the
// durable job queue, using the pure-Go modernc.org/sqlite driver.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	msqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
)

var (
	_ gate.Store     = (*Store)(nil)
	_ jobqueue.Store = (*Store)(nil)
)

// Store is a SQLite-backed implementation of gate.Store and jobqueue.Store.
type Store struct {
	db *sql.DB
}

// Open opens (creating if absent) the SQLite database at path, configures WAL
// mode and a busy timeout, and applies any pending migrations.
//
// MaxOpenConns is set to 1: modernc.org/sqlite serializes writers at the
// driver level anyway, and a single connection gives Claim's
// read-then-update a simple atomicity guarantee without BEGIN IMMEDIATE.
func Open(ctx context.Context, path string) (*Store, error) {
	// DSN pragmas run on every new connection, busy_timeout first so the WAL switch
	// waits out a previous process still holding the file.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close sqlite store: %w", err)
	}
	return nil
}

func migrations() []string {
	return []string{
		`CREATE TABLE deliveries (
			delivery_id TEXT PRIMARY KEY,
			received_at TEXT NOT NULL
		)`,
		`CREATE TABLE jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT NOT NULL,
			kind TEXT NOT NULL,
			payload BLOB NOT NULL,
			state TEXT NOT NULL,
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE INDEX idx_jobs_state_key ON jobs(state, key)`,
		`CREATE TABLE pull_requests (
			owner TEXT NOT NULL,
			repo TEXT NOT NULL,
			number INTEGER NOT NULL,
			installation_id INTEGER NOT NULL,
			head_sha TEXT NOT NULL,
			PRIMARY KEY (owner, repo, number)
		)`,
		`ALTER TABLE jobs ADD COLUMN delivery_id TEXT NOT NULL DEFAULT '';
		CREATE INDEX idx_jobs_delivery_id ON jobs(delivery_id)`,
		`ALTER TABLE pull_requests ADD COLUMN check_run_id INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE pull_requests ADD COLUMN run_id INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE pull_requests ADD COLUMN run_nonce TEXT NOT NULL DEFAULT '';
		ALTER TABLE pull_requests ADD COLUMN run_deadline TEXT NOT NULL DEFAULT '';
		CREATE UNIQUE INDEX idx_pull_requests_run ON pull_requests(owner, repo, run_id) WHERE run_id != 0`,
		`ALTER TABLE pull_requests ADD COLUMN summary_comment_id INTEGER NOT NULL DEFAULT 0;
		CREATE TABLE pr_proposals (
			owner TEXT NOT NULL,
			repo TEXT NOT NULL,
			number INTEGER NOT NULL,
			position INTEGER NOT NULL,
			id TEXT NOT NULL,
			doc_path TEXT NOT NULL,
			section TEXT NOT NULL,
			comment_id INTEGER NOT NULL,
			comment_url TEXT NOT NULL,
			state TEXT NOT NULL,
			PRIMARY KEY (owner, repo, number, position),
			FOREIGN KEY (owner, repo, number) REFERENCES pull_requests(owner, repo, number) ON DELETE CASCADE
		)`,
	}
}

// migrate applies any migrations beyond the database's current user_version,
// so repeated Open calls against the same file are no-ops.
func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}

	steps := migrations()
	for i := version; i < len(steps); i++ {
		if err := applyMigration(ctx, db, i, steps[i]); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, index int, stmt string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", index+1, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("apply migration %d: %w", index+1, err)
	}

	// PRAGMA user_version does not accept bound parameters; index+1 is an
	// internal loop counter, never external input.
	pragma := fmt.Sprintf("PRAGMA user_version = %d", index+1) //nolint:gosec // internal counter, not user input
	if _, err := tx.ExecContext(ctx, pragma); err != nil {
		return fmt.Errorf("set user_version %d: %w", index+1, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", index+1, err)
	}
	return nil
}

// isUniqueViolation reports whether err is a SQLite primary key or unique
// constraint violation.
func isUniqueViolation(err error) bool {
	var sqliteErr *msqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() {
	case sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY, sqlite3.SQLITE_CONSTRAINT_UNIQUE:
		return true
	default:
		return false
	}
}
