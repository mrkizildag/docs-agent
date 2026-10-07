package auth_test

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

type accessStore struct {
	mu       sync.Mutex
	logins   map[string]auth.Login
	sessions map[string]auth.Session
}

func (m *accessStore) CreateLogin(_ context.Context, h []byte, l auth.Login) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logins[string(h)] = l
	return nil
}

func (m *accessStore) TakeLogin(_ context.Context, h []byte, _ time.Time) (auth.Login, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logins[string(h)]
	delete(m.logins, string(h))
	return l, ok, nil
}

func (m *accessStore) CreateSession(_ context.Context, s auth.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[string(s.IDHash)] = s
	return nil
}

func (m *accessStore) Session(_ context.Context, h []byte) (auth.Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[string(h)]
	return s, ok, nil
}

func (m *accessStore) TouchSession(_ context.Context, h []byte, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[string(h)]; ok {
		s.LastUsedAt = at
		m.sessions[string(h)] = s
	}
	return nil
}

func (m *accessStore) SwapTokens(_ context.Context, h []byte, prev int64, sealed []byte, accessExp, refreshExp time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[string(h)]
	if !ok || s.Version != prev {
		return false, nil
	}
	s.SealedTokens, s.AccessExpiresAt, s.RefreshExpiresAt, s.Version = sealed, accessExp, refreshExp, prev+1
	m.sessions[string(h)] = s
	return true, nil
}

func (m *accessStore) DeleteExpired(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

func (m *accessStore) DeleteSession(_ context.Context, h []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, string(h))
	return nil
}

type accessUser struct {
	mu    sync.Mutex
	repos []auth.Repo
	calls int
}

func (f *accessUser) Exchange(context.Context, string, string, string) (auth.Tokens, error) {
	return auth.Tokens{Access: "ghu_token"}, nil
}

func (f *accessUser) Refresh(context.Context, string) (auth.Tokens, error) {
	return auth.Tokens{}, auth.ErrUnauthenticated
}
func (f *accessUser) Revoke(context.Context, string) error { return nil }
func (f *accessUser) User(context.Context, string) (auth.Profile, error) {
	return auth.Profile{Login: "octocat"}, nil
}

func (f *accessUser) AccessibleRepos(_ context.Context, token string) ([]auth.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if token != "ghu_token" {
		return nil, errors.New("unexpected token")
	}
	return append([]auth.Repo(nil), f.repos...), nil
}

func (f *accessUser) set(repos ...auth.Repo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = repos
}

func (f *accessUser) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type accessEnv struct {
	svc    *auth.Service
	user   *accessUser
	viewer auth.Viewer
	now    time.Time
}

func newAccessEnv(t *testing.T, repos ...auth.Repo) *accessEnv {
	t.Helper()
	env := &accessEnv{user: &accessUser{repos: repos}, now: time.Now()}
	store := &accessStore{logins: map[string]auth.Login{}, sessions: map[string]auth.Session{}}
	opts := auth.Options{
		RedirectURL: "https://pollux.example/auth/callback", AuthorizeURL: "https://github.example/authorize",
		Now: func() time.Time { return env.now },
	}
	env.svc = auth.NewService(store, env.user, opts)

	authorizeURL, binding, err := env.svc.BeginLogin(t.Context())
	if err != nil {
		t.Fatalf("BeginLogin() = %v", err)
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	state := u.Query().Get("state")
	id, err := env.svc.CompleteLogin(t.Context(), state, binding, "code")
	if err != nil {
		t.Fatalf("CompleteLogin() = %v", err)
	}
	env.viewer, err = env.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
	return env
}

func TestRepoAccess(t *testing.T) {
	t.Parallel()
	env := newAccessEnv(t, auth.Repo{Owner: "Acme", Name: "Widgets", InstallationID: 7})

	got, err := env.svc.RepoAccess(t.Context(), env.viewer, "acme", "WIDGETS")
	if err != nil {
		t.Fatalf("RepoAccess(acme, WIDGETS) = %v, want nil error", err)
	}
	want := auth.Repo{Owner: "Acme", Name: "Widgets", InstallationID: 7}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RepoAccess() (-want +got):\n%s", diff)
	}

	_, err = env.svc.RepoAccess(t.Context(), env.viewer, "acme", "secret")
	if !errors.Is(err, auth.ErrNoAccess) {
		t.Errorf("RepoAccess(acme, secret) = %v, want ErrNoAccess", err)
	}
}

func TestRepoAccessCachesForTTL(t *testing.T) {
	t.Parallel()
	repo := auth.Repo{Owner: "acme", Name: "widgets", InstallationID: 7}
	env := newAccessEnv(t, repo)

	if _, err := env.svc.RepoAccess(t.Context(), env.viewer, "acme", "widgets"); err != nil {
		t.Fatalf("RepoAccess() = %v", err)
	}
	env.user.set()
	env.now = env.now.Add(auth.AccessTTL - time.Second)
	if _, err := env.svc.RepoAccess(t.Context(), env.viewer, "acme", "widgets"); err != nil {
		t.Errorf("RepoAccess() inside the TTL = %v, want the cached access", err)
	}
	if n := env.user.fetches(); n != 1 {
		t.Errorf("GitHub fetches inside the TTL = %d, want 1", n)
	}

	env.now = env.now.Add(2 * time.Second)
	_, err := env.svc.RepoAccess(t.Context(), env.viewer, "acme", "widgets")
	if !errors.Is(err, auth.ErrNoAccess) {
		t.Errorf("RepoAccess() after the TTL = %v, want ErrNoAccess", err)
	}
}
