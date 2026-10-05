package github_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func TestReact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    gate.CommentKind
		pattern string
		wantID  int64
	}{
		{"review", gate.CommentKindReview, "POST /repos/o/r/pulls/comments/12/reactions", 501},
		{"issue", gate.CommentKindIssue, "POST /repos/o/r/issues/comments/12/reactions", 502},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got map[string]any
			client := newCommentsClient(t, map[string]http.HandlerFunc{
				tc.pattern: func(w http.ResponseWriter, r *http.Request) {
					got = decodeBody(t, r)
					writeJSON(t, w, http.StatusOK, fmt.Sprintf(`{"id":%d,"content":"eyes"}`, tc.wantID))
				},
			})

			id, err := client.React(t.Context(), 1, "o", "r", tc.kind, 12, gate.ReactionSeen)
			if err != nil || id != tc.wantID {
				t.Fatalf("React() = %d, %v, want %d, nil", id, err, tc.wantID)
			}
			if diff := cmp.Diff(map[string]any{"content": "eyes"}, got); diff != "" {
				t.Errorf("request (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReactUnknownKind(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, nil)
	if _, err := client.React(t.Context(), 1, "o", "r", gate.CommentKind("bogus"), 12, gate.ReactionSeen); err == nil {
		t.Error("React() error = nil, want an error")
	}
}

func TestUnreact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    gate.CommentKind
		pattern string
		status  int
		wantErr bool
	}{
		{"review", gate.CommentKindReview, "DELETE /repos/o/r/pulls/comments/12/reactions/501", http.StatusNoContent, false},
		{"issue", gate.CommentKindIssue, "DELETE /repos/o/r/issues/comments/12/reactions/501", http.StatusNoContent, false},
		{"review already gone", gate.CommentKindReview, "DELETE /repos/o/r/pulls/comments/12/reactions/501", http.StatusNotFound, false},
		{"issue already gone", gate.CommentKindIssue, "DELETE /repos/o/r/issues/comments/12/reactions/501", http.StatusNotFound, false},
		{"review server error", gate.CommentKindReview, "DELETE /repos/o/r/pulls/comments/12/reactions/501", http.StatusInternalServerError, true},
		{"issue server error", gate.CommentKindIssue, "DELETE /repos/o/r/issues/comments/12/reactions/501", http.StatusInternalServerError, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			client := newCommentsClient(t, map[string]http.HandlerFunc{
				tc.pattern: func(w http.ResponseWriter, _ *http.Request) {
					if tc.status == http.StatusNoContent {
						w.WriteHeader(tc.status)
						return
					}
					writeJSON(t, w, tc.status, `{"message":"x"}`)
				},
			})

			err := client.Unreact(t.Context(), 1, "o", "r", tc.kind, 12, 501)
			if (err != nil) != tc.wantErr {
				t.Errorf("Unreact() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestUnreactUnknownKind(t *testing.T) {
	t.Parallel()

	client := newCommentsClient(t, nil)
	if err := client.Unreact(t.Context(), 1, "o", "r", gate.CommentKind("bogus"), 12, 501); err == nil {
		t.Error("Unreact() error = nil, want an error")
	}
}
