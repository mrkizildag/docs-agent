package github_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
)

func TestFileAtRef(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
			t.Errorf("write access_tokens response: %v", err)
		}
	})
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

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, testPrivateKeyPEM(t), srv.URL)
	if err != nil {
		t.Fatalf("NewClient() = %v, want nil error", err)
	}

	tests := []struct {
		path    string
		want    string
		wantOK  bool
		wantErr bool
	}{
		{path: "docs/a.md", want: "# A\n", wantOK: true},
		{path: "docs/missing.md"},
		{path: "docs/big.md"},
		{path: "docs/boom.md", wantErr: true},
	}
	for _, tc := range tests {
		got, ok, err := client.FileAtRef(t.Context(), 1, "o", "r", tc.path, "abc")
		if (err != nil) != tc.wantErr || ok != tc.wantOK || string(got) != tc.want {
			t.Errorf("FileAtRef(%q) = %q, %v, %v; want %q, %v, error=%v", tc.path, got, ok, err, tc.want, tc.wantOK, tc.wantErr)
		}
	}
}
