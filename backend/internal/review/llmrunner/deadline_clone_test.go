package llmrunner_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func TestStart_DeadlineDuringCloneIsErrDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	model := &fakeModel{}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")
	runner.SetRemote(srv.URL + "/o/r.git")
	runner.SetTimeout(300 * time.Millisecond)

	_, err := runner.Start(t.Context(), testRequest(strings.Repeat("a", 40)))
	if !errors.Is(err, agent.ErrDeadline) {
		t.Fatalf("Start() = %v, want errors.Is agent.ErrDeadline", err)
	}
	if len(model.calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(model.calls))
	}
}
