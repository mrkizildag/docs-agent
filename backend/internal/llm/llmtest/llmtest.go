// Package llmtest holds test doubles for llm.Model.
package llmtest

import (
	"context"
	"fmt"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

// ScriptedModel returns one scripted llm.Response (or error) per call, in
// order, and records every request it saw in Calls.
type ScriptedModel struct {
	Script []func(req llm.Request) (llm.Response, error)
	Calls  []llm.Request
}

var _ llm.Model = (*ScriptedModel)(nil)

// Complete records req and runs the next script step; a call past the end of
// the script is an error.
func (m *ScriptedModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	m.Calls = append(m.Calls, req)
	i := len(m.Calls) - 1
	if i >= len(m.Script) {
		return llm.Response{}, fmt.Errorf("llmtest: unexpected call %d", i+1)
	}
	return m.Script[i](req)
}
