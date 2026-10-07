package httpapi_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
)

type repoStore struct {
	logins   map[string]auth.Login
	sessions map[string]auth.Session
}

func (m *repoStore) CreateLogin(_ context.Context, h []byte, l auth.Login) error {
	m.logins[string(h)] = l
	return nil
}

func (m *repoStore) TakeLogin(_ context.Context, h []byte, _ time.Time) (auth.Login, bool, error) {
	l, ok := m.logins[string(h)]
	delete(m.logins, string(h))
	return l, ok, nil
}

func (m *repoStore) CreateSession(_ context.Context, s auth.Session) error {
	m.sessions[string(s.IDHash)] = s
	return nil
}

func (m *repoStore) Session(_ context.Context, h []byte) (auth.Session, bool, error) {
	s, ok := m.sessions[string(h)]
	return s, ok, nil
}

func (m *repoStore) TouchSession(_ context.Context, h []byte, at time.Time) error {
	if s, ok := m.sessions[string(h)]; ok {
		s.LastUsedAt = at
		m.sessions[string(h)] = s
	}
	return nil
}

func (m *repoStore) SwapTokens(_ context.Context, h []byte, prev int64, sealed []byte, accessExp, refreshExp time.Time) (bool, error) {
	s, ok := m.sessions[string(h)]
	if !ok || s.Version != prev {
		return false, nil
	}
	s.SealedTokens, s.AccessExpiresAt, s.RefreshExpiresAt, s.Version = sealed, accessExp, refreshExp, prev+1
	m.sessions[string(h)] = s
	return true, nil
}

func (m *repoStore) DeleteExpired(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

func (m *repoStore) DeleteSession(_ context.Context, h []byte) error {
	delete(m.sessions, string(h))
	return nil
}

type repoGitHub struct{ repos []auth.Repo }

func (g *repoGitHub) Exchange(context.Context, string, string, string) (auth.Tokens, error) {
	return auth.Tokens{Access: "ghu_token"}, nil
}

func (g *repoGitHub) Refresh(context.Context, string) (auth.Tokens, error) {
	return auth.Tokens{}, auth.ErrUnauthenticated
}
func (g *repoGitHub) Revoke(context.Context, string) error { return nil }
func (g *repoGitHub) User(context.Context, string) (auth.Profile, error) {
	return auth.Profile{Login: "octocat"}, nil
}

func (g *repoGitHub) AccessibleRepos(context.Context, string) ([]auth.Repo, error) {
	return g.repos, nil
}

func TestRequireRepo(t *testing.T) {
	t.Parallel()
	now := time.Now()
	gh := &repoGitHub{repos: []auth.Repo{{Owner: "acme", Name: "widgets", InstallationID: 7}}}
	svc := auth.NewService(
		&repoStore{logins: map[string]auth.Login{}, sessions: map[string]auth.Session{}}, gh,
		auth.Options{AuthorizeURL: "https://github.example/authorize", Now: func() time.Time { return now }},
	)

	authorizeURL, binding, err := svc.BeginLogin(t.Context())
	if err != nil {
		t.Fatalf("BeginLogin() = %v", err)
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	id, err := svc.CompleteLogin(t.Context(), u.Query().Get("state"), binding, "code")
	if err != nil {
		t.Fatalf("CompleteLogin() = %v", err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: id, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/repos/{owner}/{repo}", httpapi.RequireRepo(slog.New(slog.DiscardHandler), svc,
		func(w http.ResponseWriter, _ *http.Request, _ auth.Viewer, repo auth.Repo) {
			if _, err := fmt.Fprintf(w, "%s/%s#%d", repo.Owner, repo.Name, repo.InstallationID); err != nil {
				t.Errorf("write response: %v", err)
			}
		}))

	get := func(path string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := get("/api/repos/Acme/Widgets", cookie); rec.Code != http.StatusOK || rec.Body.String() != "acme/widgets#7" {
		t.Errorf("accessible repo = %d %q, want 200 acme/widgets#7", rec.Code, rec.Body.String())
	}
	if rec := get("/api/repos/acme/widgets", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", rec.Code)
	}
	if rec := get("/api/repos/acme/widgets", &http.Cookie{Name: sessionCookie, Value: "forged", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown session = %d, want 401", rec.Code)
	}

	missing := get("/api/repos/acme/unknown", cookie)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown repo = %d, want 404", missing.Code)
	}
	foreign := get("/api/repos/other/private", cookie)
	if foreign.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
		t.Errorf("uninstalled repo = %d %q, want the same 404 as an unknown repo (%q)", foreign.Code, foreign.Body.String(), missing.Body.String())
	}

	gh.repos = nil
	if rec := get("/api/repos/acme/widgets", cookie); rec.Code != http.StatusOK {
		t.Errorf("repo inside the TTL after losing access = %d, want 200 from the cache", rec.Code)
	}
	now = now.Add(auth.AccessTTL + time.Second)
	if rec := get("/api/repos/acme/widgets", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("repo after the TTL after losing access = %d, want 404", rec.Code)
	}
}
