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

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() = %v, want nil error", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() = %v, want nil error", err)
		}
	})
	return store
}
