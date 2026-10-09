package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	"github.com/mrkizildag/pollux-agent/backend/internal/config"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
)

const (
	fakeCode      = "the-code"
	fakeToken     = "ghu_token"
	sessionCookie = "__Host-pollux_session"
)

// fakeGitHub serves the OAuth and API endpoints the sign-in flow calls. It
// accepts only fakeCode, and only with the verifier matching the challenge the
// authorize request carried.
func fakeGitHub(t *testing.T, challenge *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() = %v", err)
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != fakeCode || base64.RawURLEncoding.EncodeToString(sum[:]) != *challenge {
			if _, err := fmt.Fprint(w, `{"error":"bad_verification_code"}`); err != nil {
				t.Errorf("write response: %v", err)
			}
			return
		}
		if _, err := fmt.Fprintf(w, `{"access_token":%q,"expires_in":28800,"refresh_token":"ghr_refresh","refresh_token_expires_in":15897600}`, fakeToken); err != nil {
			t.Errorf("write response: %v", err)
		}
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if _, err := fmt.Fprint(w, `{"login":"octocat","avatar_url":"https://avatars.example/octocat.png"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeUser is a configurable auth.GitHubUser. Its tokens expire relative to
// evalT0, the start of an env's clock.
type fakeUser struct {
	refreshErr error
	reposErr   error
	revokeErr  error
	repos      []auth.Repo
}

func newFakeUser() *fakeUser {
	return &fakeUser{repos: []auth.Repo{{Owner: "acme", Name: "widgets", InstallationID: 7}}}
}

func (g *fakeUser) Exchange(context.Context, string, string, string) (auth.Tokens, error) {
	return auth.Tokens{
		Access: "ghu_secret_access", Refresh: "ghr_secret_refresh",
		AccessExpiresAt: evalT0().Add(8 * time.Hour), RefreshExpiresAt: evalT0().Add(4000 * time.Hour),
	}, nil
}

func (g *fakeUser) Refresh(context.Context, string) (auth.Tokens, error) {
	return auth.Tokens{}, g.refreshErr
}
func (g *fakeUser) Revoke(context.Context, string) error { return g.revokeErr }
func (g *fakeUser) User(context.Context, string) (auth.Profile, error) {
	return auth.Profile{Login: "octocat"}, nil
}

func (g *fakeUser) AccessibleRepos(context.Context, string) ([]auth.Repo, error) {
	if g.reposErr != nil {
		return nil, g.reposErr
	}
	return g.repos, nil
}

func evalT0() time.Time { return time.Unix(1_800_000_000, 0) }

const publicOrigin = "https://pollux.example"

// authEnv is the real handler over a real sqlite store. By default GitHub is
// the fakeGitHub server behind the real user client; withGitHub swaps in a fake
// user, and withClock gives the service a clock the test moves through now.
type authEnv struct {
	handler   http.Handler
	challenge string
	now       *time.Time
	dir       string
}

type authEnvConfig struct {
	user  auth.GitHubUser
	clock bool
	limit httpapi.RateLimitConfig
	log   io.Writer
	// origin overrides the handler's public origin.
	origin string
}

type authEnvOption func(*authEnvConfig)

func withGitHub(u auth.GitHubUser) authEnvOption { return func(c *authEnvConfig) { c.user = u } }
func withClock() authEnvOption                   { return func(c *authEnvConfig) { c.clock = true } }
func withPublicOrigin(o string) authEnvOption    { return func(c *authEnvConfig) { c.origin = o } }
func withLogOutput(w io.Writer) authEnvOption    { return func(c *authEnvConfig) { c.log = w } }
func withLimit(l httpapi.RateLimitConfig) authEnvOption {
	return func(c *authEnvConfig) { c.limit = l }
}

func newAuthEnv(t *testing.T, options ...authEnvOption) *authEnv {
	t.Helper()
	var cfg authEnvConfig
	for _, o := range options {
		o(&cfg)
	}
	env := &authEnv{dir: t.TempDir()}
	gh := fakeGitHub(t, &env.challenge)

	store, err := sqlite.Open(t.Context(), filepath.Join(env.dir, "state.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() = %v, want nil error", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	user := cfg.user
	if user == nil {
		user = ghclient.NewUserClient(&http.Client{Timeout: 5 * time.Second}, "cid", "csecret", gh.URL, gh.URL)
	}
	opts := auth.Options{ClientID: "cid", AuthorizeURL: gh.URL + "/login/oauth/authorize", RedirectURL: publicOrigin + "/auth/callback"}
	opts.Key[0] = 1
	if cfg.clock {
		now := evalT0()
		env.now = &now
		opts.Now = func() time.Time { return now }
	}
	origin := publicOrigin
	if cfg.origin != "" {
		origin = cfg.origin
	}
	logger := slog.New(slog.DiscardHandler)
	if cfg.log != nil {
		logger = slog.New(slog.NewTextHandler(cfg.log, nil))
	}
	opts.Logger = logger
	svc := auth.NewService(store, user, opts)
	api := httpapi.NewHandler(httpapi.Deps{
		Logger:        logger,
		WebhookSecret: []byte("secret"),
		Jobs:          newFakeEnqueuer(),
		Runs:          fakeRunLookup{},
		Auth:          svc,
		AuthRateLimit: cfg.limit,
		PublicOrigin:  origin,
	})
	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.HandleFunc("GET /api/repos/{owner}/{repo}", httpapi.RequireRepo(logger, svc,
		func(w http.ResponseWriter, _ *http.Request, _ auth.Viewer, repo auth.Repo) {
			if _, err := fmt.Fprintf(w, "%s/%s#%d", repo.Owner, repo.Name, repo.InstallationID); err != nil {
				t.Errorf("write response: %v", err)
			}
		}))
	env.handler = mux
	return env
}

func (e *authEnv) do(t *testing.T, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// signIn runs a whole login and returns the session cookie. extra cookies ride
// along on the callback, such as a session left over from an earlier login.
func (e *authEnv) signIn(t *testing.T, extra ...*http.Cookie) *http.Cookie {
	t.Helper()
	state, binding := e.login(t)
	rec := e.do(t, http.MethodGet, "/auth/callback?code="+fakeCode+"&state="+url.QueryEscape(state), append([]*http.Cookie{binding}, extra...)...)
	if c := sessionCookieOf(rec); c != nil {
		return c
	}
	t.Fatalf("callback = %d, set no session cookie", rec.Code)
	return nil
}

// sessionCookieOf returns the last session Set-Cookie of rec, the one a browser keeps.
func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	var last *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			last = c
		}
	}
	return last
}

// login runs GET /auth/login and returns the state, the binding cookie, and
// records the PKCE challenge for the fake GitHub.
func (e *authEnv) login(t *testing.T) (state string, binding *http.Cookie) {
	t.Helper()
	rec := e.do(t, http.MethodGet, "/auth/login")
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := loc.Query()
	if loc.Path != "/login/oauth/authorize" || q.Get("client_id") != "cid" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != publicOrigin+"/auth/callback" || q.Get("state") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("authorize URL = %s, want client_id, redirect_uri, state, and an S256 challenge", loc)
	}
	e.challenge = q.Get("code_challenge")

	for _, c := range rec.Result().Cookies() {
		if c.Name == "pollux_login" {
			assertCookieAttrs(t, c)
			return q.Get("state"), c
		}
	}
	t.Fatal("GET /auth/login set no binding cookie")
	return "", nil
}

func assertCookieAttrs(t *testing.T, c *http.Cookie) {
	t.Helper()
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 {
		t.Errorf("cookie %s = %+v, want HttpOnly, Secure, SameSite=Lax, and a lifetime", c.Name, c)
	}
}

func TestSignInFlow(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t)

	if rec := env.do(t, http.MethodGet, "/api/me"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/me without a session = %d, want 401", rec.Code)
	}

	state, binding := env.login(t)
	rec := env.do(t, http.MethodGet, "/auth/callback?code="+fakeCode+"&state="+url.QueryEscape(state), binding)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("GET /auth/callback = %d to %q, want 302 to /", rec.Code, rec.Header().Get("Location"))
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("GET /auth/callback set no session cookie")
	}
	assertCookieAttrs(t, session)
	if session.Path != "/" || session.Value == "" {
		t.Errorf("session cookie = %+v, want a value at Path=/", session)
	}

	rec = env.do(t, http.MethodGet, "/api/me", session)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me with a session = %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	want := map[string]string{"login": "octocat", "avatar_url": "https://avatars.example/octocat.png"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GET /api/me (-want +got):\n%s", diff)
	}

	if rec := env.do(t, http.MethodPost, "/auth/logout", session); rec.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/logout = %d, want 204", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/api/me", session); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/me with the logged-out cookie = %d, want 401", rec.Code)
	}
}

func TestLogoutRefusesForeignOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		origin string
		want   int
	}{
		{name: "foreign origin", origin: "https://evil.example", want: http.StatusForbidden},
		{name: "null origin", origin: "null", want: http.StatusForbidden},
		{name: "public origin", origin: publicOrigin, want: http.StatusNoContent},
		{name: "no origin", want: http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newAuthEnv(t)
			session := env.signIn(t)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
			req.AddCookie(session)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			env.handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("POST /auth/logout with Origin %q = %d, want %d", tc.origin, rec.Code, tc.want)
			}
			wantAfter := http.StatusOK
			if tc.want == http.StatusNoContent {
				wantAfter = http.StatusUnauthorized
			}
			if rec := env.do(t, http.MethodGet, "/api/me", session); rec.Code != wantAfter {
				t.Errorf("GET /api/me after logout with Origin %q = %d, want %d", tc.origin, rec.Code, wantAfter)
			}
		})
	}
}

func TestLogoutAcceptsNormalizedPublicOrigin(t *testing.T) {
	t.Parallel()

	u, err := url.Parse("https://Pollux.Example:443")
	if err != nil {
		t.Fatalf("url.Parse() = %v, want nil error", err)
	}
	dashboard := config.Dashboard{PublicURL: u}
	env := newAuthEnv(t, withPublicOrigin(dashboard.Origin()))
	session := env.signIn(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
	req.AddCookie(session)
	req.Header.Set("Origin", "https://pollux.example")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("POST /auth/logout from %s with PUBLIC_URL %s = %d, want 204", "https://pollux.example", u, rec.Code)
	}
}

func TestLogoutComparesOriginCaseInsensitively(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t)
	session := env.signIn(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil)
	req.AddCookie(session)
	req.Header.Set("Origin", "https://POLLUX.example")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("POST /auth/logout with Origin https://POLLUX.example = %d, want 204", rec.Code)
	}
}

func TestLogoutSucceedsWhenRevokeFails(t *testing.T) {
	t.Parallel()

	gh := newFakeUser()
	gh.revokeErr = errors.New("GitHub answered HTTP 502")
	env := newAuthEnv(t, withClock(), withGitHub(gh))
	session := env.signIn(t)

	rec := env.do(t, http.MethodPost, "/auth/logout", session)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/logout with a failing revoke = %d, want 204", rec.Code)
	}
	if c := sessionCookieOf(rec); c == nil || c.MaxAge >= 0 {
		t.Errorf("logout session cookie = %+v, want a clearing cookie", c)
	}
	if rec := env.do(t, http.MethodGet, "/api/me", session); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/me after logout = %d, want 401", rec.Code)
	}
}

func TestSignInAgainEndsTheOldSession(t *testing.T) {
	t.Parallel()

	env := newAuthEnv(t)
	old := env.signIn(t)

	fresh := env.signIn(t, old)

	if rec := env.do(t, http.MethodGet, "/api/me", old); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/me with the pre-login session = %d, want 401", rec.Code)
	}
	if rec := env.do(t, http.MethodGet, "/api/me", fresh); rec.Code != http.StatusOK {
		t.Errorf("GET /api/me with the new session = %d, want 200", rec.Code)
	}
}

// syncBuffer is a log sink safe for the handler's concurrent writes.
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

func TestSignInAgainLogsFailedRevokeOfTheOldSession(t *testing.T) {
	t.Parallel()

	gh := newFakeUser()
	gh.revokeErr = errors.New("GitHub answered HTTP 502")
	var logs syncBuffer
	env := newAuthEnv(t, withClock(), withGitHub(gh), withLogOutput(&logs))
	old := env.signIn(t)

	fresh := env.signIn(t, old)

	if rec := env.do(t, http.MethodGet, "/api/me", fresh); rec.Code != http.StatusOK {
		t.Errorf("GET /api/me with the new session = %d, want 200", rec.Code)
	}
	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "re-login") {
		t.Errorf("logs after a failed revoke on re-login = %q, want a re-login warning", got)
	}
}

func TestCallbackRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target func(state string) string
		cookie func(binding *http.Cookie) *http.Cookie
	}{
		{name: "wrong state", target: func(string) string { return "/auth/callback?code=" + fakeCode + "&state=wrong" }},
		{name: "wrong code", target: func(state string) string { return "/auth/callback?code=nope&state=" + url.QueryEscape(state) }},
		{name: "no binding cookie", cookie: func(*http.Cookie) *http.Cookie { return nil }},
		{name: "other browser's binding", cookie: func(b *http.Cookie) *http.Cookie {
			return &http.Cookie{Name: b.Name, Value: "other", Path: b.Path, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newAuthEnv(t)
			state, binding := env.login(t)
			target := "/auth/callback?code=" + fakeCode + "&state=" + url.QueryEscape(state)
			if tc.target != nil {
				target = tc.target(state)
			}
			var cookies []*http.Cookie
			if tc.cookie == nil {
				cookies = append(cookies, binding)
			} else if c := tc.cookie(binding); c != nil {
				cookies = append(cookies, c)
			}

			rec := env.do(t, http.MethodGet, target, cookies...)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET /auth/callback = %d, want 400", rec.Code)
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == sessionCookie {
					t.Errorf("callback set a session cookie %+v, want none", c)
				}
			}
		})
	}

	t.Run("reused state", func(t *testing.T) {
		t.Parallel()
		env := newAuthEnv(t)
		state, binding := env.login(t)
		target := "/auth/callback?code=" + fakeCode + "&state=" + url.QueryEscape(state)
		if rec := env.do(t, http.MethodGet, target, binding); rec.Code != http.StatusFound {
			t.Fatalf("first callback = %d, want 302", rec.Code)
		}
		if rec := env.do(t, http.MethodGet, target, binding); rec.Code != http.StatusBadRequest {
			t.Errorf("replayed callback = %d, want 400", rec.Code)
		}
	})
}

func TestAuthRoutesAbsentWithoutAuth(t *testing.T) {
	t.Parallel()

	handler := httpapi.NewHandler(httpapi.Deps{Logger: slog.New(slog.DiscardHandler), WebhookSecret: []byte("s"), Jobs: newFakeEnqueuer(), Runs: fakeRunLookup{}})
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/auth/login"}, {http.MethodGet, "/auth/callback"}, {http.MethodPost, "/auth/logout"}, {http.MethodGet, "/api/me"}} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), route.method, route.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", route.method, route.path, rec.Code)
		}
	}
}
