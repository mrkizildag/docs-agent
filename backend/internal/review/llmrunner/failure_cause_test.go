package llmrunner_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm/llmtest"
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
		{name: "provider error", model: &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){providerDown}}, want: review.CauseProvider},
		{name: "clone failure", model: &llmtest.ScriptedModel{}, badRemote: true, want: review.CauseClone},
		{name: "token limit", model: &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){overBudget}}, budget: 10, want: review.CauseLimit},
		{name: "too many candidate docs", model: &llmtest.ScriptedModel{}, extraDocs: 10, want: review.CauseTooManyCandidates},
		{name: "timeout", model: blockingModel{}, timeout: 200 * time.Millisecond, want: review.CauseTimeout},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoDir, headSHA := newGitRepo(t)
			if tc.extraDocs != 0 {
				headSHA = commitCoveringDocs(t, repoDir, tc.extraDocs)
			}
			runner := llmrunner.New(tc.model, noToken, "triage-model", "draft-model")
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

func TestFailed_ClassifiesEachAgentCauseSeparately(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want review.FailureCause
	}{
		{name: "model failure", err: fmt.Errorf("step 1: %w: %w", agent.ErrModel, errors.New("500")), want: review.CauseProvider},
		{name: "malformed replies", err: fmt.Errorf("%w: %w", agent.ErrModel, agent.ErrMalformed), want: review.CauseProvider},
		{name: "step limit", err: fmt.Errorf("draft: %w", agent.ErrStepLimit), want: review.CauseLimit},
		{name: "token budget", err: fmt.Errorf("draft: %w", agent.ErrTokenBudget), want: review.CauseLimit},
		{name: "agent deadline", err: fmt.Errorf("draft: %w", agent.ErrDeadline), want: review.CauseTimeout},
		{name: "context deadline", err: fmt.Errorf("draft: %w", context.DeadlineExceeded), want: review.CauseTimeout},
		{name: "unclassified", err: errors.New("boom"), want: review.CauseInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := llmrunner.Failed(tc.err).Cause; got != tc.want {
				t.Errorf("Failed(%v).Cause = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func commitCoveringDocs(t *testing.T, dir string, n int) string {
	t.Helper()

	for i := range n {
		writeRepoFile(t, dir, fmt.Sprintf("docs/extra%d.md", i), fmt.Sprintf("---\ntitle: Extra %d\nsummary: Describes extra %d.\ncovers:\n  - main.go\n---\n# Extra %d\n", i, i, i))
	}
	return commitAll(t, dir, "more docs")
}
