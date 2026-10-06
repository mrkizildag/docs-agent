package agent

import (
	"context"
	"os"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

// CallTool exposes callTool to the external test package.
func CallTool(ctx context.Context, root *os.Root, call llm.ToolCall) llm.ToolResult {
	return callTool(ctx, root, call)
}
