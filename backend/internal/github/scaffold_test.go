package github_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func TestDocsExist(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/with/contents/docs", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ref"); got != "abc" {
			t.Errorf("ref = %q, want abc", got)
		}
		writeJSON(t, w, http.StatusOK, `[{"type":"file","name":"README.md","path":"docs/README.md"}]`)
	})
	mux.HandleFunc("GET /repos/o/file/contents/docs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"type":"file","name":"docs","path":"docs","size":0,"encoding":"base64","content":""}`)
	})
	mux.HandleFunc("GET /repos/o/boom/contents/docs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/", http.NotFound)
	client := newTestClient(t, mux)

	tests := []struct {
		repo    string
		want    bool
		wantErr bool
	}{
		{repo: "with", want: true},
		{repo: "file"},
		{repo: "none"},
		{repo: "boom", wantErr: true},
	}
	for _, tc := range tests {
		got, err := client.DocsExist(t.Context(), 1, "o", tc.repo, "abc")
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("DocsExist(%q) = %v, %v; want %v, error=%v", tc.repo, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/r", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"default_branch":"trunk"}`)
	})
	mux.HandleFunc("GET /repos/o/r/branches/trunk", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"name":"trunk","commit":{"sha":"tip1"}}`)
	})
	client := newTestClient(t, mux)

	name, sha, err := client.DefaultBranch(t.Context(), 1, "o", "r")
	if err != nil || name != "trunk" || sha != "tip1" {
		t.Errorf("DefaultBranch() = %q, %q, %v; want trunk, tip1, nil", name, sha, err)
	}
}

func TestCreateBranch(t *testing.T) {
	t.Parallel()

	var body map[string]string
	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("POST /repos/o/new/git/refs", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(t, w, http.StatusCreated, `{"ref":"refs/heads/b"}`)
	})
	mux.HandleFunc("POST /repos/o/exists/git/refs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusUnprocessableEntity, `{"message":"Reference already exists"}`)
	})
	mux.HandleFunc("POST /repos/o/other/git/refs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusUnprocessableEntity, `{"message":"Object does not exist"}`)
	})
	client := newTestClient(t, mux)

	if err := client.CreateBranch(t.Context(), 1, "o", "new", "b", "sha1"); err != nil {
		t.Fatalf("CreateBranch() = %v, want nil error", err)
	}
	if diff := cmp.Diff(map[string]string{"ref": "refs/heads/b", "sha": "sha1"}, body); diff != "" {
		t.Errorf("CreateBranch() body (-want +got):\n%s", diff)
	}
	if err := client.CreateBranch(t.Context(), 1, "o", "exists", "b", "sha1"); !errors.Is(err, gate.ErrBranchExists) {
		t.Errorf("CreateBranch(existing) = %v, want ErrBranchExists", err)
	}
	if err := client.CreateBranch(t.Context(), 1, "o", "other", "b", "sha1"); err == nil || errors.Is(err, gate.ErrBranchExists) {
		t.Errorf("CreateBranch(other 422) = %v, want an error that is not ErrBranchExists", err)
	}
}

func TestBranchSHA(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/r/branches/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("branch"); got != "pollux-agent/docs-scaffold" {
			t.Errorf("branch = %q", got)
		}
		writeJSON(t, w, http.StatusOK, `{"name":"x","commit":{"sha":"tip2"}}`)
	})
	client := newTestClient(t, mux)

	if got, err := client.BranchSHA(t.Context(), 1, "o", "r", "pollux-agent/docs-scaffold"); err != nil || got != "tip2" {
		t.Errorf("BranchSHA() = %q, %v; want tip2, nil", got, err)
	}
}

func TestCreatePullRequest(t *testing.T) {
	t.Parallel()

	var body map[string]any
	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(t, w, http.StatusCreated, `{"number":5,"html_url":"https://github.com/o/r/pull/5"}`)
	})
	client := newTestClient(t, mux)

	got, err := client.CreatePullRequest(t.Context(), 1, "o", "r", gate.NewPullRequest{Title: "t", Body: "b", Head: "h", Base: "main"})
	if err != nil {
		t.Fatalf("CreatePullRequest() = %v, want nil error", err)
	}
	if want := (gate.ScaffoldPR{Number: 5, URL: "https://github.com/o/r/pull/5"}); got != want {
		t.Errorf("CreatePullRequest() = %+v, want %+v", got, want)
	}
	if diff := cmp.Diff(map[string]any{"title": "t", "body": "b", "head": "h", "base": "main"}, body); diff != "" {
		t.Errorf("CreatePullRequest() body (-want +got):\n%s", diff)
	}
}

func TestFindPullRequest(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/found/pulls", func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Get("head") != "o:b" || q.Get("state") != "all" {
			t.Errorf("query = %v, want head=o:b state=all", q)
		}
		writeJSON(t, w, http.StatusOK, `[{"number":6,"html_url":"https://github.com/o/found/pull/6"}]`)
	})
	mux.HandleFunc("GET /repos/o/none/pulls", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `[]`)
	})
	client := newTestClient(t, mux)

	got, ok, err := client.FindPullRequest(t.Context(), 1, "o", "found", "b")
	if want := (gate.ScaffoldPR{Number: 6, URL: "https://github.com/o/found/pull/6"}); err != nil || !ok || got != want {
		t.Errorf("FindPullRequest(found) = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
	if _, ok, err := client.FindPullRequest(t.Context(), 1, "o", "none", "b"); err != nil || ok {
		t.Errorf("FindPullRequest(none) = _, %v, %v; want false, nil", ok, err)
	}
}
