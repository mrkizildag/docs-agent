package httpapi_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
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

type authEnv struct {
	handler   http.Handler
	challenge string
}

func newAuthEnv(t *testing.T) *authEnv {
	t.Helper()
	return newAuthEnvWithLimit(t, httpapi.RateLimitConfig{})
}

func newAuthEnvWithLimit(t *testing.T, authLimit httpapi.RateLimitConfig) *authEnv {
	t.Helper()
	env := &authEnv{}
	gh := fakeGitHub(t, &env.challenge)

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() = %v, want nil error", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	user := ghclient.NewUserClient(&http.Client{Timeout: 5 * time.Second}, "cid", "csecret", gh.URL, gh.URL)
	opts := auth.Options{ClientID: "cid", AuthorizeURL: gh.URL + "/login/oauth/authorize", RedirectURL: "https://pollux.example/auth/callback"}
	opts.Key[0] = 1
	env.handler = httpapi.NewHandler(httpapi.Deps{
		Logger:        slog.New(slog.DiscardHandler),
		WebhookSecret: []byte("secret"),
		Jobs:          newFakeEnqueuer(),
		Runs:          fakeRunLookup{},
		Auth:          auth.NewService(store, user, opts),
		AuthRateLimit: authLimit,
	})
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
		q.Get("redirect_uri") != "https://pollux.example/auth/callback" || q.Get("state") == "" || q.Get("code_challenge") == "" {
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
