package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// blockingGitHub blocks CreateCheckRun for blockSHA until its context is cancelled.
type blockingGitHub struct {
	noComments
	blockSHA  string
	started   chan string
	cancelled chan string
	created   chan string
}

func (f *blockingGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return false, nil
}

func (f *blockingGitHub) MergeBase(_ context.Context, _ int64, _, _, base, _ string) (string, error) {
	return base, nil
}

func (f *blockingGitHub) DocsExist(context.Context, int64, string, string, string) (bool, error) {
	return true, nil
}

func (f *blockingGitHub) ListChangedFiles(_ context.Context, _ int64, _, _ string, _ int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *blockingGitHub) CreateCheckRun(ctx context.Context, _ int64, _, _ string, run gate.CheckRun) (int64, error) {
	f.started <- run.HeadSHA
	if run.HeadSHA == f.blockSHA {
		<-ctx.Done()
		f.cancelled <- run.HeadSHA
		return 0, fmt.Errorf("create check run: %w", context.Cause(ctx))
	}
	f.created <- run.HeadSHA
	return 1, nil
}

func (f *blockingGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, _ int64, _ gate.CheckRun) error {
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
	noComments
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

func (f *failThenSucceedGitHub) MergeBase(_ context.Context, _ int64, _, _, base, _ string) (string, error) {
	return base, nil
}

func (f *failThenSucceedGitHub) DocsExist(context.Context, int64, string, string, string) (bool, error) {
	return true, nil
}

func (f *failThenSucceedGitHub) ListChangedFiles(_ context.Context, _ int64, _, _ string, _ int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *failThenSucceedGitHub) CreateCheckRun(_ context.Context, _ int64, _, _ string, run gate.CheckRun) (int64, error) {
	f.calls <- run.HeadSHA

	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.failed[run.HeadSHA] {
		f.failed[run.HeadSHA] = true
		return 0, fmt.Errorf("create check run: boom")
	}
	return 1, nil
}

func (f *failThenSucceedGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, _ int64, _ gate.CheckRun) error {
	return nil
}

func TestWebhookRedeliveryAfterFailedJobEnqueuesNewJob(t *testing.T) {
	t.Parallel()

	secret := []byte("s")
	store := openStore(t, filepath.Join(t.TempDir(), "db"))
	t.Cleanup(func() { _ = store.Close() })

	gh := newFailThenSucceedGitHub()
	worker := jobqueue.NewWorker(store, jobs.HandleJob(newGate(gh, store, gate.Runners{})), slog.New(slog.DiscardHandler), 8)
	stop := runWorker(worker)
	t.Cleanup(func() { _ = stop() })
	h := httpapi.NewHandler(slog.New(slog.DiscardHandler), secret, worker, store)

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
	worker := jobqueue.NewWorker(store, jobs.HandleJob(newGate(gh, store, gate.Runners{})), slog.New(slog.DiscardHandler), 8)
	stop := runWorker(worker)
	t.Cleanup(func() { _ = stop() })
	h := httpapi.NewHandler(slog.New(slog.DiscardHandler), secret, worker, store)

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
	worker1 := jobqueue.NewWorker(store1, jobs.HandleJob(newGate(gh1, store1, gate.Runners{})), logger, 8)
	h1 := httpapi.NewHandler(logger, secret, worker1, store1)
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
	worker2 := jobqueue.NewWorker(store2, jobs.HandleJob(newGate(gh2, store2, gate.Runners{})), logger, 8)
	stop := runWorker(worker2)
	t.Cleanup(func() { _ = stop() })
	if got := waitString(t, gh2.created); got != "sha1" {
		t.Fatalf("created %q after restart, want sha1", got)
	}

	// The same delivery redelivered after restart is still a no-op.
	h2 := httpapi.NewHandler(logger, secret, worker2, store2)
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

type countingEnqueuer struct {
	next     jobs.Enqueuer
	enqueued int
}

func (c *countingEnqueuer) Enqueue(ctx context.Context, job jobqueue.NewJob) (bool, error) {
	ok, err := c.next.Enqueue(ctx, job)
	if ok {
		c.enqueued++
	}
	if err != nil {
		return ok, fmt.Errorf("enqueue %s: %w", job.DeliveryID, err)
	}
	return ok, nil
}

func TestEnqueueDeadlineJobsIsIdempotent(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "db"))
	t.Cleanup(func() { _ = store.Close() })
	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1", CheckRunID: 5,
		Run: &gate.AwaitingRun{RunID: 9, Nonce: "n1", Deadline: deadline},
	}
	if err := store.SavePR(t.Context(), state); err != nil {
		t.Fatalf("SavePR() = %v", err)
	}

	worker := jobqueue.NewWorker(store, jobs.HandleJob(newGate(&blockingGitHub{}, store, gate.Runners{})), slog.New(slog.DiscardHandler), 1)
	counter := &countingEnqueuer{next: worker}

	for _, now := range []time.Time{deadline.Add(-time.Second), deadline.Add(time.Second), deadline.Add(2 * time.Second)} {
		if err := jobs.EnqueueDeadlineJobs(t.Context(), store, counter, slog.New(slog.DiscardHandler), now); err != nil {
			t.Fatalf("EnqueueDeadlineJobs(%v) = %v", now, err)
		}
	}
	if counter.enqueued != 1 {
		t.Errorf("deadline jobs enqueued = %d, want 1 across a not-yet-due sweep and two overdue sweeps in one minute", counter.enqueued)
	}
}

// failOnceConcludeGitHub rejects the first UpdateCheckRun and records the rest.
type failOnceConcludeGitHub struct {
	blockingGitHub

	mu        sync.Mutex
	attempts  int
	concluded chan gate.CheckRun
}

func (f *failOnceConcludeGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, _ int64, run gate.CheckRun) error {
	f.mu.Lock()
	f.attempts++
	first := f.attempts == 1
	f.mu.Unlock()
	if first {
		return errors.New("github rejected the conclude write")
	}
	f.concluded <- run
	return nil
}

func TestDeadlineJobFailedConcludeIsRetriedByLaterSweep(t *testing.T) {
	t.Parallel()

	store := openStore(t, filepath.Join(t.TempDir(), "db"))
	t.Cleanup(func() { _ = store.Close() })
	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1", CheckRunID: 5,
		Run: &gate.AwaitingRun{RunID: 9, Nonce: "n1", Deadline: deadline},
	}
	if err := store.SavePR(t.Context(), state); err != nil {
		t.Fatalf("SavePR() = %v", err)
	}

	gh := &failOnceConcludeGitHub{concluded: make(chan gate.CheckRun, 1)}
	worker := jobqueue.NewWorker(store, jobs.HandleJob(newGate(gh, store, gate.Runners{})), slog.New(slog.DiscardHandler), 1)
	stop := runWorker(worker)
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("worker.Run() = %v", err)
		}
	})

	now := deadline.Add(time.Second)
	if err := jobs.EnqueueDeadlineJobs(t.Context(), store, worker, slog.New(slog.DiscardHandler), now); err != nil {
		t.Fatalf("EnqueueDeadlineJobs() = %v", err)
	}
	waitFor(t, "the first conclude attempt", func() bool {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		return gh.attempts == 1
	})

	for i := 1; ; i++ {
		select {
		case run := <-gh.concluded:
			if run.Conclusion != gate.ConclusionNeutral {
				t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionNeutral)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
		if i > 100 {
			t.Fatal("timed out waiting for the retried conclude write")
		}
		if err := jobs.EnqueueDeadlineJobs(t.Context(), store, worker, slog.New(slog.DiscardHandler), now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("EnqueueDeadlineJobs() = %v", err)
		}
	}
}

// newServiceWorker builds the gate service newService returns and the worker
// that runs its jobs; the service enqueues scaffold jobs through the worker.
func newServiceWorker(store jobqueue.Store, newService func(gate.ScaffoldQueue) *gate.Service, logger *slog.Logger, maxParallel int) (*gate.Service, *jobqueue.Worker) {
	var svc *gate.Service
	worker := jobqueue.NewWorker(store, func(ctx context.Context, job jobqueue.Job) error {
		return jobs.HandleJob(svc)(ctx, job)
	}, logger, maxParallel)
	svc = newService(jobs.NewScaffoldQueue(worker))
	return svc, worker
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
