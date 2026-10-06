package github_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func newFilesClient(t *testing.T, files http.HandlerFunc) *ghclient.Client {
	t.Helper()

	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/r/pulls/7/files", files)
	mux.HandleFunc("GET /repos/o/r/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := fmt.Fprint(w, `{"number":7,"state":"open","base":{"sha":"base1","repo":{"full_name":"o/r"}},"head":{"sha":"head1","ref":"feature","repo":{"full_name":"fork/r"}}}`); err != nil {
			t.Errorf("write pull request response: %v", err)
		}
	})

	client := newTestClient(t, mux)
	return client
}

func TestGetPullRequest(t *testing.T) {
	t.Parallel()

	client := newFilesClient(t, func(http.ResponseWriter, *http.Request) {})

	got, err := client.GetPullRequest(t.Context(), 99, "o", "r", 7)
	if err != nil {
		t.Fatalf("GetPullRequest() = %v, want nil error", err)
	}

	want := gate.PullRequest{InstallationID: 99, Owner: "o", Repo: "r", Number: 7, BaseSHA: "base1", HeadSHA: "head1", HeadRef: "feature", Fork: true, Open: true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("GetPullRequest() (-want +got):\n%s", diff)
	}
}

func TestListChangedFiles(t *testing.T) {
	t.Parallel()

	const multiPatch = "@@ -1,3 +1,4 @@ func a\n x\n+y\n@@ -10 +11 @@\n-a\n+b\n@@ -20,2 +22,0 @@\n-gone\n-gone2"

	pages := map[string]string{
		"": fmt.Sprintf(`[
			{"filename":"a.go","status":"modified","changes":5,"patch":%q},
			{"filename":"new/name.go","previous_filename":"old/name.go","status":"renamed","patch":"@@ -5,2 +5,3 @@\n x"}
		]`, multiPatch),
		"2": `[
			{"filename":"removed.go","status":"removed","patch":"@@ -1,2 +0,0 @@\n-a\n-b"},
			{"filename":"big.bin","status":"modified","changes":12000},
			{"filename":"copy.go","previous_filename":"orig.go","status":"copied"}
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
			Path:    "a.go",
			Hunks:   []review.LineRange{{Start: 1, End: 4}, {Start: 11, End: 11}},
			Patch:   multiPatch,
			Changes: 5,
		},
		{Path: "new/name.go", PreviousPath: "old/name.go", Hunks: []review.LineRange{{Start: 5, End: 7}}, Patch: "@@ -5,2 +5,3 @@\n x"},
		{Path: "removed.go", Removed: true, Patch: "@@ -1,2 +0,0 @@\n-a\n-b"},
		{Path: "big.bin", Changes: 12000},
		{Path: "copy.go"},
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
