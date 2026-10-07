package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

// requireRepo authenticates the session, then resolves the {owner} and {repo}
// path values to a repository the viewer can read. A repository the viewer
// cannot see answers 404, whether or not it exists.
func requireRepo(logger *slog.Logger, svc *auth.Service, next func(http.ResponseWriter, *http.Request, auth.Viewer, auth.Repo)) http.HandlerFunc {
	return requireSession(logger, svc, func(w http.ResponseWriter, r *http.Request, viewer auth.Viewer) {
		repo, err := svc.RepoAccess(r.Context(), viewer, r.PathValue("owner"), r.PathValue("repo"))
		if errors.Is(err, auth.ErrNoAccess) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, auth.ErrUnauthenticated) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			logger.Error("check repository access", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next(w, r, viewer, repo)
	})
}
