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

func TestOpenAIComplete_GeminiQuirks(t *testing.T) {
	t.Parallel()

	const extra = `{"google":{"thought_signature":"sig=="}}`
	responses := []string{
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","extra_content":` + extra + `,"function":{"name":"read_file","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":40}}`,
		`{"choices":[{"finish_reason":"function_call_filter: MALFORMED_FUNCTION_CALL","message":{"role":"assistant"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}
	var bodies []json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		bodies = append(bodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, responses[len(bodies)-1])
	}))
	t.Cleanup(srv.Close)

	model := llm.NewOpenAI(&http.Client{Timeout: 5 * time.Second}, srv.URL, "k")
	first, err := model.Complete(t.Context(), llm.Request{Model: "m"})
	if err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	if first.Usage.OutputTokens != 30 {
		t.Errorf("OutputTokens = %d, want 30 (total - prompt)", first.Usage.OutputTokens)
	}
	if string(first.ToolCalls[0].Extra) != extra {
		t.Errorf("Extra = %s, want %s", first.ToolCalls[0].Extra, extra)
	}
	if first.ToolCalls[1].Extra != nil {
		t.Errorf("absent extra_content decoded as %s, want nil", first.ToolCalls[1].Extra)
	}

	second, err := model.Complete(t.Context(), llm.Request{
		Model:    "m",
		Messages: []llm.Message{{Role: llm.RoleAssistant, ToolCalls: first.ToolCalls}},
	})
	if err != nil {
		t.Fatalf("second Complete: %v", err)
	}
	var sent struct {
		Messages []struct {
			ToolCalls []map[string]json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[1], &sent); err != nil {
		t.Fatalf("unmarshal sent body: %v", err)
	}
	calls := sent.Messages[0].ToolCalls
	if got := string(calls[0]["extra_content"]); got != extra {
		t.Errorf("echoed extra_content = %s, want %s", got, extra)
	}
	if _, ok := calls[1]["extra_content"]; ok {
		t.Error("extra_content present on call without it")
	}

	if second.Text != "" || len(second.ToolCalls) != 0 {
		t.Errorf("malformed-call response = %+v, want empty", second)
	}
}

func TestOpenAIComplete_EmptyToolResultKeepsContent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw struct {
			Messages []map[string]json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&raw)
		if c, ok := raw.Messages[0]["content"]; ok {
			got = string(c)
		} else {
			got = "<missing>"
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	// Trailing slash must be tolerated.
	m := llm.NewOpenAI(srv.Client(), srv.URL+"/", "k")
	_, err := m.Complete(t.Context(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, ToolResults: []llm.ToolResult{{CallID: "c1", Content: ""}}},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got != `""` {
		t.Errorf("tool message content = %s, want \"\"", got)
	}
}
