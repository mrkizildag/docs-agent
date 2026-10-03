package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
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

func (f *e2eGitHub) CreateCheckRun(_ context.Context, installationID int64, owner, repo string, run gate.CheckRun) error {
	f.calls <- e2eCheckRunCall{installationID: installationID, owner: owner, repo: repo, run: run}
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
	gateSvc := gate.NewService(gh, store)

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

	handler := httpapi.NewHandler(logger, secret, worker)

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
