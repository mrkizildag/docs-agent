package llmrunner_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func TestAllowedProtocols_TokenOnlyOverHTTPS(t *testing.T) {
	t.Parallel()

	if got := llmrunner.AllowedProtocols("ghs_token"); got != "https" {
		t.Errorf("AllowedProtocols(token) = %q, want %q", got, "https")
	}
	if got := llmrunner.AllowedProtocols(""); got != "https:file:http" {
		t.Errorf("AllowedProtocols(no token) = %q, want %q", got, "https:file:http")
	}
}
