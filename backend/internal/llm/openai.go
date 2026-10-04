package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OpenAI is a Model backed by an OpenAI-compatible chat completions endpoint:
// OpenAI itself, Gemini, OpenRouter, or Ollama.
type OpenAI struct {
	hc      *http.Client
	baseURL string
	apiKey  string
}

var _ Model = (*OpenAI)(nil)

// NewOpenAI returns an OpenAI model that calls baseURL (e.g.
// "https://api.openai.com/v1") with apiKey, using hc for the HTTP round trip.
func NewOpenAI(hc *http.Client, baseURL, apiKey string) *OpenAI {
	return &OpenAI{hc: hc, baseURL: baseURL, apiKey: apiKey}
}

// Complete implements Model over the chat completions endpoint. It rejects a
// response that isn't JSON or that carries no choices: GitHub Models' retired
// endpoint returns 200 text/plain "OK" rather than an error status.
func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(openAIRequestFrom(req))
	if err != nil {
		return Response{}, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.hc.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("openai: complete chat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("openai: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("openai: complete chat: status %d: %s", resp.StatusCode, truncate(respBody))
	}

	var wireResp openAIResponse
	if err := json.Unmarshal(respBody, &wireResp); err != nil {
		return Response{}, fmt.Errorf("openai: response is not valid JSON: %s", truncate(respBody))
	}
	if len(wireResp.Choices) == 0 {
		return Response{}, fmt.Errorf("openai: response has no choices: %s", truncate(respBody))
	}

	msg := wireResp.Choices[0].Message
	toolCalls := make([]ToolCall, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		toolCalls = append(toolCalls, ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: json.RawMessage(tc.Function.Arguments)})
	}

	return Response{
		Text:      msg.Content,
		ToolCalls: toolCalls,
		Usage: Usage{
			InputTokens:  wireResp.Usage.PromptTokens,
			OutputTokens: wireResp.Usage.CompletionTokens,
		},
	}, nil
}

func truncate(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

type openAIRequest struct {
	Model     string          `json:"model"`
	Messages  []openAIMessage `json:"messages"`
	Tools     []openAITool    `json:"tools,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string            `json:"type"`
	Function openAIFunctionDef `json:"function"`
}

type openAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIChoice struct {
	Message openAIResponseMessage `json:"message"`
}

type openAIResponseMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// openAIRequestFrom translates a Request into the chat completions wire
// format: an assistant Message's ToolCalls become its "tool_calls"; a
// Message's ToolResults become one role:"tool" message per result.
func openAIRequestFrom(req Request) openAIRequest {
	messages := make([]openAIMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, openAIMessage{Role: "system", Content: req.System})
	}

	for _, m := range req.Messages {
		if len(m.ToolResults) > 0 {
			for _, tr := range m.ToolResults {
				messages = append(messages, openAIMessage{Role: "tool", Content: tr.Content, ToolCallID: tr.CallID})
			}
			continue
		}

		wireMsg := openAIMessage{Role: string(m.Role), Content: m.Text}
		if len(m.ToolCalls) > 0 {
			calls := make([]openAIToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				calls = append(calls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openAIFunctionCall{
						Name:      tc.Name,
						Arguments: string(tc.Args),
					},
				})
			}
			wireMsg.ToolCalls = calls
		}
		messages = append(messages, wireMsg)
	}

	tools := make([]openAITool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, openAITool{
			Type: "function",
			Function: openAIFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}

	return openAIRequest{Model: req.Model, Messages: messages, Tools: tools, MaxTokens: req.MaxTokens}
}
