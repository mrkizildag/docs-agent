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
		"GET /repos/o/r/git/trees/tree0": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"sha":"tree0","tree":[{"path":"docs","mode":"040000","type":"tree","sha":"treedocs"},{"path":"README.md","mode":"100644","type":"blob"}]}`)
		},
		"GET /repos/o/r/git/trees/treedocs": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"sha":"treedocs","tree":[{"path":"a.md","mode":"100755","type":"blob"},{"path":"link.md","mode":"120000","type":"blob"},{"path":"sub","mode":"160000","type":"commit"},{"path":"guides","mode":"040000","type":"tree","sha":"treeguides"}]}`)
		},
		"GET /repos/o/r/git/trees/treeguides": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"sha":"treeguides","tree":[{"path":"x.md","mode":"100755","type":"blob"}]}`)
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
		"tree":   {"base_tree": "tree0", "tree": []any{map[string]any{"path": "docs/a.md", "mode": "100755", "type": "blob", "sha": "blob1"}}},
		"commit": {"message": "docs: apply", "tree": "tree1", "parents": []any{"parent1"}},
		"ref":    {"sha": "commit1", "force": false},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("requests (-want +got):\n%s", diff)
	}
}

func TestCommitFilesBranchMoved(t *testing.T) {
	t.Parallel()

	routes := commitRoutes(t, map[string]map[string]any{}, http.StatusUnprocessableEntity, `{"message":"Update is not a fast forward"}`)
	routes["GET /repos/o/r/branches/feature"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"name":"feature","commit":{"sha":"moved"}}`)
	}
	client := newCommentsClient(t, routes)

	_, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: "docs/a.md", Content: "x"}}, "m")
	if !errors.Is(err, gate.ErrBranchMoved) {
		t.Errorf("CommitFiles() error = %v, want %v", err, gate.ErrBranchMoved)
	}
}

func TestCommitFilesRefRejectedBranchUnmoved(t *testing.T) {
	t.Parallel()

	routes := commitRoutes(t, map[string]map[string]any{}, http.StatusUnprocessableEntity, `{"message":"Protected branch update failed"}`)
	routes["GET /repos/o/r/branches/feature"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"name":"feature","commit":{"sha":"parent1"}}`)
	}
	client := newCommentsClient(t, routes)

	_, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: "docs/a.md", Content: "x"}}, "m")
	if !errors.Is(err, gate.ErrCommitRejected) || errors.Is(err, gate.ErrBranchMoved) {
		t.Errorf("CommitFiles() error = %v, want ErrCommitRejected and not ErrBranchMoved", err)
	}
}

func TestCommitFilesRefusesSymlinkAndSubmodule(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"docs/link.md", "docs/sub"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			got := map[string]map[string]any{}
			client := newCommentsClient(t, commitRoutes(t, got, http.StatusOK, `{}`))

			if _, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: path, Content: "x"}}, "m"); !errors.Is(err, gate.ErrCommitRejected) {
				t.Errorf("CommitFiles() error = %v, want ErrCommitRejected", err)
			}
			if _, ok := got["ref"]; ok {
				t.Error("ref was updated, want no update")
			}
		})
	}
}

func TestCommitFilesNewFileMode(t *testing.T) {
	t.Parallel()

	got := map[string]map[string]any{}
	client := newCommentsClient(t, commitRoutes(t, got, http.StatusOK, `{"ref":"refs/heads/feature","object":{"sha":"commit1"}}`))

	if _, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: "docs/new.md", Content: "x"}}, "m"); err != nil {
		t.Fatalf("CommitFiles() error = %v", err)
	}
	entries, ok := got["tree"]["tree"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("tree request entries = %v, want one entry", got["tree"]["tree"])
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("tree entry = %T, want object", entries[0])
	}
	if entry["mode"] != "100644" {
		t.Errorf("new file mode = %v, want 100644", entry["mode"])
	}
}

func TestCommitFilesModesByPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want string
	}{
		{"docs/guides/x.md", "100755"},
		{"docs/guides/new.md", "100644"},
		{"docs/fresh/deep/new.md", "100644"},
		{"top.md", "100644"},
		{"README.md", "100644"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			got := map[string]map[string]any{}
			client := newCommentsClient(t, commitRoutes(t, got, http.StatusOK, `{"ref":"refs/heads/feature","object":{"sha":"commit1"}}`))

			if _, err := client.CommitFiles(t.Context(), 1, "o", "r", "feature", "parent1", []gate.FileChange{{Path: tc.path, Content: "x"}}, "m"); err != nil {
				t.Fatalf("CommitFiles() error = %v", err)
			}
			entries, _ := got["tree"]["tree"].([]any)
			if len(entries) != 1 {
				t.Fatalf("tree entries = %v, want one", got["tree"]["tree"])
			}
			entry, _ := entries[0].(map[string]any)
			if entry["mode"] != tc.want {
				t.Errorf("mode = %v, want %s", entry["mode"], tc.want)
			}
		})
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
			writeJSON(t, w, http.StatusOK, `{"name":"feature","commit":{"sha":"head1","commit":{"message":"docs: apply\n\nbody"},"author":{"login":"pollux-agent[bot]"},"parents":[{"sha":"p1"},{"sha":"p2"}]}}`)
		},
	})

	got, err := client.BranchCommit(t.Context(), 1, "o", "r", "feature")
	if err != nil {
		t.Fatalf("BranchCommit() error = %v", err)
	}
	want := gate.Commit{SHA: "head1", Message: "docs: apply\n\nbody", Parents: []string{"p1", "p2"}, Mine: true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("BranchCommit() (-want +got):\n%s", diff)
	}
}

func TestCommitAt(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/abc": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"sha":"abc","commit":{"message":"docs: apply"},"author":{"login":"pollux-agent[bot]"},"parents":[{"sha":"p1"}]}`)
		},
	})

	got, err := client.CommitAt(t.Context(), 1, "o", "r", "abc")
	if err != nil {
		t.Fatalf("CommitAt() error = %v", err)
	}
	want := gate.Commit{SHA: "abc", Message: "docs: apply", Parents: []string{"p1"}, Mine: true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("CommitAt() (-want +got):\n%s", diff)
	}
}

func TestCommitAtNotMine(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/commits/abc": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"sha":"abc","commit":{"message":"m"},"author":{"login":"dev"},"parents":[]}`)
		},
	})

	got, err := client.CommitAt(t.Context(), 1, "o", "r", "abc")
	if err != nil || got.Mine {
		t.Errorf("CommitAt() = %+v, %v, want Mine false", got, err)
	}
}

func TestBranchCommitNotMine(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/branches/feature": func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(t, w, http.StatusOK, `{"name":"feature","commit":{"sha":"head1","commit":{"message":"m"},"author":{"login":"dev"},"parents":[]}}`)
		},
	})

	got, err := client.BranchCommit(t.Context(), 1, "o", "r", "feature")
	if err != nil || got.Mine {
		t.Errorf("BranchCommit() = %+v, %v, want Mine false", got, err)
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
