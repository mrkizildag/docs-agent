package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
)

// blockingGitHub blocks CreateCheckRun for blockSHA until its context is cancelled.
type blockingGitHub struct {
	blockSHA  string
	started   chan string
	cancelled chan string
	created   chan string
}

func (f *blockingGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return false, nil
}

func (f *blockingGitHub) CreateCheckRun(ctx context.Context, _ int64, _, _ string, run gate.CheckRun) error {
	f.started <- run.HeadSHA
	if run.HeadSHA == f.blockSHA {
		<-ctx.Done()
		f.cancelled <- run.HeadSHA
		return fmt.Errorf("create check run: %w", context.Cause(ctx))
	}
	f.created <- run.HeadSHA
	return nil
}

func newBlockingGitHub(blockSHA string) *blockingGitHub {
	return &blockingGitHub{
		blockSHA:  blockSHA,
		started:   make(chan string, 10),
		cancelled: make(chan string, 10),
		created:   make(chan string, 10),
	}
}

func prBody(t *testing.T, action string, number int, sha string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"action":       action,
		"number":       number,
		"pull_request": map[string]any{"head": map[string]any{"sha": sha}},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

func postSigned(t *testing.T, h http.Handler, secret []byte, deliveryID string, body []byte) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func waitString(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		return ""
	}
}

func openStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) = %v", path, err)
	}
	return store
}

func runWorker(w *jobqueue.Worker) (stop func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return func() error { cancel(); return <-done }
}

// failThenSucceedGitHub fails CreateCheckRun once per unique SHA, then succeeds on later calls.
type failThenSucceedGitHub struct {
	calls chan string

	mu     sync.Mutex
	failed map[string]bool
}

func newFailThenSucceedGitHub() *failThenSucceedGitHub {
	return &failThenSucceedGitHub{calls: make(chan string, 10), failed: make(map[string]bool)}
}

func (f *failThenSucceedGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return false, nil
}

func (f *failThenSucceedGitHub) CreateCheckRun(_ context.Context, _ int64, _, _ string, run gate.CheckRun) error {
	f.calls <- run.HeadSHA

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failed[run.HeadSHA] {
		f.failed[run.HeadSHA] = true
		return fmt.Errorf("create check run: boom")
	}
	return nil
}

func TestWebhookRedeliveryAfterFailedJobEnqueuesNewJob(t *testing.T) {
	t.Parallel()

	secret := []byte("s")
	store := openStore(t, filepath.Join(t.TempDir(), "db"))
	t.Cleanup(func() { _ = store.Close() })

	gh := newFailThenSucceedGitHub()
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gate.NewService(gh, store, gate.Runners{})), slog.New(slog.DiscardHandler), 8)
	stop := runWorker(worker)
	t.Cleanup(func() { _ = stop() })
	h := httpapi.NewHandler(slog.New(slog.DiscardHandler), secret, worker)

	body := prBody(t, "opened", 1, "sha1")
	if code := postSigned(t, h, secret, "d1", body); code != http.StatusAccepted {
		t.Fatalf("first delivery = %d, want 202", code)
	}
	if got := waitString(t, gh.calls); got != "sha1" {
		t.Fatalf("first call sha = %q, want sha1", got)
	}

	// The worker marks the job failed asynchronously after CreateCheckRun returns; redelivering
	// before that Finish lands correctly finds the job still running and stays a no-op, so retry
	// the same delivery with a short backoff until the job has failed and the redelivery lands.
	var redelivered bool
	for range 20 {
		if code := postSigned(t, h, secret, "d1", body); code != http.StatusAccepted {
			t.Fatalf("redelivery = %d, want 202", code)
		}
		select {
		case got := <-gh.calls:
			if got != "sha1" {
				t.Fatalf("redelivery call sha = %q, want sha1", got)
			}
			redelivered = true
		case <-time.After(50 * time.Millisecond):
		}
		if redelivered {
			break
		}
	}
	if !redelivered {
		t.Fatal("redelivery of a failed delivery never produced a second CreateCheckRun call")
	}
}

func TestWebhookSecondSynchronizeCancelsFirst(t *testing.T) {
	t.Parallel()

	secret := []byte("s")
	store := openStore(t, filepath.Join(t.TempDir(), "db"))
	t.Cleanup(func() { _ = store.Close() })

	gh := newBlockingGitHub("sha1")
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gate.NewService(gh, store, gate.Runners{})), slog.New(slog.DiscardHandler), 8)
	stop := runWorker(worker)
	t.Cleanup(func() { _ = stop() })
	h := httpapi.NewHandler(slog.New(slog.DiscardHandler), secret, worker)

	if code := postSigned(t, h, secret, "d1", prBody(t, "synchronize", 1, "sha1")); code != http.StatusAccepted {
		t.Fatalf("first synchronize = %d, want 202", code)
	}
	if got := waitString(t, gh.started); got != "sha1" {
		t.Fatalf("started %q, want sha1", got)
	}

	if code := postSigned(t, h, secret, "d2", prBody(t, "synchronize", 1, "sha2")); code != http.StatusAccepted {
		t.Fatalf("second synchronize = %d, want 202", code)
	}
	if got := waitString(t, gh.cancelled); got != "sha1" {
		t.Fatalf("cancelled %q, want sha1", got)
	}
	if got := waitString(t, gh.created); got != "sha2" {
		t.Fatalf("created %q, want sha2", got)
	}
}

func TestWebhookPendingJobRunsAfterRestartAndDuplicateStaysNoOp(t *testing.T) {
	t.Parallel()

	secret := []byte("s")
	path := filepath.Join(t.TempDir(), "db")
	logger := slog.New(slog.DiscardHandler)

	// Process 1 accepts the webhook but dies before any worker runs it.
	store1 := openStore(t, path)
	gh1 := newBlockingGitHub("")
	worker1 := jobqueue.NewWorker(store1, httpapi.HandleJob(gate.NewService(gh1, store1, gate.Runners{})), logger, 8)
	h1 := httpapi.NewHandler(logger, secret, worker1)
	if code := postSigned(t, h1, secret, "d1", prBody(t, "opened", 1, "sha1")); code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202", code)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	// Process 2 runs the unfinished job.
	store2 := openStore(t, path)
	t.Cleanup(func() { _ = store2.Close() })
	gh2 := newBlockingGitHub("")
	worker2 := jobqueue.NewWorker(store2, httpapi.HandleJob(gate.NewService(gh2, store2, gate.Runners{})), logger, 8)
	stop := runWorker(worker2)
	t.Cleanup(func() { _ = stop() })
	if got := waitString(t, gh2.created); got != "sha1" {
		t.Fatalf("created %q after restart, want sha1", got)
	}

	// The same delivery redelivered after restart is still a no-op.
	h2 := httpapi.NewHandler(logger, secret, worker2)
	if code := postSigned(t, h2, secret, "d1", prBody(t, "opened", 1, "sha1")); code != http.StatusAccepted {
		t.Fatalf("duplicate POST = %d, want 202", code)
	}
	if code := postSigned(t, h2, secret, "d2", prBody(t, "opened", 2, "sha2")); code != http.StatusAccepted {
		t.Fatalf("barrier POST = %d, want 202", code)
	}
	if got := waitString(t, gh2.created); got != "sha2" {
		t.Fatalf("created %q, want barrier sha2", got)
	}
	select {
	case extra := <-gh2.created:
		t.Errorf("duplicate delivery produced a check run for %q", extra)
	case <-time.After(300 * time.Millisecond):
	}
}
