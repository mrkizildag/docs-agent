package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobs"
)

func TestWebhookRedeliveryAfterFailedJobEnqueuesNewJob(t *testing.T) {
	t.Parallel()

	gh := repoGitHub()
	gh.Before = func(c gatetest.Call) error {
		if c.Method == "CreateCheckRun" && c.N == 1 {
			return errors.New("create check run: boom")
		}
		return nil
	}
	sys := start(t, sqlitetest.Open(t), gh, gate.Runners{})

	body := pullRequestBody(t, 1, "sha1", pushOpts{})
	sys.deliverAs("d1", "pull_request", body)
	waitFor(t, "the first, failing CreateCheckRun", func() bool { return gh.CallCount("CreateCheckRun") >= 1 })

	// The worker marks the job failed asynchronously after CreateCheckRun returns; redelivering
	// before that lands correctly finds the job still running and stays a no-op, so redeliver
	// until the job has failed and the redelivery is taken.
	waitFor(t, "a redelivery of the failed delivery to run again", func() bool {
		sys.deliverAs("d1", "pull_request", body)
		return gh.CallCount("CreateCheckRun") >= 2
	})
	run := waitNth(t, "the created check run", gh.CheckRuns, 1)
	if run.Created.HeadSHA != "sha1" {
		t.Errorf("check run HeadSHA = %q, want sha1", run.Created.HeadSHA)
	}
}

func TestWebhookSecondSynchronizeCancelsFirst(t *testing.T) {
	t.Parallel()

	cancelled := make(chan struct{})
	gh := repoGitHub()
	gh.BeforeContext = func(ctx context.Context, c gatetest.Call) error {
		if c.Method != "CreateCheckRun" || c.N != 1 {
			return nil
		}
		<-ctx.Done()
		close(cancelled)
		return fmt.Errorf("create check run: %w", context.Cause(ctx))
	}
	sys := start(t, sqlitetest.Open(t), gh, gate.Runners{})

	sys.deliverAs("d1", "pull_request", pullRequestBody(t, 1, "sha1", pushOpts{action: "synchronize"}))
	waitFor(t, "the first check run to start", func() bool { return gh.CallCount("CreateCheckRun") >= 1 })

	sys.deliverAs("d2", "pull_request", pullRequestBody(t, 1, "sha2", pushOpts{action: "synchronize"}))
	waitClosed(t, cancelled, "the first job to be cancelled")
	run := waitNth(t, "the check run of sha2", gh.CheckRuns, 1)
	if run.Created.HeadSHA != "sha2" {
		t.Errorf("created check run HeadSHA = %q, want sha2", run.Created.HeadSHA)
	}
}

func TestWebhookPendingJobRunsAfterRestartAndDuplicateStaysNoOp(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	body := pullRequestBody(t, 1, "sha1", pushOpts{})

	// Process 1 accepts the webhook but dies before any worker runs it.
	store1, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) = %v", path, err)
	}
	newSystem(t, store1, repoGitHub(), gate.Runners{}).deliverAs("d1", "pull_request", body)
	if err := store1.Close(); err != nil {
		t.Fatalf("close store1: %v", err)
	}

	// Process 2 runs the unfinished job.
	store2, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) = %v", path, err)
	}
	t.Cleanup(func() {
		if err := store2.Close(); err != nil {
			t.Errorf("close store2: %v", err)
		}
	})
	gh := repoGitHub()
	sys := start(t, store2, gh, gate.Runners{})
	run := waitNth(t, "the check run after restart", gh.CheckRuns, 1)
	if run.Created.HeadSHA != "sha1" {
		t.Fatalf("created check run for %q after restart, want sha1", run.Created.HeadSHA)
	}

	// The same delivery redelivered after restart is still a no-op.
	sys.deliverAs("d1", "pull_request", body)
	sys.deliverAs("d2", "pull_request", pullRequestBody(t, 2, "sha2", pushOpts{}))
	barrier := waitNth(t, "the barrier check run", gh.CheckRuns, 2)
	if barrier.Created.HeadSHA != "sha2" {
		t.Fatalf("created check run for %q, want barrier sha2", barrier.Created.HeadSHA)
	}
	// No event marks a duplicate job that must not happen, so give one time to appear.
	time.Sleep(300 * time.Millisecond)
	if n := len(gh.CheckRuns()); n != 2 {
		t.Errorf("check runs = %d, want 2: the duplicate delivery must not start another", n)
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

// awaitingRun saves a PR whose awaited run is due at deadline.
func awaitingRun(t *testing.T, store *sqlite.Store, deadline time.Time) {
	t.Helper()

	state := gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1", CheckRunID: 5,
		Run: &gate.AwaitingRun{RunID: 9, Nonce: "n1", Deadline: deadline},
	}
	if err := store.SavePR(t.Context(), state); err != nil {
		t.Fatalf("SavePR() = %v", err)
	}
}

func TestEnqueueDeadlineJobsIsIdempotent(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	awaitingRun(t, store, deadline)

	counter := &countingEnqueuer{next: newWorker(store, repoGitHub(), gate.Runners{})}

	for _, now := range []time.Time{deadline.Add(-time.Second), deadline.Add(time.Second), deadline.Add(2 * time.Second)} {
		if err := jobs.EnqueueDeadlineJobs(t.Context(), store, counter, slog.New(slog.DiscardHandler), now); err != nil {
			t.Fatalf("EnqueueDeadlineJobs(%v) = %v", now, err)
		}
	}
	if counter.enqueued != 1 {
		t.Errorf("deadline jobs enqueued = %d, want 1 across a not-yet-due sweep and two overdue sweeps in one minute", counter.enqueued)
	}
}

func TestDeadlineJobFailedConcludeIsRetriedByLaterSweep(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	awaitingRun(t, store, deadline)

	gh := repoGitHub()
	gh.Before = func(c gatetest.Call) error {
		if c.Method == "UpdateCheckRun" && c.N == 1 {
			return errors.New("github rejected the conclude write")
		}
		return nil
	}
	sys := start(t, store, gh, gate.Runners{})

	now := deadline.Add(time.Second)
	sweep := func(at time.Time) {
		t.Helper()

		if err := jobs.EnqueueDeadlineJobs(t.Context(), store, sys.worker, slog.New(slog.DiscardHandler), at); err != nil {
			t.Fatalf("EnqueueDeadlineJobs() = %v", err)
		}
	}
	sweep(now)
	waitFor(t, "the first conclude attempt", func() bool { return gh.CallCount("UpdateCheckRun") >= 1 })

	var run gate.CheckRun
	i := 0
	waitFor(t, "the retried conclude write", func() bool {
		i++
		sweep(now.Add(time.Duration(i) * time.Minute))
		runs := gh.CheckRuns()
		if len(runs) == 0 {
			return false
		}
		run = runs[0].Latest()
		return true
	})
	if run.Conclusion != gate.ConclusionNeutral {
		t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionNeutral)
	}
}
