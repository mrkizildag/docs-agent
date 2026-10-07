package github_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
)

func TestAccessibleRepos(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, body string) {
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write response: %v", err)
		}
	}
	nextPage := func(w http.ResponseWriter, r *http.Request, page int) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?per_page=100&page=%d>; rel="next"`, r.Host, r.URL.Path, page))
	}
	mux.HandleFunc("GET /user/installations", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			ids := []string{`{"id":1}`, `{"id":2}`}
			for i := range 98 {
				ids = append(ids, fmt.Sprintf(`{"id":%d}`, 1000+i))
			}
			nextPage(w, r, 2)
			reply(w, `{"installations":[`+strings.Join(ids, ",")+`]}`)
		case "2":
			reply(w, `{"installations":[{"id":3}]}`)
		default:
			reply(w, `{"installations":[]}`)
		}
	})
	mux.HandleFunc("GET /user/installations/1/repositories", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			repos := make([]string, 100)
			for i := range repos {
				repos[i] = fmt.Sprintf(`{"name":"r%d","owner":{"login":"acme"}}`, i)
			}
			nextPage(w, r, 2)
			reply(w, `{"repositories":[`+strings.Join(repos, ",")+`]}`)
		case "2":
			reply(w, `{"repositories":[{"name":"last","owner":{"login":"acme"}}]}`)
		default:
			reply(w, `{"repositories":[]}`)
		}
	})
	mux.HandleFunc("GET /user/installations/3/repositories", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, `{"repositories":[{"name":"tools","owner":{"login":"octo"}}]}`)
	})
	mux.HandleFunc("GET /user/installations/2/repositories", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		reply(w, `{"message":"Resource protected by organization SAML enforcement."}`)
	})
	mux.HandleFunc("GET /user/installations/{id}/repositories", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := ghclient.NewUserClient(&http.Client{Timeout: 5 * time.Second}, "cid", "secret", srv.URL, srv.URL)
	got, err := c.AccessibleRepos(t.Context(), "tok")
	if err != nil {
		t.Fatalf("AccessibleRepos() = %v, want nil error", err)
	}
	if len(got) != 102 {
		t.Fatalf("AccessibleRepos() returned %d repos, want 102", len(got))
	}
	for _, tc := range []struct {
		i    int
		want auth.Repo
	}{
		{0, auth.Repo{Owner: "acme", Name: "r0", InstallationID: 1}},
		{100, auth.Repo{Owner: "acme", Name: "last", InstallationID: 1}},
		{101, auth.Repo{Owner: "octo", Name: "tools", InstallationID: 3}},
	} {
		if diff := cmp.Diff(tc.want, got[tc.i]); diff != "" {
			t.Errorf("AccessibleRepos()[%d] (-want +got):\n%s", tc.i, diff)
		}
	}
}

func TestAccessibleReposRefusal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	c := ghclient.NewUserClient(&http.Client{Timeout: 5 * time.Second}, "cid", "secret", srv.URL, srv.URL)
	if _, err := c.AccessibleRepos(t.Context(), "bad"); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("AccessibleRepos() error = %v, want auth.ErrUnauthenticated", err)
	}
}
