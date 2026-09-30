package httpapi_test

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
)

func TestHealthz(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 \"ok\"", rec.Code, rec.Body.String())
	}
}
