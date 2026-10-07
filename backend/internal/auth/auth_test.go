package auth_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
)

func t0() time.Time { return time.Unix(1_800_000_000, 0) }

type fakeGitHub struct {
	refreshCalls atomic.Int32
	refreshErr   error
	revoked      atomic.Value
	exchanged    auth.Tokens
	// onRefresh runs inside Refresh, before it answers.
	onRefresh func()

	mu        sync.Mutex
	repos     []auth.Repo
	reposErr  error
	repoCalls int
	repoDelay time.Duration
}

func (f *fakeGitHub) Exchange(context.Context, string, string, string) (auth.Tokens, error) {
	return f.exchanged, nil
}

func (f *fakeGitHub) Refresh(context.Context, string) (auth.Tokens, error) {
	n := f.refreshCalls.Add(1)
	if f.onRefresh != nil {
		f.onRefresh()
	}
	if f.refreshErr != nil {
		return auth.Tokens{}, f.refreshErr
	}
	return auth.Tokens{
		Access:           "access-" + string(rune('0'+n)),
		Refresh:          "refresh-" + string(rune('0'+n)),
		AccessExpiresAt:  t0().Add(100 * time.Hour),
		RefreshExpiresAt: t0().Add(1000 * time.Hour),
	}, nil
}

func (f *fakeGitHub) Revoke(_ context.Context, accessToken string) error {
	f.revoked.Store(accessToken)
	return errors.New("revoke refused")
}

func (*fakeGitHub) User(context.Context, string) (auth.Profile, error) {
	return auth.Profile{Login: "octocat"}, nil
}

func (f *fakeGitHub) AccessibleRepos(context.Context, string) ([]auth.Repo, error) {
	f.mu.Lock()
	f.repoCalls++
	repos := slices.Clone(f.repos)
	reposErr, delay := f.reposErr, f.repoDelay
	f.mu.Unlock()
	time.Sleep(delay)
	return repos, reposErr
}

func (f *fakeGitHub) setRepos(repos ...auth.Repo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = repos
}

func (f *fakeGitHub) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoCalls
}

// countingStore counts DeleteExpired calls.
type countingStore struct {
	*sqlite.Store
	sweeps atomic.Int32
}

func (c *countingStore) DeleteExpired(ctx context.Context, idleBefore, now time.Time) (int64, error) {
	c.sweeps.Add(1)
	n, err := c.Store.DeleteExpired(ctx, idleBefore, now)
	if err != nil {
		return n, fmt.Errorf("delete expired: %w", err)
	}
	return n, nil
}

type env struct {
	svc   *auth.Service
	store *sqlite.Store
	gh    *fakeGitHub
	now   *time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() = %v, want nil error", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() = %v, want nil error", err)
		}
	})
	e := &env{store: store, now: new(time.Time)}
	*e.now = t0()
	e.gh = &fakeGitHub{exchanged: auth.Tokens{
		Access:           "access-0",
		Refresh:          "refresh-0",
		AccessExpiresAt:  t0().Add(8 * time.Hour),
		RefreshExpiresAt: t0().Add(24 * 30 * time.Hour),
	}}
	e.svc = auth.NewService(store, e.gh, auth.Options{
		ClientID:     "cid",
		AuthorizeURL: "https://github.example/authorize",
		RedirectURL:  "https://pollux.example/auth/callback",
		Key:          [32]byte{1},
		Now:          func() time.Time { return *e.now },
	})
	return e
}

func (e *env) login(t *testing.T) string {
	t.Helper()
	authorizeURL, binding, err := e.svc.BeginLogin(t.Context())
	if err != nil {
		t.Fatalf("BeginLogin() = %v, want nil error", err)
	}
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	id, err := e.svc.CompleteLogin(t.Context(), u.Query().Get("state"), binding, "code")
	if err != nil {
		t.Fatalf("CompleteLogin() = %v, want nil error", err)
	}
	return id
}

func (e *env) lastUsed(t *testing.T, id string) time.Time {
	t.Helper()
	sum := sha256.Sum256([]byte(id))
	s, ok, err := e.store.Session(t.Context(), sum[:])
	if err != nil || !ok {
		t.Fatalf("Session() = ok %v, err %v, want ok", ok, err)
	}
	return s.LastUsedAt
}

func TestAuthenticateSlidesExpiry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.login(t)

	*e.now = t0().Add(6 * 24 * time.Hour)
	if _, err := e.svc.Authenticate(t.Context(), id); err != nil {
		t.Fatalf("Authenticate() after 6 days = %v, want nil error", err)
	}
	if got, want := e.lastUsed(t, id), *e.now; !got.Equal(want) {
		t.Errorf("last_used_at = %v, want %v after use", got, want)
	}

	usedAt := *e.now
	*e.now = usedAt.Add(30 * time.Second)
	if _, err := e.svc.Authenticate(t.Context(), id); err != nil {
		t.Fatalf("Authenticate() 30s later = %v, want nil error", err)
	}
	if got := e.lastUsed(t, id); !got.Equal(usedAt) {
		t.Errorf("last_used_at = %v, want %v: writes are limited to one a minute", got, usedAt)
	}

	*e.now = usedAt.Add(6 * 24 * time.Hour)
	if _, err := e.svc.Authenticate(t.Context(), id); err != nil {
		t.Errorf("Authenticate() 6 days after last use = %v, want nil error", err)
	}
	*e.now = e.now.Add(auth.SessionIdle + time.Second)
	if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("Authenticate() after 7 idle days = %v, want ErrUnauthenticated", err)
	}
}

func TestAuthenticateRefreshTokenExpired(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.gh.exchanged.RefreshExpiresAt = t0().Add(time.Hour)
	id := e.login(t)

	*e.now = t0().Add(2 * time.Hour)
	if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("Authenticate() past refresh expiry = %v, want ErrUnauthenticated", err)
	}
}

func TestAccessToken(t *testing.T) {
	t.Parallel()

	t.Run("valid token is not refreshed", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		v, _ := e.svc.Authenticate(t.Context(), e.login(t))
		got, err := e.svc.AccessToken(t.Context(), v)
		if err != nil || got != "access-0" || e.gh.refreshCalls.Load() != 0 {
			t.Errorf("AccessToken() = %q, %v with %d refreshes, want access-0 and none", got, err, e.gh.refreshCalls.Load())
		}
	})

	t.Run("near expiry refreshes and persists", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.login(t)
		*e.now = t0().Add(8*time.Hour - 30*time.Second)
		v, err := e.svc.Authenticate(t.Context(), id)
		if err != nil {
			t.Fatalf("Authenticate() = %v, want nil error", err)
		}
		got, err := e.svc.AccessToken(t.Context(), v)
		if err != nil || got != "access-1" {
			t.Fatalf("AccessToken() = %q, %v, want access-1", got, err)
		}

		v, err = e.svc.Authenticate(t.Context(), id)
		if err != nil {
			t.Fatalf("Authenticate() after refresh = %v, want nil error", err)
		}
		got, err = e.svc.AccessToken(t.Context(), v)
		if err != nil || got != "access-1" || e.gh.refreshCalls.Load() != 1 {
			t.Errorf("AccessToken() = %q, %v with %d refreshes, want the stored access-1 and one refresh", got, err, e.gh.refreshCalls.Load())
		}
	})

	t.Run("refused refresh deletes the session", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.login(t)
		*e.now = t0().Add(9 * time.Hour)
		v, _ := e.svc.Authenticate(t.Context(), id)
		e.gh.refreshErr = errors.Join(errors.New("bad_refresh_token"), auth.ErrRefreshRefused)

		if _, err := e.svc.AccessToken(t.Context(), v); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("AccessToken() = %v, want ErrUnauthenticated", err)
		}
		if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Authenticate() after refused refresh = %v, want ErrUnauthenticated", err)
		}
	})

	t.Run("other refresh failure keeps the session", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.login(t)
		*e.now = t0().Add(9 * time.Hour)
		v, _ := e.svc.Authenticate(t.Context(), id)
		e.gh.refreshErr = errors.New("GitHub answered HTTP 502")

		if _, err := e.svc.AccessToken(t.Context(), v); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("AccessToken() = %v, want ErrUnauthenticated", err)
		}
		if _, err := e.svc.Authenticate(t.Context(), id); err != nil {
			t.Errorf("Authenticate() after a failed refresh = %v, want the session kept", err)
		}
	})
}

func TestAccessTokenRefreshSurvivesCancel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.login(t)
	*e.now = t0().Add(9 * time.Hour)
	v, err := e.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil error", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	e.gh.onRefresh = cancel
	if got, err := e.svc.AccessToken(ctx, v); err != nil || got != "access-1" {
		t.Fatalf("AccessToken() with the client gone = %q, %v, want access-1", got, err)
	}

	e.gh.onRefresh = nil
	v, err = e.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() after refresh = %v, want nil error", err)
	}
	if got, err := e.svc.AccessToken(t.Context(), v); err != nil || got != "access-1" || e.gh.refreshCalls.Load() != 1 {
		t.Errorf("AccessToken() = %q, %v with %d refreshes, want the stored access-1 and one refresh", got, err, e.gh.refreshCalls.Load())
	}
}

func TestUnreadableTokensDeleteSession(t *testing.T) {
	t.Parallel()

	rotated := func(e *env) *auth.Service {
		return auth.NewService(e.store, e.gh, auth.Options{Key: [32]byte{2}, Now: func() time.Time { return *e.now }})
	}

	t.Run("Authenticate", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.login(t)
		if _, err := rotated(e).Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("Authenticate() with a rotated key = %v, want ErrUnauthenticated", err)
		}
		if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Authenticate() with the old key after = %v, want the session deleted", err)
		}
	})

	t.Run("accessToken", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		id := e.login(t)
		v, _ := e.svc.Authenticate(t.Context(), id)
		*e.now = t0().Add(9 * time.Hour)
		if _, err := rotated(e).AccessToken(t.Context(), v); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("AccessToken() with a rotated key = %v, want ErrUnauthenticated", err)
		}
		if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Authenticate() after = %v, want the session deleted", err)
		}
	})
}

func TestAccessTokenConcurrentRefreshOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.login(t)
	*e.now = t0().Add(9 * time.Hour)
	v, err := e.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil error", err)
	}

	var wg sync.WaitGroup
	tokens := make([]string, 2)
	errs := make([]error, 2)
	for i := range tokens {
		wg.Go(func() { tokens[i], errs[i] = e.svc.AccessToken(t.Context(), v) })
	}
	wg.Wait()

	for i := range tokens {
		if errs[i] != nil || tokens[i] != "access-1" {
			t.Errorf("AccessToken() #%d = %q, %v, want access-1", i, tokens[i], errs[i])
		}
	}
	if n := e.gh.refreshCalls.Load(); n != 1 {
		t.Errorf("GitHub refresh calls = %d, want 1", n)
	}
	if n := e.svc.SessionLocks(); n != 0 {
		t.Errorf("session locks after the refreshes = %d, want 0", n)
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.login(t)

	err := e.svc.Logout(t.Context(), id)
	if !errors.Is(err, auth.ErrRevokeFailed) {
		t.Fatalf("Logout() = %v, want ErrRevokeFailed from the failed revoke", err)
	}
	if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("Authenticate() after logout = %v, want ErrUnauthenticated", err)
	}
	if got := e.gh.revoked.Load(); got != "access-0" {
		t.Errorf("revoked token = %v, want access-0", got)
	}
	if err := e.svc.Logout(t.Context(), id); err != nil {
		t.Errorf("Logout() of an unknown session = %v, want nil error", err)
	}
}

func TestBeginLoginSweepsOncePerMinute(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	store := &countingStore{Store: e.store}
	svc := auth.NewService(store, e.gh, auth.Options{AuthorizeURL: "https://github.example/authorize", Now: func() time.Time { return *e.now }})

	begin := func() {
		t.Helper()
		if _, _, err := svc.BeginLogin(t.Context()); err != nil {
			t.Fatalf("BeginLogin() = %v, want nil error", err)
		}
	}
	begin()
	begin()
	if n := store.sweeps.Load(); n != 1 {
		t.Errorf("DeleteExpired calls after two logins = %d, want 1", n)
	}
	*e.now = e.now.Add(time.Minute + time.Second)
	begin()
	if n := store.sweeps.Load(); n != 2 {
		t.Errorf("DeleteExpired calls a minute later = %d, want 2", n)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v, err := e.svc.Authenticate(t.Context(), e.login(t))
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil error", err)
	}
	opts := auth.Options{ClientID: "cid", Key: [32]byte{0x41, 0x42}}
	tokens := e.gh.exchanged

	for name, val := range map[string]any{"Tokens": tokens, "&Tokens": &tokens, "Viewer": v, "Options": opts} {
		for _, verb := range []string{"%v", "%+v"} {
			out := fmt.Sprintf(verb, val)
			for _, secret := range []string{"access-0", "refresh-0", "AB", "65 66"} {
				if strings.Contains(out, secret) {
					t.Errorf("Sprintf(%q, %s) = %q, want no %q", verb, name, out, secret)
				}
			}
		}
		var buf strings.Builder
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "v", val)
		for _, secret := range []string{"access-0", "refresh-0", "AB", "65,66"} {
			if strings.Contains(buf.String(), secret) {
				t.Errorf("slog of %s = %s, want no %q", name, buf.String(), secret)
			}
		}
	}
	if got := fmt.Sprintf("%+v", v); !strings.Contains(got, "octocat") {
		t.Errorf("Sprintf(%%+v, Viewer) = %q, want the login", got)
	}
}

func TestBeginLoginCleansExpiredSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	old := e.login(t)

	*e.now = t0().Add(auth.SessionIdle + time.Hour)
	e.gh.exchanged.RefreshExpiresAt = e.now.Add(time.Hour)
	e.login(t)

	if _, err := e.svc.Authenticate(t.Context(), old); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("Authenticate() of the idle session = %v, want ErrUnauthenticated", err)
	}
	if n, err := e.store.DeleteExpired(t.Context(), e.now.Add(-auth.SessionIdle), *e.now); err != nil || n != 0 {
		t.Errorf("DeleteExpired() after login = %d, %v, want 0: the idle session is already gone", n, err)
	}
}

// flakyStore fails the next swapFailures SwapTokens and sweepFailures
// DeleteExpired calls, and counts the sweeps.
type flakyStore struct {
	*sqlite.Store
	swapFailures  atomic.Int32
	sweepFailures atomic.Int32
	sweeps        atomic.Int32
}

func (f *flakyStore) SwapTokens(ctx context.Context, idHash []byte, prev int64, sealed []byte, accessExp, refreshExp time.Time) (bool, error) {
	if f.swapFailures.Add(-1) >= 0 {
		return false, errors.New("disk full")
	}
	ok, err := f.Store.SwapTokens(ctx, idHash, prev, sealed, accessExp, refreshExp)
	if err != nil {
		return ok, fmt.Errorf("swap tokens: %w", err)
	}
	return ok, nil
}

func (f *flakyStore) DeleteExpired(ctx context.Context, idleBefore, now time.Time) (int64, error) {
	f.sweeps.Add(1)
	if f.sweepFailures.Add(-1) >= 0 {
		return 0, errors.New("database locked")
	}
	n, err := f.Store.DeleteExpired(ctx, idleBefore, now)
	if err != nil {
		return n, fmt.Errorf("delete expired: %w", err)
	}
	return n, nil
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, _ = b.buf.Write(p) // strings.Builder.Write always returns a nil error
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// withFlakyStore swaps e's service for one over a flakyStore that logs to logs.
func withFlakyStore(e *env, logs io.Writer) *flakyStore {
	store := &flakyStore{Store: e.store}
	e.svc = auth.NewService(store, e.gh, auth.Options{
		AuthorizeURL: "https://github.example/authorize",
		Key:          [32]byte{1},
		Now:          func() time.Time { return *e.now },
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	})
	return store
}

func TestRefreshRetriesFailedStore(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var logs syncBuffer
	store := withFlakyStore(e, &logs)
	e.gh.setRepos(auth.Repo{Owner: "acme", Name: "widgets", InstallationID: 7})
	id := e.login(t)
	*e.now = t0().Add(9 * time.Hour)
	v, err := e.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil error", err)
	}

	store.swapFailures.Store(1)
	if _, err := e.svc.RepoAccess(t.Context(), v, "acme", "widgets"); err != nil {
		t.Fatalf("RepoAccess() after one failed store = %v, want nil error", err)
	}
	v, err = e.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() after refresh = %v, want nil error", err)
	}
	if got, err := e.svc.AccessToken(t.Context(), v); err != nil || got != "access-1" || e.gh.refreshCalls.Load() != 1 {
		t.Errorf("AccessToken() = %q, %v with %d refreshes, want the stored access-1 and one refresh", got, err, e.gh.refreshCalls.Load())
	}
}

func TestRefreshStoreAlwaysFailing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var logs syncBuffer
	store := withFlakyStore(e, &logs)
	id := e.login(t)
	*e.now = t0().Add(9 * time.Hour)
	v, err := e.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil error", err)
	}

	store.swapFailures.Store(1000)
	if _, err := e.svc.AccessToken(t.Context(), v); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("AccessToken() with a failing store = %v, want ErrUnauthenticated", err)
	}
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "store refreshed tokens") {
		t.Errorf("logs = %q, want an error about the failed store", got)
	}
}

func TestFailedSweepRetriesAndLogs(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var logs syncBuffer
	store := withFlakyStore(e, &logs)
	store.sweepFailures.Store(1)

	for range 2 {
		if _, _, err := e.svc.BeginLogin(t.Context()); err != nil {
			t.Fatalf("BeginLogin() = %v, want nil error", err)
		}
	}
	if n := store.sweeps.Load(); n != 2 {
		t.Errorf("DeleteExpired calls after a failed sweep and a retry = %d, want 2", n)
	}
	if got := logs.String(); !strings.Contains(got, "database locked") {
		t.Errorf("logs = %q, want the failed sweep", got)
	}
}

func TestCompleteLoginDoesNotSweep(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var logs syncBuffer
	store := withFlakyStore(e, &logs)
	e.login(t)
	if n := store.sweeps.Load(); n != 1 {
		t.Errorf("DeleteExpired calls for BeginLogin plus CompleteLogin = %d, want 1", n)
	}
}

func TestLogoutSurvivesCancelledContext(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.login(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := e.svc.Logout(ctx, id); err != nil && !errors.Is(err, auth.ErrRevokeFailed) {
		t.Fatalf("Logout() with the client gone = %v, want nil or ErrRevokeFailed", err)
	}
	if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("Authenticate() after logout with the client gone = %v, want ErrUnauthenticated", err)
	}
}
