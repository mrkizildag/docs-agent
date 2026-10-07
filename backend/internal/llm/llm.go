// Package llm is the wire-format-agnostic contract the agent loop and the
// review runners speak to a chat model, plus the adapters that translate it
// to a provider's HTTP API.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Role is who sent a Message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Model completes one turn of a conversation.
type Model interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// Request is one call to a Model.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
}

// Message is one turn of the conversation. A message produced by the model
// carries Text and, if it called tools, ToolCalls. A message reporting what
// those tools returned carries ToolResults instead, one per call.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult
}

// Tool is a function the model may call, described by a JSON Schema.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolCall is one invocation of a Tool the model asked for.
type ToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
	// Extra is provider data echoed back verbatim on the next turn.
	Extra json.RawMessage
}

// ToolResult is what running a ToolCall produced.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Response is what a Model returned for one Request.
type Response struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
}

// Usage is the token accounting for one Response.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// New returns the adapter for provider, "openai" or "anthropic".
func New(provider string, hc *http.Client, baseURL, apiKey string) (Model, error) { //nolint:ireturn // choosing the adapter is the point; callers only need Model
	switch provider {
	case "openai":
		return NewOpenAI(hc, baseURL, apiKey), nil
	case "anthropic":
		return NewAnthropic(hc, baseURL, apiKey), nil
	default:
		return nil, fmt.Errorf("unknown LLM provider %q", provider)
	}
}

const maxResponseBytes = 8 << 20

// postJSON POSTs body as JSON to url with headers and returns the response
// body of any 2xx reply. A non-2xx status or a body over maxResponseBytes is
// an error.
func postJSON(ctx context.Context, hc *http.Client, url string, headers map[string]string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, truncate(respBody))
	}
	return respBody, nil
}

func truncate(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
