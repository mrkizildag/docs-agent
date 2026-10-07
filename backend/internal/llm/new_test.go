package llm_test

import (
	"net/http"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

func TestNew(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		wantErr  bool
	}{
		{provider: "openai"},
		{provider: "anthropic"},
		{provider: "gemini", wantErr: true},
		{provider: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			t.Parallel()
			m, err := llm.New(tc.provider, &http.Client{}, "http://example.com", "k")
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%q) error = %v, wantErr %v", tc.provider, err, tc.wantErr)
			}
			if !tc.wantErr && m == nil {
				t.Errorf("New(%q) = nil, want a model", tc.provider)
			}
		})
	}
}
