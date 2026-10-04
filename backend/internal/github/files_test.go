package github_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	ghclient "github.com/mrkizildag/docs-agent/backend/internal/github"
	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

func newFilesClient(t *testing.T, files http.HandlerFunc) *ghclient.Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprintf(w, `{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)); err != nil {
			t.Errorf("write access_tokens response: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/o/r/pulls/7/files", files)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, testPrivateKeyPEM(t), srv.URL)
	if err != nil {
		t.Fatalf("NewClient() = %v, want nil error", err)
	}
	return client
}

func TestListChangedFiles(t *testing.T) {
	t.Parallel()

	const multiPatch = "@@ -1,3 +1,4 @@ func a\n x\n+y\n@@ -10 +11 @@\n-a\n+b\n@@ -20,2 +22,0 @@\n-gone\n-gone2"

	pages := map[string]string{
		"": fmt.Sprintf(`[
			{"filename":"a.go","status":"modified","patch":%q},
			{"filename":"new/name.go","previous_filename":"old/name.go","status":"renamed","patch":"@@ -5,2 +5,3 @@\n x"}
		]`, multiPatch),
		"2": `[
			{"filename":"removed.go","status":"removed","patch":"@@ -1,2 +0,0 @@\n-a\n-b"},
			{"filename":"big.bin","status":"modified"}
		]`,
	}

	var pageRequests []string
	client := newFilesClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pageRequests = append(pageRequests, page+"/"+r.URL.Query().Get("per_page"))
		if page == "" {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/o/r/pulls/7/files?page=2>; rel="next"`, r.Host))
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, pages[page]); err != nil {
			t.Errorf("write files response: %v", err)
		}
	})

	got, err := client.ListChangedFiles(t.Context(), 99, "o", "r", 7)
	if err != nil {
		t.Fatalf("ListChangedFiles() = %v, want nil error", err)
	}

	want := []review.ChangedFile{
		{
			Path:  "a.go",
			Hunks: []review.LineRange{{Start: 1, End: 4}, {Start: 11, End: 11}},
			Patch: multiPatch,
		},
		{Path: "new/name.go", PreviousPath: "old/name.go", Hunks: []review.LineRange{{Start: 5, End: 7}}, Patch: "@@ -5,2 +5,3 @@\n x"},
		{Path: "removed.go", Patch: "@@ -1,2 +0,0 @@\n-a\n-b"},
		{Path: "big.bin"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ListChangedFiles() (-want +got):\n%s", diff)
	}

	if diff := cmp.Diff([]string{"/100", "2/100"}, pageRequests); diff != "" {
		t.Errorf("page requests (-want +got):\n%s", diff)
	}
}

func TestListChangedFilesMalformedHunk(t *testing.T) {
	t.Parallel()

	client := newFilesClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `[{"filename":"bad.go","status":"modified","patch":"@@ -1,2 oops @@\n x"}]`); err != nil {
			t.Errorf("write files response: %v", err)
		}
	})

	_, err := client.ListChangedFiles(t.Context(), 99, "o", "r", 7)
	if err == nil {
		t.Fatalf("ListChangedFiles() = nil error, want error for malformed hunk header")
	}
	if got := err.Error(); !strings.Contains(got, "bad.go") {
		t.Errorf("ListChangedFiles() error = %q, want it to name bad.go", got)
	}
}
