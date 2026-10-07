package llmrunner_test

import (
	"log/slog"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func withUsage(next func(llm.Request) (llm.Response, error), u llm.Usage) func(llm.Request) (llm.Response, error) {
	return func(req llm.Request) (llm.Response, error) {
		resp, err := next(req)
		resp.Usage = u
		return resp, err
	}
}

func startFull(t *testing.T, model llm.Model) review.Result {
	t.Helper()
	repoDir, headSHA := newGitRepo(t)
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	started, err := runner.Start(t.Context(), testRequest(headSHA))
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	return result
}

func TestStart_ReportsUsageAndProducingModel(t *testing.T) {
	t.Parallel()

	t.Run("proposals sum triage, draft and verify", func(t *testing.T) {
		t.Parallel()
		result := startFull(t, &fakeModel{script: []func(llm.Request) (llm.Response, error){
			withUsage(triageResponse(true), llm.Usage{InputTokens: 10, OutputTokens: 1, CacheReadTokens: 2}),
			withUsage(submitResponse(proposalFor("docs/x.md", 2)), llm.Usage{InputTokens: 20, OutputTokens: 5, CacheWriteTokens: 3}),
			withUsage(verifyResponse(true), llm.Usage{InputTokens: 7, OutputTokens: 1}),
		}})
		want := &review.Usage{Tokens: &review.Tokens{Input: 37, Output: 7, CacheRead: 2, CacheWrite: 3}}
		if diff := cmp.Diff(want, result.Usage); diff != "" {
			t.Errorf("Usage (-want +got):\n%s", diff)
		}
		if result.Model != "draft-model" {
			t.Errorf("Model = %q, want draft-model", result.Model)
		}
	})

	t.Run("a triage-only verdict names the triage model", func(t *testing.T) {
		t.Parallel()
		result := startFull(t, &fakeModel{script: []func(llm.Request) (llm.Response, error){
			withUsage(triageResponse(false), llm.Usage{InputTokens: 4, OutputTokens: 2}),
		}})
		if result.Model != "triage-model" {
			t.Errorf("Model = %q, want triage-model", result.Model)
		}
		if diff := cmp.Diff(&review.Usage{Tokens: &review.Tokens{Input: 4, Output: 2}}, result.Usage); diff != "" {
			t.Errorf("Usage (-want +got):\n%s", diff)
		}
	})
}
