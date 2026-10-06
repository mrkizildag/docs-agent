package github_test

import (
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDocsAtRef(t *testing.T) {
	t.Parallel()

	var blobFetches atomic.Int32
	mux := http.NewServeMux()
	handleAccessToken(t, mux)
	mux.HandleFunc("GET /repos/o/r/git/trees/abc", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "" {
			t.Errorf("root tree request is recursive")
		}
		writeJSON(t, w, http.StatusOK, `{"truncated":true,"tree":[
			{"path":"README.md","type":"blob","mode":"100644","sha":"s6","size":4},
			{"path":"docs","type":"tree","mode":"040000","sha":"dt"}]}`)
	})
	mux.HandleFunc("GET /repos/o/r/git/trees/dt", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") == "" {
			t.Errorf("docs tree request is not recursive")
		}
		writeJSON(t, w, http.StatusOK, `{"truncated":false,"tree":[
			{"path":"a.md","type":"blob","mode":"100644","sha":"s1","size":4},
			{"path":"sub","type":"tree","mode":"040000","sha":"t1"},
			{"path":"sub/b.md","type":"blob","mode":"100644","sha":"s2","size":4},
			{"path":"c.mdx","type":"blob","mode":"100644","sha":"s7","size":4},
			{"path":"big.md","type":"blob","mode":"100644","sha":"s3","size":2000000},
			{"path":"img.png","type":"blob","mode":"100644","sha":"s4","size":4},
			{"path":"link.md","type":"blob","mode":"120000","sha":"s5","size":4}]}`)
	})
	for sha, body := range map[string]string{"s1": "# A\n", "s2": "# B\n", "s7": "# C\n"} {
		mux.HandleFunc("GET /repos/o/r/git/blobs/"+sha, func(w http.ResponseWriter, r *http.Request) {
			blobFetches.Add(1)
			if !strings.Contains(r.Header.Get("Accept"), "raw") {
				t.Errorf("blob Accept = %q, want raw", r.Header.Get("Accept"))
			}
			if _, err := fmt.Fprint(w, body); err != nil {
				t.Errorf("write blob: %v", err)
			}
		})
	}
	mux.HandleFunc("/", http.NotFound)
	client := newTestClient(t, mux)

	got, err := client.DocsAtRef(t.Context(), 1, "o", "r", "abc")
	if err != nil {
		t.Fatalf("DocsAtRef() = %v, want nil", err)
	}

	var paths []string
	if err := fs.WalkDir(got, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			paths = append(paths, p)
		}
		return err
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if fmt.Sprint(paths) != "[docs/a.md docs/c.mdx docs/sub/b.md]" {
		t.Errorf("DocsAtRef() files = %v, want [docs/a.md docs/c.mdx docs/sub/b.md]", paths)
	}
	if b, _ := fs.ReadFile(got, "docs/sub/b.md"); string(b) != "# B\n" {
		t.Errorf("docs/sub/b.md = %q, want %q", b, "# B\n")
	}

	if _, err := client.DocsAtRef(t.Context(), 1, "o", "r", "abc"); err != nil {
		t.Fatalf("second DocsAtRef() = %v, want nil", err)
	}
	if n := blobFetches.Load(); n != 3 {
		t.Errorf("blob fetches after two calls = %d, want 3 (second call served from cache)", n)
	}
}

func TestDocsAtRefTruncatedRootWithoutDocs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		truncated bool
		wantErr   bool
	}{
		{name: "truncated root without docs", truncated: true, wantErr: true},
		{name: "complete root without docs", truncated: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mux := http.NewServeMux()
			handleAccessToken(t, mux)
			mux.HandleFunc("GET /repos/o/r/git/trees/abc", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusOK, fmt.Sprintf(`{"truncated":%t,"tree":[{"path":"README.md","type":"blob","mode":"100644","sha":"s6","size":4}]}`, tc.truncated))
			})
			mux.HandleFunc("/", http.NotFound)
			client := newTestClient(t, mux)

			_, err := client.DocsAtRef(t.Context(), 1, "o", "r", "abc")
			if (err != nil) != tc.wantErr {
				t.Errorf("DocsAtRef() error = %v, want error: %t", err, tc.wantErr)
			}
		})
	}
}
