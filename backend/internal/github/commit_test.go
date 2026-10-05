package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Errorf("decode %s %s body: %v", r.Method, r.URL.Path, err)
	}
	return got
}

func commitRoutes(t *testing.T, got map[string]map[string]any, refStatus int, refBody string) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /repos/o/r/git/commits/parent1": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"sha":"parent1","tree":{"sha":"tree0"}}`)
		},
		"POST /repos/o/r/git/blobs": func(w http.ResponseWriter, r *http.Request) {
			got["blob"] = decodeBody(t, r)
			writeJSON(t, w, http.StatusCreated, `{"sha":"blob1"}`)
		},
		"POST /repos/o/r/git/trees": func(w http.ResponseWriter, r *http.Request) {
			got["tree"] = decodeBody(t, r)
			writeJSON(t, w, http.StatusCreated, `{"sha":"tree1"}`)
		},
		"POST /repos/o/r/git/commits": func(w http.ResponseWriter, r *http.Request) {
			got["commit"] = decodeBody(t, r)
			writeJSON(t, w, http.StatusCreated, `{"sha":"commit1"}`)
		},
		"PATCH /repos/o/r/git/refs/heads/feature": func(w http.ResponseWriter, r *http.Request) {
			got["ref"] = decodeBody(t, r)
			writeJSON(t, w, refStatus, refBody)
		},
	}
}

func TestCommitFiles(t *testing.T) {
	t.Parallel()

	got := map[string]map[string]any{}
	client := newCommentsClient(t, commitRoutes(t, got, http.StatusOK, `{"ref":"refs/heads/feature","object":{"sha":"commit1"}}`))

	sha, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: "docs/a.md", Content: "# A\n"}}, "docs: apply")
	if err != nil || sha != "commit1" {
		t.Fatalf("CommitFiles() = %q, %v, want commit1, nil", sha, err)
	}

	want := map[string]map[string]any{
		"blob":   {"content": "# A\n", "encoding": "utf-8"},
		"tree":   {"base_tree": "tree0", "tree": []any{map[string]any{"path": "docs/a.md", "mode": "100644", "type": "blob", "sha": "blob1"}}},
		"commit": {"message": "docs: apply", "tree": "tree1", "parents": []any{"parent1"}},
		"ref":    {"sha": "commit1", "force": false},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestCommitFilesBranchMoved(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, commitRoutes(t, map[string]map[string]any{}, http.StatusUnprocessableEntity, `{"message":"Update is not a fast forward"}`))

	_, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: "docs/a.md", Content: "x"}}, "m")
	if !errors.Is(err, gate.ErrBranchMoved) {
		t.Errorf("CommitFiles() error = %v, want %v", err, gate.ErrBranchMoved)
	}
}

func TestCommitFilesRefError(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, commitRoutes(t, map[string]map[string]any{}, http.StatusInternalServerError, `{"message":"boom"}`))

	_, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: "docs/a.md", Content: "x"}}, "m")
	if err == nil || errors.Is(err, gate.ErrBranchMoved) {
		t.Errorf("CommitFiles() error = %v, want an error other than ErrBranchMoved", err)
	}
}

func TestBranchCommit(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/branches/feature": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"name":"feature","commit":{"sha":"head1","commit":{"message":"docs: apply\n\nbody"},"parents":[{"sha":"p1"},{"sha":"p2"}]}}`)
		},
	})

	got, err := client.BranchCommit(t.Context(), 1, "o", "r", "feature")
	if err != nil {
		t.Fatalf("BranchCommit() error = %v", err)
	}
	want := gate.Commit{SHA: "head1", Message: "docs: apply\n\nbody", Parents: []string{"p1", "p2"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("BranchCommit() (-want +got):\n%s", diff)
	}
}

func TestBranchCommitNotFound(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/branches/feature": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusNotFound, `{"message":"Branch not found"}`)
		},
	})

	if _, err := client.BranchCommit(t.Context(), 1, "o", "r", "feature"); err == nil {
		t.Error("BranchCommit() error = nil, want an error")
	}
}

func TestPermission(t *testing.T) {
	t.Parallel()

	tests := []struct {
		permission string
		want       bool
	}{
		{"admin", true}, {"maintain", true}, {"write", true}, {"triage", false}, {"read", false}, {"none", false},
	}
	for _, tc := range tests {
		t.Run(tc.permission, func(t *testing.T) {
			t.Parallel()

			client := newCommentsClient(t, map[string]http.HandlerFunc{
				"GET /repos/o/r/collaborators/dev/permission": func(w http.ResponseWriter, _ *http.Request) {
					writeJSON(t, w, http.StatusOK, fmt.Sprintf(`{"permission":%q,"user":{"login":"dev"}}`, tc.permission))
				},
			})

			got, err := client.Permission(t.Context(), 1, "o", "r", "dev")
			if err != nil || got != tc.want {
				t.Errorf("Permission() = %v, %v, want %v, nil", got, err, tc.want)
			}
		})
	}
}

func TestReplyToReviewComment(t *testing.T) {
	t.Parallel()

	var got map[string]any
	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/pulls/7/comments": func(w http.ResponseWriter, r *http.Request) {
			got = decodeBody(t, r)
			writeJSON(t, w, http.StatusCreated, `{"id":99,"html_url":"https://x/99","body":"Applied in abc"}`)
		},
	})

	c, err := client.ReplyToReviewComment(t.Context(), 1, "o", "r", 7, 12, "Applied in abc")
	if err != nil {
		t.Fatalf("ReplyToReviewComment() error = %v", err)
	}
	if c.ID != 99 || c.URL != "https://x/99" {
		t.Errorf("ReplyToReviewComment() = %+v, want ID 99 and URL https://x/99", c)
	}
	if diff := cmp.Diff(map[string]any{"body": "Applied in abc", "in_reply_to": float64(12)}, got); diff != "" {
		t.Errorf("request (-want +got):\n%s", diff)
	}
}
