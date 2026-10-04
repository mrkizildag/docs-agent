package llm_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

func TestAnthropicComplete_Rejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "non-2xx", status: http.StatusTooManyRequests, body: `{"error":"slow down"}`, wantErr: "status 429: {\"error\":\"slow down\"}"},
		{name: "not json", status: http.StatusOK, body: "OK", wantErr: "not valid JSON"},
		{name: "no content", status: http.StatusOK, body: `{"usage":{}}`, wantErr: "no content"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			model := llm.NewAnthropic(&http.Client{Timeout: 5 * time.Second}, srv.URL, "k")
			_, err := model.Complete(t.Context(), llm.Request{Model: "m", Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Complete() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAnthropicComplete_DefaultMaxTokens(t *testing.T) {
	t.Parallel()

	var got struct {
		MaxTokens int `json:"max_tokens"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(srv.Close)

	model := llm.NewAnthropic(&http.Client{Timeout: 5 * time.Second}, srv.URL, "k")
	if _, err := model.Complete(t.Context(), llm.Request{Model: "m"}); err != nil {
		t.Fatalf("Complete() = %v, want nil", err)
	}
	if got.MaxTokens != 8192 {
		t.Errorf("max_tokens = %d, want 8192", got.MaxTokens)
	}
}
