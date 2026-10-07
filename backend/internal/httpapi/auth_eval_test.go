package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
)

// evalGitHub issues tokens that expire, and can fail refresh or repo listing.
type evalGitHub struct {
	refreshErr error
	reposErr   error
}

func (g *evalGitHub) Exchange(context.Context, string, string, string) (auth.Tokens, error) {
	return auth.Tokens{
		Access: "ghu_secret_access", Refresh: "ghr_secret_refresh",
		AccessExpiresAt: evalT0().Add(8 * time.Hour), RefreshExpiresAt: evalT0().Add(4000 * time.Hour),
	}, nil
}

func (g *evalGitHub) Refresh(context.Context, string) (auth.Tokens, error) {
	return auth.Tokens{}, g.refreshErr
}
func (g *evalGitHub) Revoke(context.Context, string) error { return nil }
func (g *evalGitHub) User(context.Context, string) (auth.Profile, error) {
	return auth.Profile{Login: "octocat"}, nil
}

func (g *evalGitHub) AccessibleRepos(context.Context, string) ([]auth.Repo, error) {
	if g.reposErr != nil {
		return nil, g.reposErr
	}
	return []auth.Repo{{Owner: "acme", Name: "widgets", InstallationID: 7}}, nil
}

func evalT0() time.Time { return time.Unix(1_800_000_000, 0) }

type authEvalEnv struct {
	mux   *http.ServeMux
	now   *time.Time
	dir   string
	login func(t *testing.T) (state string, binding *http.Cookie)
}

func newAuthEvalEnv(t *testing.T, gh auth.GitHubUser) *authEvalEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := evalT0()
	opts := auth.Options{ClientID: "cid", AuthorizeURL: "https://github.example/authorize", RedirectURL: "https://pollux.example/auth/callback", Now: func() time.Time { return now }}
	opts.Key[0] = 9
	svc := auth.NewService(store, gh, opts)
	handler := httpapi.NewHandler(httpapi.Deps{
		Logger: slog.New(slog.DiscardHandler), WebhookSecret: []byte("s"), Jobs: newFakeEnqueuer(), Runs: fakeRunLookup{}, Auth: svc,
	})
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}", httpapi.RequireRepo(slog.New(slog.DiscardHandler), svc,
		func(w http.ResponseWriter, _ *http.Request, _ auth.Viewer, _ auth.Repo) { w.WriteHeader(http.StatusOK) }))
	e := &authEvalEnv{mux: mux, now: &now, dir: dir}
	e.login = func(t *testing.T) (string, *http.Cookie) {
		rec := e.do(t, http.MethodGet, "/auth/login")
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse Location: %v", err)
		}
		for _, c := range rec.Result().Cookies() {
			if c.Name == "pollux_login" {
				return loc.Query().Get("state"), c
			}
		}
		t.Fatal("no binding cookie")
		return "", nil
	}
	return e
}

func (e *authEvalEnv) do(t *testing.T, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *authEvalEnv) signIn(t *testing.T) *http.Cookie {
	t.Helper()
	state, binding := e.login(t)
	rec := e.do(t, http.MethodGet, "/auth/callback?code=c&state="+url.QueryEscape(state), binding)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("callback = %d, set no session cookie", rec.Code)
	return nil
}

func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	return nil
}

// Criterion: a session stays signed in while used. A browser drops the cookie
// at its Max-Age, so a used session must renew the cookie, not only the row.
func TestEvalSessionCookieRenewedOnUse(t *testing.T) {
	t.Parallel()
	e := newAuthEvalEnv(t, &evalGitHub{})
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
	e := newAuthEvalEnv(t, &evalGitHub{})
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
	e := newAuthEvalEnv(t, &evalGitHub{})
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
	gh := &evalGitHub{refreshErr: errors.New("GitHub answered HTTP 502")}
	e := newAuthEvalEnv(t, gh)
	session := e.signIn(t)
	*e.now = evalT0().Add(9 * time.Hour)
	if rec := e.do(t, http.MethodGet, "/api/repos/acme/widgets", session); rec.Code != http.StatusUnauthorized {
		t.Errorf("repo call with failing refresh = %d, want 401", rec.Code)
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

	e := newAuthEvalEnv(t, &evalGitHub{reposErr: reposErr})
	session := e.signIn(t)
	if rec := e.do(t, http.MethodGet, "/api/repos/acme/widgets", session); rec.Code == http.StatusInternalServerError {
		t.Errorf("repo call with a revoked GitHub token = 500 (%v), want 401", reposErr)
	}
}

// Edge probe for sliding sessions: use on day 6 keeps the session alive on day
// 12, and 7 idle days after that sign it out.
func TestEvalSessionSlidesThenIdlesOut(t *testing.T) {
	t.Parallel()
	e := newAuthEvalEnv(t, &evalGitHub{})
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
