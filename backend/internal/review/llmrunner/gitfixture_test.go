package llmrunner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git runs git in dir and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this package
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initGitRepo creates an empty repo on main in a temp dir.
func initGitRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "test@example.com")
	git(t, dir, "config", "user.name", "test")
	return dir
}

func writeRepoFile(t *testing.T, dir, relPath, content string) {
	t.Helper()

	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", relPath, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
}

// commitAll commits every change in dir and returns the new commit's SHA.
func commitAll(t *testing.T, dir, message string) string {
	t.Helper()

	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
	return git(t, dir, "rev-parse", "HEAD")
}
