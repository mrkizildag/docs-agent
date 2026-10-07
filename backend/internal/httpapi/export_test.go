package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

// RequireRepo exposes requireRepo to the external tests until a real route uses it.
func RequireRepo(logger *slog.Logger, svc *auth.Service, next func(http.ResponseWriter, *http.Request, auth.Viewer, auth.Repo)) http.HandlerFunc {
	return requireRepo(logger, svc, next)
}

// ClientIP exposes clientIP to the external tests.
func ClientIP(r *http.Request) string { return clientIP(r) }
