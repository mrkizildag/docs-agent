package jobqueue_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
)

const testTimeout = 5 * time.Second

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()

	return sqlitetest.Open(t)
}

// finishingStore fails every job that Enqueue reports as superseded and running, as if it
// ended between the store listing it and the worker finishing it.
type finishingStore struct {
	*sqlite.Store
}

func (s finishingStore) Enqueue(ctx context.Context, job jobqueue.NewJob) (bool, []int64, error) {
	enqueued, ids, err := s.Store.Enqueue(ctx, job)
	if err != nil {
		return false, nil, fmt.Errorf("enqueue: %w", err)
	}
	for _, id := range ids {
		if err := s.Finish(ctx, id, jobqueue.StateFailed, "boom"); err != nil {
			return false, nil, fmt.Errorf("finish %d: %w", id, err)
		}
	}
	return enqueued, ids, nil
}

func TestSupersedeOrphan_KeepsTerminalState(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	ctx := t.Context()

	if enqueued, _, err := store.Enqueue(ctx, jobqueue.NewJob{DeliveryID: "d1", Key: "k", Kind: "a", Payload: []byte("x")}); err != nil || !enqueued {
		t.Fatalf("Enqueue(d1) = %v, %v, want true, nil", enqueued, err)
	}
	if _, ok, err := store.Claim(ctx); err != nil || !ok {
		t.Fatalf("Claim() = %v, %v, want true, nil", ok, err)
	}

	w := jobqueue.NewWorker(finishingStore{store}, newCtrl().handle, testLogger(), 1)
	if enqueued, err := w.Enqueue(ctx, jobqueue.NewJob{DeliveryID: "d2", Key: "k", Kind: "a", Payload: []byte("x"), Supersedes: true}); err != nil || !enqueued {
		t.Fatalf("Enqueue(d2) = %v, %v, want true, nil", enqueued, err)
	}

	retried, _, err := store.Enqueue(ctx, jobqueue.NewJob{DeliveryID: "d1", Key: "k", Kind: "a", Payload: []byte("x")})
	if err != nil || !retried {
		t.Errorf("Enqueue(d1) after orphan supersede = %v, %v, want true, nil: the failed job must stay failed so a redelivery retries", retried, err)
	}
}

// ctrl lets a test observe when jobs start and are cancelled, and control when they finish.
type ctrl struct {
	started   chan jobqueue.Job
	cancelled chan jobqueue.Job

	mu       sync.Mutex
	releases map[string]chan error
}

func newCtrl() *ctrl {
	return &ctrl{
		started:   make(chan jobqueue.Job, 16),
		cancelled: make(chan jobqueue.Job, 16),
		releases:  make(map[string]chan error),
	}
}

func (c *ctrl) release(payload string) chan error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch, ok := c.releases[payload]
	if !ok {
		ch = make(chan error, 1)
		c.releases[payload] = ch
	}

	return ch
}

func (c *ctrl) handle(ctx context.Context, job jobqueue.Job) error {
	c.started <- job

	select {
	case err := <-c.release(string(job.Payload)):
		return err
	case <-ctx.Done():
		c.cancelled <- job

		return fmt.Errorf("job %d: %w", job.ID, ctx.Err())
	}
}

func waitJob(t *testing.T, ch chan jobqueue.Job, timeout time.Duration) jobqueue.Job {
	t.Helper()

	select {
	case job := <-ch:
		return job
	case <-time.After(timeout):
		t.Fatal("timed out waiting for job")

		return jobqueue.Job{}
	}
}

func assertNoJob(t *testing.T, ch chan jobqueue.Job, d time.Duration) {
	t.Helper()

	select {
	case job := <-ch:
		t.Fatalf("unexpected job: %+v", job)
	case <-time.After(d):
	}
}

func waitRun(t *testing.T, runDone chan error) {
	t.Helper()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for Run to return")
	}
}

func startWorker(t *testing.T, w *jobqueue.Worker) (runDone chan error, cancel context.CancelFunc) {
	t.Helper()

	runCtx, cancel := context.WithCancel(context.Background())
	runDone = make(chan error, 1)

	go func() { runDone <- w.Run(runCtx) }()

	return runDone, cancel
}

func TestSameKeyRunsSerially(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	c := newCtrl()
	w := jobqueue.NewWorker(store, c.handle, testLogger(), 4)
	runDone, cancelRun := startWorker(t, w)

	enqueued, err := w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d1", Key: "k", Kind: "a", Payload: []byte("1"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 1: enqueued=%v err=%v", enqueued, err)
	}

	job1 := waitJob(t, c.started, testTimeout)
	if string(job1.Payload) != "1" {
		t.Fatalf("job1 payload = %q, want %q", job1.Payload, "1")
	}

	enqueued, err = w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d2", Key: "k", Kind: "a", Payload: []byte("2"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 2: enqueued=%v err=%v", enqueued, err)
	}

	assertNoJob(t, c.started, 200*time.Millisecond)

	c.release("1") <- nil

	job2 := waitJob(t, c.started, testTimeout)
	if string(job2.Payload) != "2" {
		t.Fatalf("job2 payload = %q, want %q", job2.Payload, "2")
	}

	c.release("2") <- nil
	cancelRun()
	waitRun(t, runDone)
}

func TestDifferentKeysRunInParallel(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	c := newCtrl()
	w := jobqueue.NewWorker(store, c.handle, testLogger(), 4)
	runDone, cancelRun := startWorker(t, w)

	enqueued, err := w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d1", Key: "k1", Kind: "a", Payload: []byte("1"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 1: enqueued=%v err=%v", enqueued, err)
	}

	waitJob(t, c.started, testTimeout)

	enqueued, err = w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d2", Key: "k2", Kind: "a", Payload: []byte("2"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 2: enqueued=%v err=%v", enqueued, err)
	}

	job2 := waitJob(t, c.started, testTimeout)
	if string(job2.Payload) != "2" {
		t.Fatalf("job2 payload = %q, want %q", job2.Payload, "2")
	}

	c.release("1") <- nil
	c.release("2") <- nil
	cancelRun()
	waitRun(t, runDone)
}

func TestDuplicateDeliveryIDIsNoop(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	c := newCtrl()
	w := jobqueue.NewWorker(store, c.handle, testLogger(), 4)
	runDone, cancelRun := startWorker(t, w)

	enqueued, err := w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "dup", Key: "k", Kind: "a", Payload: []byte("1"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 1: enqueued=%v err=%v", enqueued, err)
	}

	waitJob(t, c.started, testTimeout)

	enqueued, err = w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "dup", Key: "k", Kind: "a", Payload: []byte("2"),
	})
	if err != nil {
		t.Fatalf("enqueue duplicate delivery: %v", err)
	}

	if enqueued {
		t.Fatal("duplicate delivery ID was enqueued")
	}

	assertNoJob(t, c.started, 200*time.Millisecond)

	c.release("1") <- nil
	cancelRun()
	waitRun(t, runDone)
}

func TestSupersedeCancelsRunningJob(t *testing.T) {
	t.Parallel()

	store := newStore(t)
	c := newCtrl()
	w := jobqueue.NewWorker(store, c.handle, testLogger(), 4)
	runDone, cancelRun := startWorker(t, w)

	enqueued, err := w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d1", Key: "k", Kind: "a", Payload: []byte("1"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 1: enqueued=%v err=%v", enqueued, err)
	}

	waitJob(t, c.started, testTimeout)

	enqueued, err = w.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d2", Key: "k", Kind: "a", Payload: []byte("2"), Supersedes: true,
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 2: enqueued=%v err=%v", enqueued, err)
	}

	cancelledJob := waitJob(t, c.cancelled, testTimeout)
	if string(cancelledJob.Payload) != "1" {
		t.Fatalf("cancelled job payload = %q, want %q", cancelledJob.Payload, "1")
	}

	job2 := waitJob(t, c.started, testTimeout)
	if string(job2.Payload) != "2" {
		t.Fatalf("job2 payload = %q, want %q", job2.Payload, "2")
	}

	c.release("2") <- nil
	cancelRun()
	waitRun(t, runDone)
}

func TestUnfinishedJobsRunAfterRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")

	store1, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	c1 := newCtrl()
	w1 := jobqueue.NewWorker(store1, c1.handle, testLogger(), 4)
	runDone1, cancelRun1 := startWorker(t, w1)

	enqueued, err := w1.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d1", Key: "k", Kind: "a", Payload: []byte("1"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job: enqueued=%v err=%v", enqueued, err)
	}

	waitJob(t, c1.started, testTimeout)

	cancelRun1()
	waitRun(t, runDone1)

	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	store2, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := store2.Close(); err != nil {
			t.Errorf("close store2: %v", err)
		}
	})

	c2 := newCtrl()
	w2 := jobqueue.NewWorker(store2, c2.handle, testLogger(), 4)
	runDone2, cancelRun2 := startWorker(t, w2)
	t.Cleanup(cancelRun2)

	job := waitJob(t, c2.started, testTimeout)
	if job.Key != "k" || job.Kind != "a" || string(job.Payload) != "1" {
		t.Fatalf("unexpected requeued job: %+v", job)
	}

	c2.release("1") <- nil
	cancelRun2()
	waitRun(t, runDone2)
}

// barrierCtrl is like ctrl but its handle ignores job ctx cancellation, so a test controls
// exactly when and with what result the handler returns, including after Run's ctx is cancelled.
type barrierCtrl struct {
	started chan jobqueue.Job

	mu       sync.Mutex
	releases map[string]chan error
}

func newBarrierCtrl() *barrierCtrl {
	return &barrierCtrl{
		started:  make(chan jobqueue.Job, 16),
		releases: make(map[string]chan error),
	}
}

func (c *barrierCtrl) release(payload string) chan error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch, ok := c.releases[payload]
	if !ok {
		ch = make(chan error, 1)
		c.releases[payload] = ch
	}

	return ch
}

func (c *barrierCtrl) handle(_ context.Context, job jobqueue.Job) error {
	c.started <- job

	return <-c.release(string(job.Payload))
}

func TestNilReturnDuringShutdownIsDone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")

	store1, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	c1 := newBarrierCtrl()
	w1 := jobqueue.NewWorker(store1, c1.handle, testLogger(), 4)
	runDone1, cancelRun1 := startWorker(t, w1)

	enqueued, err := w1.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d1", Key: "k1", Kind: "a", Payload: []byte("1"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job: enqueued=%v err=%v", enqueued, err)
	}

	waitJob(t, c1.started, testTimeout)

	// Cancel Run while the handler is blocked, then let the handler return nil: the job
	// must be recorded done, not left running, even though shutdown is in progress.
	cancelRun1()
	c1.release("1") <- nil
	waitRun(t, runDone1)

	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	store2, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := store2.Close(); err != nil {
			t.Errorf("close store2: %v", err)
		}
	})

	c2 := newBarrierCtrl()
	w2 := jobqueue.NewWorker(store2, c2.handle, testLogger(), 4)
	runDone2, cancelRun2 := startWorker(t, w2)
	t.Cleanup(cancelRun2)

	enqueued, err = w2.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d2", Key: "k2", Kind: "a", Payload: []byte("2"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue barrier job: enqueued=%v err=%v", enqueued, err)
	}

	barrier := waitJob(t, c2.started, testTimeout)
	if string(barrier.Payload) != "2" {
		t.Fatalf("barrier job payload = %q, want %q", barrier.Payload, "2")
	}

	assertNoJob(t, c2.started, 200*time.Millisecond)

	c2.release("2") <- nil
	cancelRun2()
	waitRun(t, runDone2)
}

func TestOrphanedRunningJobIsSupersededBeforeRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")

	store1, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	c1 := newCtrl()
	w1 := jobqueue.NewWorker(store1, c1.handle, testLogger(), 4)
	runDone1, cancelRun1 := startWorker(t, w1)

	enqueued, err := w1.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d1", Key: "k", Kind: "a", Payload: []byte("1"),
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 1: enqueued=%v err=%v", enqueued, err)
	}

	waitJob(t, c1.started, testTimeout)

	// Cancel Run while the handler is blocked: the job is left running in the store,
	// and this worker's w.running map is discarded when the process (here, w1) goes away.
	cancelRun1()
	waitRun(t, runDone1)

	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	store2, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := store2.Close(); err != nil {
			t.Errorf("close store2: %v", err)
		}
	})

	c2 := newCtrl()
	w2 := jobqueue.NewWorker(store2, c2.handle, testLogger(), 4)

	// Enqueue the superseding job before Run (and its RequeueRunning) starts: the orphaned
	// job 1 is not in w2's running map, so it must be finished here, not left to be requeued.
	enqueued, err = w2.Enqueue(context.Background(), jobqueue.NewJob{
		DeliveryID: "d2", Key: "k", Kind: "a", Payload: []byte("2"), Supersedes: true,
	})
	if err != nil || !enqueued {
		t.Fatalf("enqueue job 2: enqueued=%v err=%v", enqueued, err)
	}

	runDone2, cancelRun2 := startWorker(t, w2)
	t.Cleanup(cancelRun2)

	job := waitJob(t, c2.started, testTimeout)
	if string(job.Payload) != "2" {
		t.Fatalf("started job payload = %q, want %q", job.Payload, "2")
	}

	assertNoJob(t, c2.started, 200*time.Millisecond)

	c2.release("2") <- nil
	cancelRun2()
	waitRun(t, runDone2)
}
