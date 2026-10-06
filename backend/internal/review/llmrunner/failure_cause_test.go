package llmrunner_test

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func TestStart_FailureNamesItsCause(t *testing.T) {
	t.Parallel()

	providerDown := func(llm.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("upstream 500: secret provider body")
	}
	overBudget := func(llm.Request) (llm.Response, error) {
		return llm.Response{Text: `{"impacted": false, "reason": "x"}`, Usage: llm.Usage{InputTokens: 100}}, nil
	}
	tests := []struct {
		name      string
		model     llm.Model
		badRemote bool
		extraDocs int
		budget    int
		timeout   time.Duration
		want      review.FailureCause
	}{
		{name: "provider error", model: &fakeModel{script: []func(llm.Request) (llm.Response, error){providerDown}}, want: review.CauseProvider},
		{name: "clone failure", model: &fakeModel{}, badRemote: true, want: review.CauseClone},
		{name: "token limit", model: &fakeModel{script: []func(llm.Request) (llm.Response, error){overBudget}}, budget: 10, want: review.CauseLimit},
		{name: "too many candidate docs", model: &fakeModel{}, extraDocs: 10, want: review.CauseTooManyCandidates},
		{name: "timeout", model: blockingModel{}, timeout: 200 * time.Millisecond, want: review.CauseTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoDir, headSHA := newGitRepo(t)
			if tc.extraDocs != 0 {
				headSHA = commitCoveringDocs(t, repoDir, tc.extraDocs)
			}
			runner := llmrunner.New(tc.model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
			runner.SetRemote(repoDir)
			if tc.badRemote {
				runner.SetRemote(filepath.Join(t.TempDir(), "missing.git"))
			}
			if tc.budget != 0 {
				runner.SetTokenBudget(tc.budget)
			}
			if tc.timeout != 0 {
				runner.SetTimeout(tc.timeout)
			}

			_, err := runner.Start(t.Context(), testRequest(headSHA))
			var failed *review.FailedError
			if !errors.As(err, &failed) || failed.Cause != tc.want {
				t.Fatalf("Start() = %v, want a *review.FailedError with cause %q", err, tc.want)
			}
		})
	}
}

// commitCoveringDocs commits n more docs that cover main.go on top of dir's
// HEAD and returns the new commit's SHA.
func commitCoveringDocs(t *testing.T, dir string, n int) string {
	t.Helper()

	git := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return out
	}

	for i := range n {
		doc := fmt.Sprintf("---\ntitle: Extra %d\nsummary: Describes extra %d.\ncovers:\n  - main.go\n---\n# Extra %d\n", i, i, i)
		if err := os.WriteFile(filepath.Join(dir, "docs", fmt.Sprintf("extra%d.md", i)), []byte(doc), 0o600); err != nil {
			t.Fatalf("write extra doc %d: %v", i, err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "more docs")
	return strings.TrimSpace(string(git("rev-parse", "HEAD")))
}
