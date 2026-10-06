package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
)

func newCommentsClient(t *testing.T, routes map[string]http.HandlerFunc) *ghclient.Client {
	t.Helper()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	if _, ok := routes["GET /app"]; !ok {
		mux.HandleFunc("GET /app", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprint(w, `{"id":1,"slug":"pollux-agent"}`); err != nil {
				t.Errorf("write app response: %v", err)
			}
		})
	}
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}

	client := newTestClient(t, mux)
	return client
}

func TestCreateReviewComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   gate.ReviewComment
		want map[string]any
	}{
		{
			name: "single line",
			in:   gate.ReviewComment{CommitSHA: "abc", Path: "a.go", Line: 4, Body: "hi"},
			want: map[string]any{"body": "hi", "commit_id": "abc", "path": "a.go", "line": float64(4), "side": "RIGHT"},
		},
		{
			name: "multi line",
			in:   gate.ReviewComment{CommitSHA: "abc", Path: "a.go", StartLine: 2, Line: 4, Body: "hi"},
			want: map[string]any{"body": "hi", "commit_id": "abc", "path": "a.go", "start_line": float64(2), "line": float64(4), "side": "RIGHT", "start_side": "RIGHT"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got map[string]any
			client := newCommentsClient(t, map[string]http.HandlerFunc{
				"POST /repos/o/r/pulls/7/comments": func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Errorf("decode request body: %v", err)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusCreated)
					if _, err := fmt.Fprint(w, `{"id":55,"html_url":"https://gh/c/55","body":"hi"}`); err != nil {
						t.Errorf("write response: %v", err)
					}
				},
			})

			c, err := client.CreateReviewComment(t.Context(), 99, "o", "r", 7, tc.in)
			if err != nil {
				t.Fatalf("CreateReviewComment(%+v) = %v, want nil error", tc.in, err)
			}
			if diff := cmp.Diff(gate.Comment{ID: 55, Kind: gate.CommentKindReview, URL: "https://gh/c/55", Body: "hi"}, c); diff != "" {
				t.Errorf("CreateReviewComment() comment (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("request body (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCreateIssueComment(t *testing.T) {
	t.Parallel()

	var got map[string]any
	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"POST /repos/o/r/issues/7/comments": func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			if _, err := fmt.Fprint(w, `{"id":77,"html_url":"https://gh/i/77","body":"summary"}`); err != nil {
				t.Errorf("write response: %v", err)
			}
		},
	})

	c, err := client.CreateIssueComment(t.Context(), 99, "o", "r", 7, "summary")
	if err != nil {
		t.Fatalf("CreateIssueComment() = %v, want nil error", err)
	}
	if diff := cmp.Diff(gate.Comment{ID: 77, Kind: gate.CommentKindIssue, URL: "https://gh/i/77", Body: "summary"}, c); diff != "" {
		t.Errorf("CreateIssueComment() comment (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]any{"body": "summary"}, got); diff != "" {
		t.Errorf("request body (-want +got):\n%s", diff)
	}
}

func writePage(t *testing.T, w http.ResponseWriter, r *http.Request, nextPath string, pages map[string]string) {
	t.Helper()

	page := r.URL.Query().Get("page")
	if r.URL.Query().Get("per_page") != "100" {
		t.Errorf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
	}
	if page == "" {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, nextPath))
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := fmt.Fprint(w, pages[page]); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestListComments(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/pulls/7/comments": func(w http.ResponseWriter, r *http.Request) {
			writePage(t, w, r, "/repos/o/r/pulls/7/comments", map[string]string{
				"":  `[{"id":1,"html_url":"https://gh/r/1","body":"r1","path":"a.go","start_line":2,"line":4,"user":{"login":"pollux-agent[bot]"}}]`,
				"2": `[{"id":2,"html_url":"https://gh/r/2","body":"r2"}]`,
			})
		},
		"GET /repos/o/r/issues/7/comments": func(w http.ResponseWriter, r *http.Request) {
			writePage(t, w, r, "/repos/o/r/issues/7/comments", map[string]string{
				"":  `[{"id":3,"html_url":"https://gh/i/3","body":"i3","user":{"login":"mallory"}}]`,
				"2": `[{"id":4,"html_url":"https://gh/i/4","body":"i4","user":{"login":"pollux-agent[bot]"}}]`,
			})
		},
	})

	got, err := client.ListComments(t.Context(), 99, "o", "r", 7)
	if err != nil {
		t.Fatalf("ListComments() = %v, want nil error", err)
	}

	want := []gate.Comment{
		{ID: 1, Mine: true, Kind: gate.CommentKindReview, URL: "https://gh/r/1", Body: "r1", Path: "a.go", StartLine: 2, Line: 4},
		{ID: 2, Kind: gate.CommentKindReview, URL: "https://gh/r/2", Body: "r2"},
		{ID: 3, Kind: gate.CommentKindIssue, URL: "https://gh/i/3", Body: "i3"},
		{ID: 4, Mine: true, Kind: gate.CommentKindIssue, URL: "https://gh/i/4", Body: "i4"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListComments() (-want +got):\n%s", diff)
	}
}

func TestListCommentsResolvesBotLoginOnce(t *testing.T) {
	t.Parallel()

	var appCalls atomic.Int32
	empty := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `[]`); err != nil {
			t.Errorf("write response: %v", err)
		}
	}
	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /app": func(w http.ResponseWriter, r *http.Request) {
			appCalls.Add(1)
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
				t.Errorf("GET /app Authorization = %q, want an App JWT bearer token", got)
			}
			w.Header().Set("Content-Type", "application/json")
			if _, err := fmt.Fprint(w, `{"id":1,"slug":"pollux-agent"}`); err != nil {
				t.Errorf("write app response: %v", err)
			}
		},
		"GET /repos/o/r/pulls/7/comments":  empty,
		"GET /repos/o/r/issues/7/comments": empty,
	})

	for range 2 {
		if _, err := client.ListComments(t.Context(), 99, "o", "r", 7); err != nil {
			t.Fatalf("ListComments() = %v, want nil error", err)
		}
	}
	if got := appCalls.Load(); got != 1 {
		t.Errorf("GET /app calls = %d, want 1 across two ListComments", got)
	}
}

func TestListCommentsAppLookupError(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /app": func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) },
	})

	if _, err := client.ListComments(t.Context(), 99, "o", "r", 7); err == nil {
		t.Fatal("ListComments() = nil error, want the App lookup failure")
	}
}

func TestEditComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pattern string
		edit    func(c *ghclient.Client) error
	}{
		{
			name:    "review",
			pattern: "PATCH /repos/o/r/pulls/comments/55",
			edit: func(c *ghclient.Client) error {
				return c.EditReviewComment(t.Context(), 99, "o", "r", 55, "new")
			},
		},
		{
			name:    "issue",
			pattern: "PATCH /repos/o/r/issues/comments/77",
			edit: func(c *ghclient.Client) error {
				return c.EditIssueComment(t.Context(), 99, "o", "r", 77, "new")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got map[string]any
			client := newCommentsClient(t, map[string]http.HandlerFunc{
				tc.pattern: func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Errorf("decode request body: %v", err)
					}
					w.Header().Set("Content-Type", "application/json")
					if _, err := fmt.Fprint(w, `{"id":1,"body":"new"}`); err != nil {
						t.Errorf("write response: %v", err)
					}
				},
			})

			if err := tc.edit(client); err != nil {
				t.Fatalf("edit = %v, want nil error", err)
			}
			if diff := cmp.Diff(map[string]any{"body": "new"}, got); diff != "" {
				t.Errorf("request body (-want +got):\n%s", diff)
			}
		})
	}
}

//nolint:wrapcheck // the closures pass the client's error through so the test can inspect its wrapping.
func TestCommentsAPIError(t *testing.T) {
	t.Parallel()

	fail := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusUnprocessableEntity)
	}
	client := newCommentsClient(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/pulls/7/comments":    fail,
		"POST /repos/o/r/pulls/7/comments":   fail,
		"PATCH /repos/o/r/pulls/comments/1":  fail,
		"POST /repos/o/r/issues/7/comments":  fail,
		"PATCH /repos/o/r/issues/comments/1": fail,
	})

	tests := []struct {
		name string
		call func() error
		want string
	}{
		{"list", func() error { _, err := client.ListComments(t.Context(), 99, "o", "r", 7); return err }, "list review comments o/r#7"},
		{"create review", func() error {
			_, err := client.CreateReviewComment(t.Context(), 99, "o", "r", 7, gate.ReviewComment{Path: "a.go", Line: 4})
			return err
		}, "create review comment o/r#7 on a.go:4"},
		{"edit review", func() error { return client.EditReviewComment(t.Context(), 99, "o", "r", 1, "x") }, "edit review comment o/r 1"},
		{"create issue", func() error { _, err := client.CreateIssueComment(t.Context(), 99, "o", "r", 7, "x"); return err }, "create issue comment o/r#7"},
		{"edit issue", func() error { return client.EditIssueComment(t.Context(), 99, "o", "r", 1, "x") }, "edit issue comment o/r 1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.call()
			if err == nil {
				t.Fatalf("%s = nil error, want error", tc.name)
			}
			var apiErr *github.ErrorResponse
			if !errors.As(err, &apiErr) {
				t.Errorf("%s error = %v, want wrapped *github.ErrorResponse", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s error = %q, want it to contain %q", tc.name, err, tc.want)
			}
		})
	}
}
