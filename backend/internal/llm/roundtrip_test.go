package llm_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/llm"
)

func scriptedRequest() llm.Request {
	return llm.Request{
		Model:     "test-model",
		System:    "be terse",
		MaxTokens: 1000,
		Tools: []llm.Tool{
			{Name: "read_file", Description: "reads a file", Schema: json.RawMessage(`{"type":"object"}`)},
		},
		Messages: []llm.Message{
			{Role: llm.RoleUser, Text: "review this"},
			{Role: llm.RoleAssistant, Text: "looking", ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "read_file", Args: json.RawMessage(`{"path":"a.md"}`)},
				{ID: "c2", Name: "read_file", Args: json.RawMessage(`{"path":"b.md"}`)},
			}},
			{Role: llm.RoleUser, ToolResults: []llm.ToolResult{
				{CallID: "c1", Content: "contents of a"},
				{CallID: "c2", Content: "no such file", IsError: true},
			}},
		},
	}
}

func scriptedWant() llm.Response {
	return llm.Response{
		ToolCalls: []llm.ToolCall{{ID: "c3", Name: "read_file", Args: json.RawMessage(`{"path":"c.md"}`)}},
		Usage:     llm.Usage{InputTokens: 11, OutputTokens: 7},
	}
}

func TestProviders_ScriptedConversationRoundTrips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		wantBody string
		reply    string
		model    func(srvURL string) llm.Model
	}{
		{
			name: "openai",
			path: "/chat/completions",
			wantBody: `{"model":"test-model","max_tokens":1000,"messages":[
				{"role":"system","content":"be terse"},
				{"role":"user","content":"review this"},
				{"role":"assistant","content":"looking","tool_calls":[
					{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.md\"}"}},
					{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b.md\"}"}}]},
				{"role":"tool","content":"contents of a","tool_call_id":"c1"},
				{"role":"tool","content":"no such file","tool_call_id":"c2"}],
				"tools":[{"type":"function","function":{"name":"read_file","description":"reads a file","parameters":{"type":"object"}}}]}`,
			reply: `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
				{"id":"c3","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"c.md\"}"}}]}}],
				"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			model: func(u string) llm.Model {
				return llm.NewOpenAI(&http.Client{Timeout: 5 * time.Second}, u, "k")
			},
		},
		{
			name: "anthropic",
			path: "/v1/messages",
			wantBody: `{"model":"test-model","system":"be terse","max_tokens":1000,"messages":[
				{"role":"user","content":[{"type":"text","text":"review this"}]},
				{"role":"assistant","content":[
					{"type":"text","text":"looking"},
					{"type":"tool_use","id":"c1","name":"read_file","input":{"path":"a.md"}},
					{"type":"tool_use","id":"c2","name":"read_file","input":{"path":"b.md"}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"c1","content":"contents of a"},
					{"type":"tool_result","tool_use_id":"c2","content":"no such file","is_error":true}]}],
				"tools":[{"name":"read_file","description":"reads a file","input_schema":{"type":"object"}}]}`,
			reply: `{"content":[{"type":"tool_use","id":"c3","name":"read_file","input":{"path":"c.md"}}],
				"usage":{"input_tokens":11,"output_tokens":7}}`,
			model: func(u string) llm.Model {
				return llm.NewAnthropic(&http.Client{Timeout: 5 * time.Second}, u, "k")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.path)
				}
				if tc.name == "anthropic" {
					if got := r.Header.Get("x-api-key"); got != "k" {
						t.Errorf("x-api-key = %q, want k", got)
					}
					if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
						t.Errorf("anthropic-version = %q, want 2023-06-01", got)
					}
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				var got, want any
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if err := json.Unmarshal([]byte(tc.wantBody), &want); err != nil {
					t.Errorf("decode wantBody: %v", err)
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("wire request mismatch (-want +got):\n%s", diff)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.reply))
			}))
			t.Cleanup(srv.Close)

			resp, err := tc.model(srv.URL).Complete(t.Context(), scriptedRequest())
			if err != nil {
				t.Fatalf("Complete() = %v, want nil", err)
			}
			if diff := cmp.Diff(scriptedWant(), resp); diff != "" {
				t.Errorf("Response mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
