package httpapi_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
)

// Criterion: a session stays signed in while used. A browser drops the cookie
// at its Max-Age, so a used session must renew the cookie, not only the row.
func TestEvalSessionCookieRenewedOnUse(t *testing.T) {
	t.Parallel()
	e := newAuthEnv(t, withClock(), withGitHub(newFakeUser()))
	session := e.signIn(t)

	*e.now = evalT0().Add(6 * 24 * time.Hour)
	rec := e.do(t, http.MethodGet, "/api/me", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me after 6 days of use = %d, want 200", rec.Code)
	}
	renewed := sessionCookieOf(rec)
	if renewed == nil || renewed.MaxAge < int((6*24*time.Hour).Seconds()) {
		t.Errorf("GET /api/me on day 6 renewed cookie = %+v, want Set-Cookie with Max-Age ~7 days; the login cookie (Max-Age %d) expires in the browser on day 7 despite use", renewed, session.MaxAge)
	}
}

// Criterion: an expired state is rejected and creates no session.
func TestEvalExpiredStateRefused(t *testing.T) {
	t.Parallel()
	e := newAuthEnv(t, withClock(), withGitHub(newFakeUser()))
	state, binding := e.login(t)
	*e.now = evalT0().Add(auth.LoginTTL + time.Second)
	rec := e.do(t, http.MethodGet, "/auth/callback?code=c&state="+url.QueryEscape(state), binding)
	if rec.Code != http.StatusBadRequest || sessionCookieOf(rec) != nil {
		t.Errorf("expired state callback = %d, cookie %+v; want 400 and no session", rec.Code, sessionCookieOf(rec))
	}
}

// Criterion: the GitHub token is not stored in plain text.
func TestEvalTokensNotInDatabase(t *testing.T) {
	t.Parallel()
	e := newAuthEnv(t, withClock(), withGitHub(newFakeUser()))
	session := e.signIn(t)
	files, err := filepath.Glob(filepath.Join(e.dir, "state.db*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob db files = %v, %v", files, err)
	}
	root, err := os.OpenRoot(e.dir)
	if err != nil {
		t.Fatalf("open %s: %v", e.dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	for _, f := range files {
		b, err := root.ReadFile(filepath.Base(f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, secret := range []string{"ghu_secret_access", "ghr_secret_refresh", session.Value} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s contains %q in plain text", filepath.Base(f), secret)
			}
		}
	}
}

// Criterion: if refresh fails, the user gets 401, never a 500.
func TestEvalRefreshFailureIs401(t *testing.T) {
	t.Parallel()
	gh := newFakeUser()
	gh.refreshErr = errors.New("GitHub answered HTTP 502")
	e := newAuthEnv(t, withClock(), withGitHub(gh))
	session := e.signIn(t)
	*e.now = evalT0().Add(9 * time.Hour)
	if rec := e.do(t, http.MethodGet, "/api/repos/acme/widgets", session); rec.Code != http.StatusUnauthorized {
		t.Errorf("repo call with failing refresh = %d, want 401", rec.Code)
	}
}

// A 401 clears the session cookie, overriding the renewal set earlier in the
// same response.
func TestRepoCallTransientRefreshFailureKeepsSessionCookie(t *testing.T) {
	t.Parallel()
	gh := newFakeUser()
	gh.refreshErr = errors.New("GitHub answered HTTP 502")
	e := newAuthEnv(t, withClock(), withGitHub(gh))
	session := e.signIn(t)
	*e.now = evalT0().Add(9 * time.Hour)

	rec := e.do(t, http.MethodGet, "/api/repos/acme/widgets", session)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("repo call with failing refresh = %d, want 401", rec.Code)
	}
	if c := sessionCookieOf(rec); c != nil && c.MaxAge < 0 {
		t.Errorf("session Set-Cookie on a transient 401 = %+v, want the cookie kept", c)
	}
	gh.refreshErr = nil
	if rec := e.do(t, http.MethodGet, "/api/me", session); rec.Code != http.StatusOK {
		t.Errorf("/api/me after the refresh recovers = %d, want 200: the session must survive", rec.Code)
	}
}

// Edge probe for losing access: a user who revokes the app's authorization on
// GitHub makes the stored token answer 401; the dashboard must not 500.
func TestEvalRevokedTokenOnRepoCall(t *testing.T) {
	t.Parallel()
	ghAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"message":"Bad credentials"}`)
	}))
	t.Cleanup(ghAPI.Close)
	client := ghclient.NewUserClient(ghAPI.Client(), "cid", "cs", ghAPI.URL, ghAPI.URL)
	_, reposErr := client.AccessibleRepos(t.Context(), "ghu_revoked")

	gh := newFakeUser()
	gh.reposErr = reposErr
	e := newAuthEnv(t, withClock(), withGitHub(gh))
	session := e.signIn(t)
	if rec := e.do(t, http.MethodGet, "/api/repos/acme/widgets", session); rec.Code == http.StatusInternalServerError {
		t.Errorf("repo call with a revoked GitHub token = 500 (%v), want 401", reposErr)
	}
}

// Edge probe for sliding sessions: use on day 6 keeps the session alive on day
// 12, and 7 idle days after that sign it out.
func TestEvalSessionSlidesThenIdlesOut(t *testing.T) {
	t.Parallel()
	e := newAuthEnv(t, withClock(), withGitHub(newFakeUser()))
	session := e.signIn(t)

	*e.now = evalT0().Add(6 * 24 * time.Hour)
	if rec := e.do(t, http.MethodGet, "/api/me", session); rec.Code != http.StatusOK {
		t.Fatalf("day 6 = %d, want 200", rec.Code)
	}
	*e.now = evalT0().Add(12 * 24 * time.Hour)
	if rec := e.do(t, http.MethodGet, "/api/me", session); rec.Code != http.StatusOK {
		t.Fatalf("day 12 after use on day 6 = %d, want 200", rec.Code)
	}
	*e.now = evalT0().Add(19*24*time.Hour + time.Minute)
	if rec := e.do(t, http.MethodGet, "/api/me", session); rec.Code != http.StatusUnauthorized {
		t.Errorf("7 idle days after day 12 = %d, want 401", rec.Code)
	}
}
