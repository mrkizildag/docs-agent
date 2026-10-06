package llmrunner_test

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

// A nested file matched by a ** covers glob reaches triage with its doc and
// patch; a doc whose covers do not match is not triaged.
func TestStart_GlobCoveredNestedFileTriagesOnlyItsDoc(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	write("src/pkg/deep/x.go", "package deep\n\nfunc X() {}\n")
	write("other/y.go", "package other\n")
	write("docs/a.md", "---\ntitle: A\nsummary: Describes A.\ncovers:\n  - src/**/*.go\n---\n# A\n\nold.\n")
	write("docs/b.md", "---\ntitle: B\nsummary: Describes B.\ncovers:\n  - other/*.go\n---\n# B\n\nold.\n")
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	headSHA := run("rev-parse", "HEAD")

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){triageResponse(false)}}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(dir)

	const patch = "@@ -1,2 +1,3 @@\n package deep\n+func X() {}\n"
	req := review.Request{
		InstallationID: 1, Owner: "o", Repo: "r", Number: 1, HeadSHA: headSHA, BaseSHA: headSHA,
		ChangedFiles: []review.ChangedFile{{Path: "src/pkg/deep/x.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: patch}},
	}

	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	if _, ok := result.Verdict.(review.NoImpact); !ok {
		t.Fatalf("Verdict = %T, want review.NoImpact", result.Verdict)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model saw %d calls, want exactly 1 triage call (docs/a.md only)", len(model.calls))
	}
	prompt := model.calls[0].Messages[0].Text
	for _, want := range []string{"docs/a.md", patch} {
		if !strings.Contains(prompt, want) {
			t.Errorf("triage prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "docs/b.md") {
		t.Errorf("triage prompt mentions uncovered docs/b.md:\n%s", prompt)
	}
}
