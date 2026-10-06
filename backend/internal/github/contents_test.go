package github_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestFileAtRef(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/r/contents/docs/a.md", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ref"); got != "abc" {
			t.Errorf("ref = %q, want abc", got)
		}
		w.Header().Set("Content-Type", "application/json")
		enc := base64.StdEncoding.EncodeToString([]byte("# A\n"))
		if _, err := fmt.Fprintf(w, `{"type":"file","encoding":"base64","size":4,"content":%q}`, enc); err != nil {
			t.Errorf("write contents response: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/o/r/contents/docs/big.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"type":"file","encoding":"none","size":2000000,"content":""}`); err != nil {
			t.Errorf("write contents response: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/o/r/contents/docs/boom.md", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	mux.HandleFunc("/", http.NotFound)

	client := newTestClient(t, mux)

	tests := []struct {
		path    string
		want    string
		wantOK  bool
		wantErr error
		anyErr  bool
	}{
		{path: "docs/a.md", want: "# A\n", wantOK: true},
		{path: "docs/missing.md"},
		{path: "docs/big.md", wantErr: review.ErrFileTooLarge},
		{path: "docs/boom.md", anyErr: true},
	}
	for _, tc := range tests {
		got, ok, err := client.FileAtRef(t.Context(), 1, "o", "r", tc.path, "abc")
		switch {
		case tc.wantErr == nil && !tc.anyErr && err != nil, tc.anyErr && err == nil,
			tc.wantErr != nil && !errors.Is(err, tc.wantErr),
			ok != tc.wantOK, string(got) != tc.want:
			t.Errorf("FileAtRef(%q) = %q, %v, %v; want %q, %v, error %v", tc.path, got, ok, err, tc.want, tc.wantOK, tc.wantErr)
		}
	}
}
