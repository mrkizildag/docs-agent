package httpapi_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/gate/sqlite"
	ghclient "github.com/mrkizildag/docs-agent/backend/internal/github"
	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/docs-agent/backend/internal/review"
	"github.com/mrkizildag/docs-agent/backend/internal/review/actions"
)

type e2eCheckRunCall struct {
	installationID int64
	owner          string
	repo           string
	run            gate.CheckRun
}

type e2eGitHub struct {
	calls chan e2eCheckRunCall
}

func (f *e2eGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return false, nil
}

func (f *e2eGitHub) ListChangedFiles(_ context.Context, _ int64, _, _ string, _ int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *e2eGitHub) CreateCheckRun(_ context.Context, installationID int64, owner, repo string, run gate.CheckRun) (int64, error) {
	f.calls <- e2eCheckRunCall{installationID: installationID, owner: owner, repo: repo, run: run}
	return 0, nil
}

func (f *e2eGitHub) UpdateCheckRun(context.Context, int64, string, string, int64, gate.CheckRun) error {
	return nil
}

func e2ePullRequestBody(t *testing.T, number int, sha string) []byte {
	t.Helper()

	payload := map[string]any{
		"action": "opened",
		"number": number,
		"pull_request": map[string]any{
			"head": map[string]any{"sha": sha},
		},
		"repository": map[string]any{
			"name":  "widgets",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": 42},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal pull_request payload: %v", err)
	}
	return body
}

func waitCheckRun(t *testing.T, calls chan e2eCheckRunCall) e2eCheckRunCall {
	t.Helper()

	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for check run")
		return e2eCheckRunCall{}
	}
}

func TestWebhookToCheckRunEndToEnd(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	dbPath := filepath.Join(t.TempDir(), "docs-agent.db")

	store, err := sqlite.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) error = %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	gh := &e2eGitHub{calls: make(chan e2eCheckRunCall, 10)}
	gateSvc := gate.NewService(gh, store, gate.Runners{})

	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), logger, 8)

	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- worker.Run(workerCtx)
	}()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	handler := httpapi.NewHandler(logger, secret, worker, store)

	post := func(deliveryID string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", deliveryID)
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	firstBody := e2ePullRequestBody(t, 1, "sha1")
	if rec := post("d1", firstBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook first delivery = %d, want %d", rec.Code, http.StatusAccepted)
	}

	firstRun := waitCheckRun(t, gh.calls)
	if firstRun.run.HeadSHA != "sha1" {
		t.Errorf("first check run HeadSHA = %q, want %q", firstRun.run.HeadSHA, "sha1")
	}

	if rec := post("d1", firstBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook duplicate delivery = %d, want %d", rec.Code, http.StatusAccepted)
	}

	// A distinct PR acts as a barrier: it runs on a different key, in parallel with any
	// (incorrect) duplicate job, giving the worker a chance to have drained one if it existed.
	barrierBody := e2ePullRequestBody(t, 2, "sha2")
	if rec := post("d2", barrierBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook barrier delivery = %d, want %d", rec.Code, http.StatusAccepted)
	}

	barrierRun := waitCheckRun(t, gh.calls)
	if barrierRun.run.HeadSHA != "sha2" {
		t.Errorf("barrier check run HeadSHA = %q, want %q", barrierRun.run.HeadSHA, "sha2")
	}

	select {
	case extra := <-gh.calls:
		t.Errorf("unexpected extra check run: %+v", extra)
	default:
	}
}

// savedStore reports each saved PRState, so a test knows the gate has recorded
// an awaited run before the workflow_run webhook arrives.
type savedStore struct {
	*sqlite.Store
	saved chan gate.PRState
}

func (s *savedStore) SavePR(ctx context.Context, state gate.PRState) error {
	if err := s.Store.SavePR(ctx, state); err != nil {
		return fmt.Errorf("save pr: %w", err)
	}
	s.saved <- state
	return nil
}

// fakeActionsGitHub serves the GitHub API surface of a repo with the docs-agent
// workflow, recording the dispatch and check run requests.
type fakeActionsGitHub struct {
	t       *testing.T
	created chan map[string]any
	updated chan map[string]any

	mu         sync.Mutex
	dispatched map[string]any
	blobURL    string
}

func (f *fakeActionsGitHub) json(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil {
		f.t.Errorf("write response: %v", err)
	}
}

func (f *fakeActionsGitHub) decode(r *http.Request) map[string]any {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode %s %s body: %v", r.Method, r.URL.Path, err)
	}
	return body
}

func (f *fakeActionsGitHub) resultZip() []byte {
	f.mu.Lock()
	inputs, _ := f.dispatched["inputs"].(map[string]any)
	f.mu.Unlock()

	result, err := json.Marshal(map[string]any{
		"head_sha": "sha1",
		"nonce":    inputs["nonce"],
		"claude": map[string]any{
			"is_error": false,
			"structured_output": map[string]any{
				"proposals": []any{map[string]any{
					"doc_path": "docs/features/greeting.md", "section": "Greeting",
					"anchor": map[string]any{"file": "src/greet.py", "line": 3},
					"reason": "greeting changed", "content": "Hello!",
				}},
			},
		},
	})
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

func (f *fakeActionsGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)))
	})
	mux.HandleFunc("GET /repos/acme/widgets/contents/.github/workflows/docs-agent.yml", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"type":"file","name":"docs-agent.yml","path":".github/workflows/docs-agent.yml"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"default_branch":"main"}`)
	})
	mux.HandleFunc("POST /repos/acme/widgets/actions/workflows/docs-agent.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.dispatched = body
		f.mu.Unlock()
		f.json(w, http.StatusOK, `{"workflow_run_id":4242}`)
	})
	mux.HandleFunc("POST /repos/acme/widgets/check-runs", func(w http.ResponseWriter, r *http.Request) {
		f.created <- f.decode(r)
		f.json(w, http.StatusCreated, `{"id":555}`)
	})
	mux.HandleFunc("PATCH /repos/acme/widgets/check-runs/555", func(w http.ResponseWriter, r *http.Request) {
		f.updated <- f.decode(r)
		f.json(w, http.StatusOK, `{"id":555}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/runs/4242/artifacts", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"total_count":1,"artifacts":[{"id":9,"name":"docs-agent-result"}]}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/artifacts/9/zip", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		blobURL := f.blobURL
		f.mu.Unlock()
		http.Redirect(w, r, blobURL, http.StatusFound)
	})
	mux.HandleFunc("GET /repos/acme/widgets/pulls/{number}/files", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `[{"filename":"src/greet.py","status":"modified","patch":"@@ -1,3 +1,4 @@\n a\n b\n+c\n d"}]`)
	})
	mux.HandleFunc("GET /blob", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(f.resultZip()); err != nil {
			f.t.Errorf("write blob: %v", err)
		}
	})
	return mux
}

func e2eWorkflowRunBody(t *testing.T) []byte {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"action": "completed",
		"workflow_run": map[string]any{
			"id": 4242, "path": ".github/workflows/docs-agent.yml", "conclusion": "success",
		},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatalf("marshal workflow_run payload: %v", err)
	}
	return body
}

func TestActionsRunnerEndToEnd(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")

	baseStore, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "docs-agent.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := baseStore.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	store := &savedStore{Store: baseStore, saved: make(chan gate.PRState, 10)}

	github := &fakeActionsGitHub{t: t, created: make(chan map[string]any, 1), updated: make(chan map[string]any, 1)}
	srv := httptest.NewServer(github.handler())
	t.Cleanup(srv.Close)
	github.blobURL = srv.URL + "/blob"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, keyPEM, srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	gateSvc := gate.NewService(client, store, gate.Runners{Actions: actions.New(client, 10*time.Minute)})
	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(baseStore, httpapi.HandleJob(gateSvc), logger, 8)

	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	handler := httpapi.NewHandler(logger, secret, worker, baseStore)
	post := func(event, deliveryID string, body []byte) {
		t.Helper()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", deliveryID)
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST /webhook %s = %d, want %d", event, rec.Code, http.StatusAccepted)
		}
	}
	wait := func(ch chan map[string]any, what string) map[string]any {
		t.Helper()

		select {
		case body := <-ch:
			return body
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return nil
		}
	}

	post("pull_request", "d1", e2ePullRequestBody(t, 1, "sha1"))

	created := wait(github.created, "check run create")
	if created["status"] != "in_progress" || created["conclusion"] != nil || created["head_sha"] != "sha1" {
		t.Errorf("created check run = %v, want in_progress on sha1 with no conclusion", created)
	}
	select {
	case saved := <-store.saved:
		if saved.Run == nil || saved.Run.RunID != 4242 || saved.CheckRunID != 555 {
			t.Errorf("saved state = %+v, want awaiting run 4242 with check run 555", saved)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for awaited run to be saved")
	}

	github.mu.Lock()
	dispatched := github.dispatched
	github.mu.Unlock()
	inputs, _ := dispatched["inputs"].(map[string]any)
	if dispatched["ref"] != "main" || dispatched["return_run_details"] != true ||
		inputs["head_sha"] != "sha1" || inputs["pr_number"] != "1" || inputs["nonce"] == "" {
		t.Errorf("dispatch body = %v, want ref main, return_run_details, and head sha1, PR 1, a nonce", dispatched)
	}

	post("workflow_run", "d2", e2eWorkflowRunBody(t))

	updated := wait(github.updated, "check run update")
	if updated["status"] != "completed" || updated["conclusion"] != "action_required" {
		t.Errorf("updated check run = %v, want completed action_required", updated)
	}
	output, _ := updated["output"].(map[string]any)
	if summary, _ := output["summary"].(string); !strings.Contains(summary, "docs/features/greeting.md") {
		t.Errorf("updated check run output = %v, want the proposal for docs/features/greeting.md", output)
	}
}
