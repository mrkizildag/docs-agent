package auth_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

// newAccessEnv signs in and returns the environment, the session id, and its viewer.
func newAccessEnv(t *testing.T, repos ...auth.Repo) (*env, string, auth.Viewer) {
	t.Helper()
	e := newEnv(t)
	e.gh.setRepos(repos...)
	id := e.login(t)
	v, err := e.svc.Authenticate(t.Context(), id)
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil error", err)
	}
	return e, id, v
}

func TestRepoAccess(t *testing.T) {
	t.Parallel()
	e, _, v := newAccessEnv(t,
		auth.Repo{Owner: "Acme", Name: "Widgets", InstallationID: 7},
		auth.Repo{Owner: "Acme", Name: "kit", InstallationID: 8},
	)

	got, err := e.svc.RepoAccess(t.Context(), v, "acme", "WIDGETS")
	if err != nil {
		t.Fatalf("RepoAccess(acme, WIDGETS) = %v, want nil error", err)
	}
	want := auth.Repo{Owner: "Acme", Name: "Widgets", InstallationID: 7}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RepoAccess() (-want +got):\n%s", diff)
	}

	_, err = e.svc.RepoAccess(t.Context(), v, "acme", "secret")
	if !errors.Is(err, auth.ErrNoAccess) {
		t.Errorf("RepoAccess(acme, secret) = %v, want ErrNoAccess", err)
	}
	_, err = e.svc.RepoAccess(t.Context(), v, "acme", "Kit")
	if !errors.Is(err, auth.ErrNoAccess) {
		t.Errorf("RepoAccess(acme, Kelvin-sign it) = %v, want ErrNoAccess: only ASCII folds", err)
	}
}

func TestRepoAccessCachesForTTL(t *testing.T) {
	t.Parallel()
	e, _, v := newAccessEnv(t, auth.Repo{Owner: "acme", Name: "widgets", InstallationID: 7})

	if _, err := e.svc.RepoAccess(t.Context(), v, "acme", "widgets"); err != nil {
		t.Fatalf("RepoAccess() = %v", err)
	}
	e.gh.setRepos()
	*e.now = e.now.Add(auth.AccessTTL - time.Second)
	if _, err := e.svc.RepoAccess(t.Context(), v, "acme", "widgets"); err != nil {
		t.Errorf("RepoAccess() inside the TTL = %v, want the cached access", err)
	}
	if n := e.gh.fetches(); n != 1 {
		t.Errorf("GitHub fetches inside the TTL = %d, want 1", n)
	}

	*e.now = e.now.Add(2 * time.Second)
	_, err := e.svc.RepoAccess(t.Context(), v, "acme", "widgets")
	if !errors.Is(err, auth.ErrNoAccess) {
		t.Errorf("RepoAccess() after the TTL = %v, want ErrNoAccess", err)
	}
}

func TestReposConcurrentMissesFetchOnce(t *testing.T) {
	t.Parallel()
	e, _, v := newAccessEnv(t, auth.Repo{Owner: "acme", Name: "widgets"})
	e.gh.repoDelay = 50 * time.Millisecond

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := e.svc.Repos(t.Context(), v); err != nil {
				t.Errorf("Repos() = %v, want nil error", err)
			}
		})
	}
	wg.Wait()
	if n := e.gh.fetches(); n != 1 {
		t.Errorf("GitHub fetches for concurrent misses = %d, want 1", n)
	}
}

func TestReposSweepsExpiredEntries(t *testing.T) {
	t.Parallel()
	e, _, v := newAccessEnv(t, auth.Repo{Owner: "acme", Name: "widgets"})
	if _, err := e.svc.Repos(t.Context(), v); err != nil {
		t.Fatalf("Repos() = %v, want nil error", err)
	}

	*e.now = e.now.Add(auth.AccessTTL + time.Second)
	id2 := e.login(t)
	v2, err := e.svc.Authenticate(t.Context(), id2)
	if err != nil {
		t.Fatalf("Authenticate() = %v, want nil error", err)
	}
	if _, err := e.svc.Repos(t.Context(), v2); err != nil {
		t.Fatalf("Repos() = %v, want nil error", err)
	}
	if n := e.svc.AccessEntries(); n != 1 {
		t.Errorf("cached sessions = %d, want 1: the expired entry is swept", n)
	}
}

func TestSessionEndsForgetAccess(t *testing.T) {
	t.Parallel()

	t.Run("logout", func(t *testing.T) {
		t.Parallel()
		e, id, v := newAccessEnv(t, auth.Repo{Owner: "acme", Name: "widgets"})
		if _, err := e.svc.Repos(t.Context(), v); err != nil {
			t.Fatalf("Repos() = %v, want nil error", err)
		}
		_ = e.svc.Logout(t.Context(), id)
		if n := e.svc.AccessEntries(); n != 0 {
			t.Errorf("cached sessions after logout = %d, want 0", n)
		}
	})

	t.Run("refused refresh", func(t *testing.T) {
		t.Parallel()
		e, _, v := newAccessEnv(t, auth.Repo{Owner: "acme", Name: "widgets"})
		if _, err := e.svc.Repos(t.Context(), v); err != nil {
			t.Fatalf("Repos() = %v, want nil error", err)
		}
		*e.now = t0().Add(9 * time.Hour)
		e.gh.refreshErr = fmt.Errorf("bad_refresh_token: %w", auth.ErrRefreshRefused)
		if _, err := e.svc.Repos(t.Context(), v); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("Repos() = %v, want ErrUnauthenticated", err)
		}
		if n := e.svc.AccessEntries(); n != 0 {
			t.Errorf("cached sessions after a refused refresh = %d, want 0", n)
		}
	})
}

func TestReposRevokedAppDeletesSession(t *testing.T) {
	t.Parallel()
	e, id, v := newAccessEnv(t, auth.Repo{Owner: "acme", Name: "widgets"})
	e.gh.reposErr = fmt.Errorf("GitHub answered 401: %w", auth.ErrUnauthenticated)

	if _, err := e.svc.Repos(t.Context(), v); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("Repos() = %v, want ErrUnauthenticated", err)
	}
	if _, err := e.svc.Authenticate(t.Context(), id); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("Authenticate() after a revoked app = %v, want the session deleted", err)
	}
	if n := e.svc.AccessEntries(); n != 0 {
		t.Errorf("cached sessions = %d, want 0", n)
	}
}
