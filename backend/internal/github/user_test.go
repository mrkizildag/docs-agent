package github_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
)

func newUserClient(t *testing.T, handler http.HandlerFunc) *ghclient.UserClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return ghclient.NewUserClient(&http.Client{Timeout: 5 * time.Second}, "cid", "csecret", srv.URL, srv.URL)
}

func TestUserClientExchange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		body        string
		want        auth.Tokens
		wantInvalid bool
	}{
		{name: "tokens", status: http.StatusOK, body: `{"access_token":"ghu_a","refresh_token":"ghr_r"}`, want: auth.Tokens{Access: "ghu_a", Refresh: "ghr_r"}},
		{name: "error with HTTP 200", status: http.StatusOK, body: `{"error":"bad_verification_code"}`, wantInvalid: true},
		{name: "unknown client", status: http.StatusNotFound, body: `{"message":"Not Found"}`, wantInvalid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotForm map[string]string
			client := newUserClient(t, func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("ParseForm() = %v", err)
				}
				gotForm = map[string]string{}
				for _, k := range []string{"client_id", "client_secret", "code", "redirect_uri", "code_verifier"} {
					gotForm[k] = r.PostForm.Get(k)
				}
				if r.URL.Path != "/login/oauth/access_token" || r.Header.Get("Accept") != "application/json" {
					t.Errorf("request = %s Accept %q, want /login/oauth/access_token as JSON", r.URL.Path, r.Header.Get("Accept"))
				}
				w.WriteHeader(tc.status)
				if _, err := fmt.Fprint(w, tc.body); err != nil {
					t.Errorf("write response: %v", err)
				}
			})

			got, err := client.Exchange(t.Context(), "the-code", "the-verifier", "https://pollux.example/auth/callback")
			if tc.wantInvalid {
				if !errors.Is(err, auth.ErrInvalidLogin) {
					t.Fatalf("Exchange() error = %v, want auth.ErrInvalidLogin", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Exchange() = %v, want nil error", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Exchange() (-want +got):\n%s", diff)
			}
			wantForm := map[string]string{"client_id": "cid", "client_secret": "csecret", "code": "the-code", "redirect_uri": "https://pollux.example/auth/callback", "code_verifier": "the-verifier"}
			if diff := cmp.Diff(wantForm, gotForm); diff != "" {
				t.Errorf("token request form (-want +got):\n%s", diff)
			}
		})
	}
}

func TestUserClientUser(t *testing.T) {
	t.Parallel()

	client := newUserClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer ghu_a" {
			t.Errorf("request = %s Authorization %q, want GET /user with the bearer token", r.URL.Path, r.Header.Get("Authorization"))
		}
		if _, err := fmt.Fprint(w, `{"login":"octocat","avatar_url":"https://avatars.example/o.png"}`); err != nil {
			t.Errorf("write response: %v", err)
		}
	})

	got, err := client.User(t.Context(), "ghu_a")
	if err != nil {
		t.Fatalf("User() = %v, want nil error", err)
	}
	want := auth.Profile{Login: "octocat", AvatarURL: "https://avatars.example/o.png"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("User() (-want +got):\n%s", diff)
	}
}

func TestUserClientRefresh(t *testing.T) {
	t.Parallel()

	t.Run("rotated tokens", func(t *testing.T) {
		t.Parallel()
		var gotForm map[string]string
		client := newUserClient(t, func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm() = %v", err)
			}
			gotForm = map[string]string{}
			for _, k := range []string{"client_id", "client_secret", "grant_type", "refresh_token"} {
				gotForm[k] = r.PostForm.Get(k)
			}
			if _, err := fmt.Fprint(w, `{"access_token":"ghu_b","refresh_token":"ghr_b"}`); err != nil {
				t.Errorf("write response: %v", err)
			}
		})

		got, err := client.Refresh(t.Context(), "ghr_a")
		if err != nil {
			t.Fatalf("Refresh() = %v, want nil error", err)
		}
		if diff := cmp.Diff(auth.Tokens{Access: "ghu_b", Refresh: "ghr_b"}, got); diff != "" {
			t.Errorf("Refresh() (-want +got):\n%s", diff)
		}
		wantForm := map[string]string{"client_id": "cid", "client_secret": "csecret", "grant_type": "refresh_token", "refresh_token": "ghr_a"}
		if diff := cmp.Diff(wantForm, gotForm); diff != "" {
			t.Errorf("refresh form (-want +got):\n%s", diff)
		}
	})

	t.Run("bad_refresh_token is unauthenticated", func(t *testing.T) {
		t.Parallel()
		client := newUserClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if _, err := fmt.Fprint(w, `{"error":"bad_refresh_token"}`); err != nil {
				t.Errorf("write response: %v", err)
			}
		})
		if _, err := client.Refresh(t.Context(), "ghr_a"); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Refresh() error = %v, want auth.ErrUnauthenticated", err)
		}
	})

	t.Run("server error is not a refusal", func(t *testing.T) {
		t.Parallel()
		client := newUserClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		})
		_, err := client.Refresh(t.Context(), "ghr_a")
		if err == nil || errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Refresh() error = %v, want a non-refusal error", err)
		}
	})
}

func TestUserClientRevoke(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "revoked", status: http.StatusNoContent},
		{name: "refused", status: http.StatusNotFound, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newUserClient(t, func(w http.ResponseWriter, r *http.Request) {
				id, secret, ok := r.BasicAuth()
				body, _ := io.ReadAll(r.Body)
				if r.Method != http.MethodDelete || r.URL.Path != "/applications/cid/token" || !ok || id != "cid" || secret != "csecret" ||
					string(body) != `{"access_token":"ghu_a"}` {
					t.Errorf("request = %s %s basic %q:%q body %s, want DELETE /applications/cid/token with client credentials", r.Method, r.URL.Path, id, secret, body)
				}
				w.WriteHeader(tc.status)
			})
			err := client.Revoke(t.Context(), "ghu_a")
			if (err != nil) != tc.wantErr {
				t.Errorf("Revoke() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
