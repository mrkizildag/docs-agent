package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

func TestRequireRepo(t *testing.T) {
	t.Parallel()
	gh := newFakeUser()
	env := newAuthEnv(t, withClock(), withGitHub(gh))
	cookie := env.signIn(t)

	if rec := env.do(t, http.MethodGet, "/api/repos/Acme/Widgets", cookie); rec.Code != http.StatusOK || rec.Body.String() != "acme/widgets#7" {
		t.Errorf("accessible repo = %d %q, want 200 acme/widgets#7", rec.Code, rec.Body.String())
	}
	if rec := env.do(t, http.MethodGet, "/api/repos/acme/widgets"); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", rec.Code)
	}
	forged := &http.Cookie{Name: sessionCookie, Value: "forged", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
	if rec := env.do(t, http.MethodGet, "/api/repos/acme/widgets", forged); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown session = %d, want 401", rec.Code)
	}

	missing := env.do(t, http.MethodGet, "/api/repos/acme/unknown", cookie)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown repo = %d, want 404", missing.Code)
	}
	foreign := env.do(t, http.MethodGet, "/api/repos/other/private", cookie)
	if foreign.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
		t.Errorf("uninstalled repo = %d %q, want the same 404 as an unknown repo (%q)", foreign.Code, foreign.Body.String(), missing.Body.String())
	}

	gh.repos = nil
	if rec := env.do(t, http.MethodGet, "/api/repos/acme/widgets", cookie); rec.Code != http.StatusOK {
		t.Errorf("repo inside the TTL after losing access = %d, want 200 from the cache", rec.Code)
	}
	*env.now = env.now.Add(auth.AccessTTL + time.Second)
	if rec := env.do(t, http.MethodGet, "/api/repos/acme/widgets", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("repo after the TTL after losing access = %d, want 404", rec.Code)
	}
}
