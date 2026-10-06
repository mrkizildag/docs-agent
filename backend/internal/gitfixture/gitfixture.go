// Package gitfixture builds throwaway git repositories for tests.
package gitfixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NewRepo creates a repository on main in a temp dir, commits files as "init",
// and returns its directory and head SHA.
func NewRepo(t testing.TB, files map[string]string) (dir, sha string) {
	t.Helper()

	dir = t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "test")
	return dir, Commit(t, dir, files, "init")
}

// Commit writes files into dir, commits every change with msg, and returns the
// new commit's SHA.
func Commit(t testing.TB, dir string, files map[string]string, msg string) string {
	t.Helper()

	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return commitAll(t, dir, msg)
}

// Remove deletes paths from dir, commits the removal with msg, and returns the
// new commit's SHA.
func Remove(t testing.TB, dir, msg string, paths ...string) string {
	t.Helper()

	run(t, dir, append([]string{"rm", "-q"}, paths...)...)
	return commitAll(t, dir, msg)
}

func commitAll(t testing.TB, dir, msg string) string {
	t.Helper()

	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

func run(t testing.TB, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // fixture git args are literals in this package
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
