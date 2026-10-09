package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

const (
	// The __Host- prefix makes browsers refuse the cookie unless it is Secure,
	// Path=/, and has no Domain.
	sessionCookie     = "__Host-pollux_session"
	bindingCookie     = "pollux_login"
	bindingCookiePath = "/auth"
)

// mountAuth registers the sign-in routes and /api/me. publicOrigin is the
// origin browsers reach pollux at; logout refuses a request from another one.
func mountAuth(mux *http.ServeMux, logger *slog.Logger, svc *auth.Service, limit *ipRateLimiter, publicOrigin string) {
	mux.HandleFunc("GET /auth/login", withSecurityHeaders(withRateLimit(logger, "auth", limit, loginHandler(logger, svc))))
	mux.HandleFunc("GET /auth/callback", withSecurityHeaders(withRateLimit(logger, "auth", limit, callbackHandler(logger, svc))))
	mux.HandleFunc("POST /auth/logout", withSecurityHeaders(withRateLimit(logger, "auth", limit, logoutHandler(logger, svc, publicOrigin))))
	mux.HandleFunc("GET /api/me", requireSession(logger, svc, meHandler))
}

// withSecurityHeaders marks a response as an API response no browser should
// frame, sniff, cache a referrer for, or load subresources from.
func withSecurityHeaders(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Strict-Transport-Security", "max-age=31536000")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		next(w, r)
	}
}

func loginHandler(logger *slog.Logger, svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorizeURL, binding, err := svc.BeginLogin(r.Context())
		if err != nil {
			logger.Error("begin login", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		http.SetCookie(w, secureCookie(bindingCookie, binding, bindingCookiePath, int(auth.LoginTTL.Seconds())))
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, authorizeURL, http.StatusFound)
	}
}

func callbackHandler(logger *slog.Logger, svc *auth.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, secureCookie(bindingCookie, "", bindingCookiePath, -1))
		w.Header().Set("Cache-Control", "no-store")

		q := r.URL.Query()
		binding, err := r.Cookie(bindingCookie)
		if q.Get("code") == "" || q.Get("state") == "" || err != nil {
			http.Error(w, "invalid login", http.StatusBadRequest)
			return
		}
		id, err := svc.CompleteLogin(r.Context(), q.Get("state"), binding.Value, q.Get("code"))
		if errors.Is(err, auth.ErrInvalidLogin) {
			logger.Warn("login refused", "err", err)
			http.Error(w, "invalid login", http.StatusBadRequest)
			return
		}
		if err != nil {
			logger.Error("complete login", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// A browser that signs in again must not leave its old session alive.
		if old, err := r.Cookie(sessionCookie); err == nil {
			// The new session already exists, so a failure here is logged and the login continues.
			if err := svc.Logout(r.Context(), old.Value); errors.Is(err, auth.ErrRevokeFailed) {
				logger.Warn("re-login: revoke GitHub grant of the old session", "err", err)
			} else if err != nil {
				logger.Error("re-login: end the old session", "err", err)
			}
		}
		http.SetCookie(w, secureCookie(sessionCookie, id, "/", int(auth.SessionIdle.Seconds())))
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func logoutHandler(logger *slog.Logger, svc *auth.Service, publicOrigin string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !strings.EqualFold(origin, publicOrigin) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.SetCookie(w, secureCookie(sessionCookie, "", "/", -1))
		if c, err := r.Cookie(sessionCookie); err == nil {
			err := svc.Logout(r.Context(), c.Value)
			if errors.Is(err, auth.ErrRevokeFailed) {
				logger.Warn("logout: revoke GitHub grant", "err", err)
			} else if err != nil {
				logger.Error("logout", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// requireSession authenticates the request's session cookie and passes the
// viewer to next, or answers 401.
func requireSession(logger *slog.Logger, svc *auth.Service, next func(http.ResponseWriter, *http.Request, auth.Viewer)) http.HandlerFunc {
	return withSecurityHeaders(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			unauthorized(w)
			return
		}
		viewer, err := svc.Authenticate(r.Context(), c.Value)
		if errors.Is(err, auth.ErrUnauthenticated) {
			unauthorized(w)
			return
		}
		if err != nil {
			logger.Error("authenticate session", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// The session slides server-side on use; renew the cookie so the browser keeps it as long.
		http.SetCookie(w, secureCookie(sessionCookie, c.Value, "/", int(auth.SessionIdle.Seconds())))
		next(w, r, viewer)
	})
}

// unauthorized answers 401 and clears the session cookie. It is sent after any
// renewal already set on w, and the browser keeps the last Set-Cookie.
func unauthorized(w http.ResponseWriter) {
	http.SetCookie(w, secureCookie(sessionCookie, "", "/", -1))
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func meHandler(w http.ResponseWriter, _ *http.Request, viewer auth.Viewer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	// The encoder only fails on a broken connection, which no one can be told about.
	_ = json.NewEncoder(w).Encode(struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}{viewer.Login, viewer.AvatarURL})
}

// secureCookie builds a cookie that script cannot read and that is never sent
// cross-site or over http. config requires an https PUBLIC_URL, so Secure is
// unconditional.
func secureCookie(name, value, path string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}
