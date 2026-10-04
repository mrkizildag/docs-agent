package llm_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/llm"
)

func TestOpenAIComplete_ToolCallRoundTrip(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", auth)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"choices": [{"message": {"role": "assistant", "content": "", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"docs/x.md\"}"}}
			]}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5}
		}`)
	}))
	t.Cleanup(srv.Close)

	model := llm.NewOpenAI(&http.Client{Timeout: 5 * time.Second}, srv.URL, "test-key")

	req := llm.Request{
		Model:  "gpt-test",
		System: "be terse",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Text: "read the doc"},
		},
		Tools: []llm.Tool{
			{Name: "read_file", Description: "reads a file", Schema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	resp, err := model.Complete(t.Context(), req)
	if err != nil {
		t.Fatalf("Complete() = %v, want nil error", err)
	}

	if len(resp.ToolCalls) != 1 {
		t.Fatalf("len(ToolCalls) = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" {
		t.Errorf("ToolCalls[0] = %+v, want ID=call_1 Name=read_file", tc)
	}
	if string(tc.Args) != `{"path":"docs/x.md"}` {
		t.Errorf("ToolCalls[0].Args = %s, want {\"path\":\"docs/x.md\"}", tc.Args)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want {10 5}", resp.Usage)
	}

	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("request messages = %v, want 2 messages (system, user)", gotBody["messages"])
	}
}

func TestOpenAIComplete_RejectsNonJSONResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// GitHub Models' retired endpoint returns 200 text/plain "OK" rather
		// than an error status.
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "OK")
	}))
	t.Cleanup(srv.Close)

	model := llm.NewOpenAI(&http.Client{Timeout: 5 * time.Second}, srv.URL, "test-key")

	_, err := model.Complete(t.Context(), llm.Request{Model: "gpt-test"})
	if err == nil {
		t.Fatal("Complete() = nil error, want error for a non-JSON 200 response")
	}
}

func TestOpenAIComplete_RejectsMissingChoices(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"usage": {"prompt_tokens": 1, "completion_tokens": 1}}`)
	}))
	t.Cleanup(srv.Close)

	model := llm.NewOpenAI(&http.Client{Timeout: 5 * time.Second}, srv.URL, "test-key")

	_, err := model.Complete(t.Context(), llm.Request{Model: "gpt-test"})
	if err == nil {
		t.Fatal("Complete() = nil error, want error for a response without choices")
	}
}
