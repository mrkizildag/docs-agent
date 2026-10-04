package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	anthropicDefaultBaseURL   = "https://api.anthropic.com"
	anthropicVersion          = "2023-06-01"
	anthropicDefaultMaxTokens = 8192
)

// Anthropic is a Model backed by the Anthropic Messages API.
type Anthropic struct {
	hc      *http.Client
	baseURL string
	apiKey  string
}

var _ Model = (*Anthropic)(nil)

// NewAnthropic returns an Anthropic model that calls baseURL with apiKey,
// using hc for the HTTP round trip. An empty baseURL means api.anthropic.com.
func NewAnthropic(hc *http.Client, baseURL, apiKey string) *Anthropic {
	if baseURL == "" {
		baseURL = anthropicDefaultBaseURL
	}
	return &Anthropic{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey}
}

// Complete implements Model over the Messages endpoint. It rejects a response
// that isn't JSON or that has no content field, mirroring OpenAI.Complete.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(anthropicRequestFrom(req))
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.hc.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: create message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("anthropic: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Response{}, fmt.Errorf("anthropic: create message: status %d: %s", resp.StatusCode, truncate(respBody))
	}

	var wireResp anthropicResponse
	if err := json.Unmarshal(respBody, &wireResp); err != nil {
		return Response{}, fmt.Errorf("anthropic: response is not valid JSON: %s", truncate(respBody))
	}
	if wireResp.Content == nil {
		return Response{}, fmt.Errorf("anthropic: response has no content: %s", truncate(respBody))
	}

	var text strings.Builder
	toolCalls := make([]ToolCall, 0)
	for _, block := range wireResp.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, ToolCall{ID: block.ID, Name: block.Name, Args: block.Input})
		}
	}

	return Response{
		Text:      text.String(),
		ToolCalls: toolCalls,
		Usage: Usage{
			InputTokens:  wireResp.Usage.InputTokens,
			OutputTokens: wireResp.Usage.OutputTokens,
		},
	}, nil
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	MaxTokens int                `json:"max_tokens"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicResponse struct {
	Content []anthropicBlock `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// anthropicRequestFrom translates a Request into the Messages wire format:
// an assistant Message's ToolCalls become tool_use blocks; a Message's
// ToolResults become tool_result blocks in one user message.
func anthropicRequestFrom(req Request) anthropicRequest {
	messages := make([]anthropicMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		blocks := make([]anthropicBlock, 0, 1+len(m.ToolCalls)+len(m.ToolResults))
		if m.Text != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Text})
		}
		for _, tc := range m.ToolCalls {
			input := tc.Args
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: input})
		}
		for _, tr := range m.ToolResults {
			blocks = append(blocks, anthropicBlock{Type: "tool_result", ToolUseID: tr.CallID, Content: tr.Content, IsError: tr.IsError})
		}
		messages = append(messages, anthropicMessage{Role: string(m.Role), Content: blocks})
	}

	tools := make([]anthropicTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = anthropicDefaultMaxTokens
	}

	return anthropicRequest{Model: req.Model, System: req.System, Messages: messages, Tools: tools, MaxTokens: maxTokens}
}
