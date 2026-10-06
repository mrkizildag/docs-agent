// Package sqlitetest opens throwaway SQLite stores for tests.
package sqlitetest

import (
	"path/filepath"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
)

// Open returns a Store backed by a fresh temp file, closed when the test ends.
func Open(t testing.TB) *sqlite.Store {
	t.Helper()

	store, _ := OpenPath(t)
	return store
}

// OpenPath is Open that also returns the database file's path.
func OpenPath(t testing.TB) (*sqlite.Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) = %v, want nil error", path, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() = %v, want nil error", err)
		}
	})
	return store, path
}
