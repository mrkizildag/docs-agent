package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm/llmtest"
)

func testRoot(t *testing.T) *os.Root {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.md"), []byte("doc content"), 0o600); err != nil {
		t.Fatalf("write doc.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write .git/config: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func finishTool() llm.Tool {
	return llm.Tool{Name: "submit", Schema: json.RawMessage(`{"type":"object"}`)}
}

func TestRun_FinishesOnFirstToolCall(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "1", Name: "submit", Args: json.RawMessage(`{"ok":true}`)}},
				Usage:     llm.Usage{InputTokens: 10, OutputTokens: 5},
			}, nil
		},
	}}

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 5,
	}

	raw, stats, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
	if err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("Run() raw = %s, want {\"ok\":true}", raw)
	}
	if stats.Steps != 1 || stats.InputTokens != 10 || stats.OutputTokens != 5 {
		t.Errorf("Run() stats = %+v, want {1 10 5}", stats)
	}
}

func TestRun_ReadFileThenFinish(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", Args: json.RawMessage(`{"path":"doc.md"}`)}},
			}, nil
		},
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || last.ToolResults[0].Content != "doc content" {
				t.Errorf("tool result message = %+v, want content %q", last, "doc content")
			}
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "2", Name: "submit", Args: json.RawMessage(`{"ok":true}`)}},
			}, nil
		},
	}}

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 5,
	}

	raw, stats, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
	if err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("Run() raw = %s, want {\"ok\":true}", raw)
	}
	if stats.Steps != 2 {
		t.Errorf("Run() stats.Steps = %d, want 2", stats.Steps)
	}
}

func TestRun_ReadFileRefusesGitPath(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", Args: json.RawMessage(`{"path":".git/config"}`)}},
			}, nil
		},
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError {
				t.Fatalf("tool result = %+v, want an IsError result", last)
			}
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "2", Name: "submit", Args: json.RawMessage(`{}`)}},
			}, nil
		},
	}}

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 5,
	}

	if _, _, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000)); err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
}

func TestRun_UnknownToolDoesNotPanic(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "1", Name: "run_shell", Args: json.RawMessage(`{}`)}},
			}, nil
		},
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError {
				t.Fatalf("tool result = %+v, want an IsError result", last)
			}
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "2", Name: "submit", Args: json.RawMessage(`{}`)}},
			}, nil
		},
	}}

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 5,
	}

	if _, _, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000)); err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
}

func TestRun_AcceptRejectionIsRetried(t *testing.T) {
	t.Parallel()

	attempts := 0
	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "1", Name: "submit", Args: json.RawMessage(`{"bad":true}`)}},
			}, nil
		},
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError {
				t.Fatalf("tool result = %+v, want an IsError result", last)
			}
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "2", Name: "submit", Args: json.RawMessage(`{"ok":true}`)}},
			}, nil
		},
	}}

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept: func(args json.RawMessage) error {
			attempts++
			if attempts == 1 {
				return errors.New("bad proposal")
			}
			return nil
		},
		MaxSteps: 5,
	}

	raw, _, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
	if err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("Run() raw = %s, want {\"ok\":true}", raw)
	}
	if attempts != 2 {
		t.Errorf("Accept called %d times, want 2", attempts)
	}
}

func TestRun_StepLimit(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) { return llm.Response{Text: "thinking"}, nil },
		func(llm.Request) (llm.Response, error) { return llm.Response{Text: "thinking"}, nil },
	}}

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 2,
	}

	_, stats, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
	if !errors.Is(err, agent.ErrStepLimit) {
		t.Fatalf("Run() err = %v, want ErrStepLimit", err)
	}
	if stats.Steps != 2 {
		t.Errorf("Run() stats.Steps = %d, want 2", stats.Steps)
	}
}

func TestRun_TokenBudget(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{Text: "thinking", Usage: llm.Usage{InputTokens: 600, OutputTokens: 600}}, nil
		},
	}}

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 5,
	}

	_, _, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
	if !errors.Is(err, agent.ErrTokenBudget) {
		t.Fatalf("Run() err = %v, want ErrTokenBudget", err)
	}
}

func TestRun_OffersExactlyTheFourTools(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(req llm.Request) (llm.Response, error) {
			var names []string
			for _, tool := range req.Tools {
				names = append(names, tool.Name)
			}
			want := []string{"read_file", "grep", "list_dir", "submit"}
			if diff := cmp.Diff(want, names); diff != "" {
				t.Errorf("offered tools (-want +got):\n%s", diff)
			}
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: "submit", Args: json.RawMessage(`{}`)}}}, nil
		},
	}}
	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 1,
	}
	if _, _, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000)); err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
}

func TestRun_TextOnlyReplyGetsNudge(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) { return llm.Response{Text: "hmm"}, nil },
		func(req llm.Request) (llm.Response, error) {
			n := len(req.Messages)
			if n != 3 || req.Messages[1].Role != llm.RoleAssistant || req.Messages[1].Text != "hmm" {
				t.Fatalf("messages = %+v, want prompt, assistant text, nudge", req.Messages)
			}
			nudge := req.Messages[2]
			if nudge.Role != llm.RoleUser || !strings.Contains(nudge.Text, "submit") {
				t.Errorf("nudge = %+v, want a user message naming submit", nudge)
			}
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: "submit", Args: json.RawMessage(`{}`)}}}, nil
		},
	}}
	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 3,
	}
	_, stats, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
	if err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	if stats.Steps != 2 {
		t.Errorf("stats.Steps = %d, want 2", stats.Steps)
	}
}

func TestRun_DeadlineIsErrDeadline(t *testing.T) {
	t.Parallel()

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 3,
	}

	t.Run("from Complete", func(t *testing.T) {
		t.Parallel()
		model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
			func(llm.Request) (llm.Response, error) { return llm.Response{}, context.DeadlineExceeded },
		}}
		_, _, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
		if !errors.Is(err, agent.ErrDeadline) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run() err = %v, want ErrDeadline wrapping DeadlineExceeded", err)
		}
	})

	t.Run("expired before step", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		_, _, err := agent.Run(ctx, &llmtest.ScriptedModel{}, task, agent.NewBudget(1000))
		if !errors.Is(err, agent.ErrDeadline) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run() err = %v, want ErrDeadline wrapping DeadlineExceeded", err)
		}
	})
}

func TestRun_CancelIsNotErrDeadline(t *testing.T) {
	t.Parallel()

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 3,
	}

	t.Run("from Complete", func(t *testing.T) {
		t.Parallel()
		model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
			func(llm.Request) (llm.Response, error) { return llm.Response{}, context.Canceled },
		}}
		_, _, err := agent.Run(t.Context(), model, task, agent.NewBudget(1000))
		if errors.Is(err, agent.ErrDeadline) || !errors.Is(err, context.Canceled) {
			t.Errorf("Run() err = %v, want Canceled and not ErrDeadline", err)
		}
	})

	t.Run("canceled before step", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := agent.Run(ctx, &llmtest.ScriptedModel{}, task, agent.NewBudget(1000))
		if errors.Is(err, agent.ErrDeadline) || !errors.Is(err, context.Canceled) {
			t.Errorf("Run() err = %v, want Canceled and not ErrDeadline", err)
		}
	})
}

func emptyReply(llm.Request) (llm.Response, error) { return llm.Response{}, nil }

func finishReply(llm.Request) (llm.Response, error) {
	return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: "submit", Args: json.RawMessage(`{}`)}}}, nil
}

func emptyReplyTask(t *testing.T) agent.Task {
	t.Helper()
	return agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 10,
	}
}

func TestRun_ThreeEmptyRepliesIsErrMalformed(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){emptyReply, emptyReply, emptyReply}}
	_, _, err := agent.Run(t.Context(), model, emptyReplyTask(t), agent.NewBudget(1000))
	if !errors.Is(err, agent.ErrMalformed) {
		t.Fatalf("Run() = %v, want ErrMalformed", err)
	}
	if !strings.Contains(err.Error(), "3 consecutive") {
		t.Errorf("Run() = %q, want the count named", err)
	}
}

func TestRun_TwoEmptyRepliesThenFinishSucceeds(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){emptyReply, emptyReply, finishReply}}
	_, stats, err := agent.Run(t.Context(), model, emptyReplyTask(t), agent.NewBudget(1000))
	if err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	if stats.Steps != 3 {
		t.Errorf("stats.Steps = %d, want 3", stats.Steps)
	}
}

func TestRun_EmptyReplyAppendsNoEmptyAssistantMessage(t *testing.T) {
	t.Parallel()

	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){emptyReply, finishReply}}
	if _, _, err := agent.Run(t.Context(), model, emptyReplyTask(t), agent.NewBudget(1000)); err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	for _, m := range model.Calls[1].Messages {
		if m.Role == llm.RoleAssistant {
			t.Errorf("messages contain assistant message %+v, want none after an empty reply", m)
		}
	}
	if n := len(model.Calls[1].Messages); n != 2 {
		t.Errorf("len(messages) = %d, want prompt and nudge", n)
	}
}

func TestRun_ModelErrorWrapsErrModel(t *testing.T) {
	t.Parallel()

	task := agent.Task{
		Model: "m", Prompt: "go", Root: testRoot(t), Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 5,
	}
	boom := errors.New("boom")

	tests := []struct {
		name      string
		script    []func(llm.Request) (llm.Response, error)
		wantCause error
	}{
		{
			name:      "complete fails",
			script:    []func(llm.Request) (llm.Response, error){func(llm.Request) (llm.Response, error) { return llm.Response{}, boom }},
			wantCause: boom,
		},
		{
			name:      "malformed replies",
			script:    []func(llm.Request) (llm.Response, error){emptyReply, emptyReply, emptyReply},
			wantCause: agent.ErrMalformed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := agent.Run(t.Context(), &llmtest.ScriptedModel{Script: tc.script}, task, agent.NewBudget(1000))
			if !errors.Is(err, agent.ErrModel) || !errors.Is(err, tc.wantCause) {
				t.Errorf("Run() err = %v, want ErrModel wrapping %v", err, tc.wantCause)
			}
			if errors.Is(err, agent.ErrDeadline) {
				t.Errorf("Run() err = %v, want not ErrDeadline", err)
			}
		})
	}
}
