// Package llm is the wire-format-agnostic contract the agent loop and the
// review runners speak to a chat model, plus the adapters that translate it
// to a provider's HTTP API.
package llm

import (
	"context"
	"encoding/json"
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
