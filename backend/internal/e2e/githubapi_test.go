package e2e_test

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
)

const baseGreetingDoc = "---\ntitle: Greeting\nsummary: Greets users.\ncovers:\n  - \"src/**\"\n---\n# Greetings\n\n## Greeting\n\nHi.\n"

// apiConfig is the repo acme/widgets the fake GitHub API serves: it has the
// pollux-agent workflow, and the workflow's artifact is whatever result builds
// from the dispatch inputs.
type apiConfig struct {
	// docs says whether the repo has a docs/ folder, with greeting.md in it.
	docs bool
	// nextCheckRun is the ID of the first check run created.
	nextCheckRun int64
	// branches are the branch tips by name.
	branches map[string]string
	// result is the workflow's result.json given the dispatch inputs.
	result func(inputs map[string]any) any
}

// githubAPI is a fake of the GitHub REST API the Actions runner and the
// scaffold flow use, behind a real github.Client over HTTP. It records the
// dispatches, check runs, branches, commits and pull requests made against it.
type githubAPI struct {
	t      *testing.T
	cfg    apiConfig
	client *ghclient.Client
	blob   string

	mu            sync.Mutex
	runs          int64
	nextCheckRun  int64
	inputs        map[string]any
	blobs         map[string]string
	branches      map[string]string
	branchCreates int
	dispatched    []map[string]any
	created       []map[string]any
	updated       []map[string]any
	commits       []map[string]string
	pulls         []map[string]any
}

func newGitHubAPI(t *testing.T, cfg apiConfig) *githubAPI {
	t.Helper()

	api := &githubAPI{t: t, cfg: cfg, nextCheckRun: cfg.nextCheckRun, blobs: map[string]string{}, branches: map[string]string{}}
	for name, sha := range cfg.branches {
		api.branches[name] = sha
	}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	api.blob = srv.URL + "/blob"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	api.client, err = ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, keyPEM, srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return api
}

func (f *githubAPI) dispatches() []map[string]any { return snapshot(f, &f.dispatched) }

func (f *githubAPI) checkRunsCreated() []map[string]any { return snapshot(f, &f.created) }

func (f *githubAPI) checkRunsUpdated() []map[string]any { return snapshot(f, &f.updated) }

func (f *githubAPI) pullRequests() []map[string]any { return snapshot(f, &f.pulls) }

func (f *githubAPI) commitFiles() []map[string]string { return snapshot(f, &f.commits) }

func (f *githubAPI) branchCreations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.branchCreates
}

func snapshot[T any](f *githubAPI, list *[]T) []T {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(*list)
}

func (f *githubAPI) json(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil { //nolint:gosec // a test fake writing fixture JSON, not request input
		f.t.Errorf("write response: %v", err)
	}
}

func (f *githubAPI) decode(r *http.Request) map[string]any {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode %s %s body: %v", r.Method, r.URL.Path, err)
	}
	return body
}

func (f *githubAPI) resultZip() []byte {
	f.mu.Lock()
	inputs := f.inputs
	f.mu.Unlock()

	result, err := json.Marshal(f.cfg.result(inputs))
	if err != nil {
		f.t.Errorf("marshal result: %v", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	file, err := zw.Create("result.json")
	if err != nil {
		f.t.Errorf("create zip entry: %v", err)
	}
	if _, err := file.Write(result); err != nil {
		f.t.Errorf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		f.t.Errorf("close zip: %v", err)
	}
	return buf.Bytes()
}

func (f *githubAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)))
	})
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"id":1,"slug":"pollux-agent"}`)
	})
	f.repoRoutes(mux)
	f.workflowRoutes(mux)
	f.checkRunRoutes(mux)
	f.pullRequestRoutes(mux)
	f.gitRoutes(mux)
	return mux
}

func (f *githubAPI) repoRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"default_branch":"main"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/contents/.github/workflows/pollux-agent.yml", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"type":"file","name":"pollux-agent.yml","path":".github/workflows/pollux-agent.yml"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/contents/docs", func(w http.ResponseWriter, _ *http.Request) {
		if !f.cfg.docs {
			f.json(w, http.StatusNotFound, `{"message":"Not Found"}`)
			return
		}
		f.json(w, http.StatusOK, `[{"type":"file","name":"README.md","path":"docs/README.md"}]`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/contents/docs/features/greeting.md", func(w http.ResponseWriter, _ *http.Request) {
		content := base64.StdEncoding.EncodeToString([]byte(baseGreetingDoc))
		f.json(w, http.StatusOK, fmt.Sprintf(`{"type":"file","encoding":"base64","size":%d,"path":"docs/features/greeting.md","content":%q}`, len(baseGreetingDoc), content))
	})
}

func (f *githubAPI) workflowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /repos/acme/widgets/actions/workflows/pollux-agent.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.runs++
		id := 4241 + f.runs
		f.inputs, _ = body["inputs"].(map[string]any)
		f.dispatched = append(f.dispatched, body)
		f.mu.Unlock()
		f.json(w, http.StatusOK, fmt.Sprintf(`{"workflow_run_id":%d}`, id))
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/runs/{run}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, http.StatusOK, fmt.Sprintf(`{"total_count":1,"artifacts":[{"id":9,"name":"pollux-agent-result","workflow_run":{"id":%s}}]}`, r.PathValue("run")))
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/artifacts/9/zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.blob, http.StatusFound)
	})
	mux.HandleFunc("GET /blob", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(f.resultZip()); err != nil {
			f.t.Errorf("write blob: %v", err)
		}
	})
}

func (f *githubAPI) checkRunRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /repos/acme/widgets/check-runs", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		id := f.nextCheckRun
		f.nextCheckRun++
		f.created = append(f.created, body)
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"id":%d}`, id))
	})
	mux.HandleFunc("PATCH /repos/acme/widgets/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		body["id"] = r.PathValue("id")
		f.mu.Lock()
		f.updated = append(f.updated, body)
		f.mu.Unlock()
		f.json(w, http.StatusOK, `{"id":1}`)
	})
}

func (f *githubAPI) pullRequestRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/acme/widgets/pulls/{number}/files", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `[{"filename":"src/greet.py","status":"modified","patch":"@@ -1,3 +1,4 @@\n a\n b\n+c\n d"}]`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/compare/{basehead}", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"merge_base_commit":{"sha":"base1"}}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `[]`)
	})
	mux.HandleFunc("POST /repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.pulls = append(f.pulls, body)
		f.mu.Unlock()
		f.json(w, http.StatusCreated, `{"number":9,"html_url":"https://github.com/acme/widgets/pull/9"}`)
	})
}

func (f *githubAPI) gitRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /repos/acme/widgets/git/trees/base1", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"sha":"base1","truncated":false,"tree":[{"path":"docs","mode":"040000","type":"tree","sha":"docs1"}]}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/git/trees/docs1", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"sha":"docs1","truncated":false,"tree":[{"path":"features/greeting.md","mode":"100644","type":"blob","sha":"doc1","size":80}]}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/git/blobs/doc1", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(baseGreetingDoc)); err != nil {
			f.t.Errorf("write base doc blob: %v", err)
		}
	})
	mux.HandleFunc("GET /repos/acme/widgets/branches/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("branch")
		f.mu.Lock()
		sha, ok := f.branches[name]
		f.mu.Unlock()
		if !ok {
			f.json(w, http.StatusNotFound, `{"message":"Branch not found"}`)
			return
		}
		f.json(w, http.StatusOK, fmt.Sprintf(`{"name":%q,"commit":{"sha":%q}}`, name, sha))
	})
	mux.HandleFunc("POST /repos/acme/widgets/git/refs", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		name := strings.TrimPrefix(fmt.Sprint(body["ref"]), "refs/heads/")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.branchCreates++
		if _, ok := f.branches[name]; ok {
			f.json(w, http.StatusUnprocessableEntity, `{"message":"Reference already exists"}`)
			return
		}
		f.branches[name] = fmt.Sprint(body["sha"])
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"ref":%q,"object":{"sha":%q}}`, body["ref"], body["sha"]))
	})
	mux.HandleFunc("PATCH /repos/acme/widgets/git/refs/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.branches[r.PathValue("branch")] = fmt.Sprint(body["sha"])
		f.mu.Unlock()
		f.json(w, http.StatusOK, `{"ref":"ok"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, http.StatusOK, fmt.Sprintf(`{"sha":%q,"tree":{"sha":"tree-of-%s"}}`, r.PathValue("sha"), r.PathValue("sha")))
	})
	mux.HandleFunc("GET /repos/acme/widgets/git/trees/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, http.StatusOK, fmt.Sprintf(`{"sha":%q,"tree":[{"path":"README.md","type":"blob","mode":"100644","sha":"readme"}]}`, r.PathValue("sha")))
	})
	mux.HandleFunc("POST /repos/acme/widgets/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		sha := fmt.Sprintf("blob-%d", len(f.blobs)+1)
		f.blobs[sha] = fmt.Sprint(body["content"])
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"sha":%q}`, sha))
	})
	mux.HandleFunc("POST /repos/acme/widgets/git/trees", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		files := map[string]string{}
		entries, _ := body["tree"].([]any)
		f.mu.Lock()
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			files[fmt.Sprint(entry["path"])] = f.blobs[fmt.Sprint(entry["sha"])]
		}
		f.commits = append(f.commits, files)
		n := len(f.commits)
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"sha":"tree-new-%d"}`, n))
	})
	mux.HandleFunc("POST /repos/acme/widgets/git/commits", func(w http.ResponseWriter, r *http.Request) {
		f.decode(r)
		f.mu.Lock()
		n := len(f.commits)
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"sha":"commit-%d"}`, n))
	})
}
