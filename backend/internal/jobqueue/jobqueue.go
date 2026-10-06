// Package jobqueue runs durable jobs: one at a time per key, keys in parallel.
package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type State string

const (
	StatePending    State = "pending"
	StateRunning    State = "running"
	StateDone       State = "done"
	StateFailed     State = "failed"
	StateSuperseded State = "superseded"
)

// errSuperseded is the context cancellation cause for a running job replaced by a newer one.
var errSuperseded = errors.New("job superseded by a newer job")

// NewJob describes a job to enqueue.
type NewJob struct {
	DeliveryID string // dedup key; a DeliveryID already seen is a no-op
	Key        string // jobs with the same Key run one at a time
	Kind       string // what the job carries; the handler dispatches on it
	Group      string // supersede scope within a Key; empty means Kind
	Payload    []byte
	Supersedes bool // cancel older pending and running jobs with the same Key and Group
}

// Job is a unit of work claimed from the Store.
type Job struct {
	ID         int64
	Key        string
	Kind       string
	Payload    []byte
	DeliveryID string
}

// Store persists jobs and deliveries durably.
type Store interface {
	// Enqueue records the delivery and the job in one transaction. A seen DeliveryID returns
	// enqueued=false and changes nothing. With Supersedes, older pending jobs of the same Key
	// and Group become superseded, and the IDs of running ones are returned for cancellation.
	Enqueue(ctx context.Context, job NewJob) (enqueued bool, supersededRunning []int64, err error)
	// Claim marks the oldest pending job whose Key has no running job as running and returns it.
	Claim(ctx context.Context) (job Job, ok bool, err error)
	// Finish records a running job's terminal state; errMsg is "" unless failed. A job that is
	// no longer running keeps its state.
	Finish(ctx context.Context, id int64, state State, errMsg string) error
	// RequeueRunning moves every running job back to pending; called once at startup.
	RequeueRunning(ctx context.Context) (int, error)
}

// Handler runs a single job. The context is cancelled if the job is superseded or the worker shuts down.
type Handler func(ctx context.Context, job Job) error

// Worker claims and runs jobs from a Store, keyed to run one at a time per Key.
type Worker struct {
	store       Store
	handle      Handler
	logger      *slog.Logger
	maxParallel int

	wake    chan struct{}
	jobDone chan struct{}

	mu      sync.Mutex
	running map[int64]context.CancelCauseFunc
}

// NewWorker builds a Worker that runs up to maxParallel jobs concurrently, one per Key.
func NewWorker(store Store, handle Handler, logger *slog.Logger, maxParallel int) *Worker {
	return &Worker{
		store:       store,
		handle:      handle,
		logger:      logger,
		maxParallel: maxParallel,
		wake:        make(chan struct{}, 1),
		jobDone:     make(chan struct{}, 1),
		running:     make(map[int64]context.CancelCauseFunc),
	}
}

// Enqueue records a new job and, if it supersedes running jobs, cancels them. Safe to call
// concurrently with Run.
func (w *Worker) Enqueue(ctx context.Context, job NewJob) (enqueued bool, err error) {
	enqueued, supersededRunning, err := w.store.Enqueue(ctx, job)
	if err != nil {
		return false, fmt.Errorf("enqueue job: %w", err)
	}
	if !enqueued {
		return false, nil
	}

	w.mu.Lock()
	var orphaned []int64
	for _, id := range supersededRunning {
		if cancel, ok := w.running[id]; ok {
			cancel(errSuperseded)
		} else {
			orphaned = append(orphaned, id)
		}
	}
	w.mu.Unlock()

	// An ID absent from w.running is a job orphaned by a previous process, or one that finished
	// after the store listed it; Finish only moves a job that is still running.
	for _, id := range orphaned {
		if ferr := w.store.Finish(ctx, id, StateSuperseded, ""); ferr != nil {
			w.logger.Error("finish orphaned superseded job", "job_id", id, "err", ferr)
		}
	}

	w.notify(w.wake)

	return true, nil
}

// Run requeues jobs left running from a previous run, then claims and runs jobs until ctx is
// cancelled. It waits for in-flight jobs to return before Run returns.
func (w *Worker) Run(ctx context.Context) error {
	if _, err := w.store.RequeueRunning(ctx); err != nil {
		return fmt.Errorf("requeue running jobs: %w", err)
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		for w.runningCount() < w.maxParallel {
			job, jobCtx, cancel, ok := w.claim(ctx)
			if !ok {
				break
			}

			wg.Add(1)
			go w.runJob(ctx, jobCtx, cancel, job, &wg)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-w.wake:
		case <-w.jobDone:
		case <-ticker.C:
		}
	}
}

// claim holds mu across the store claim and the cancel registration, so an Enqueue that
// supersedes the job in between still finds its cancel func.
func (w *Worker) claim(ctx context.Context) (Job, context.Context, context.CancelCauseFunc, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	job, ok, err := w.store.Claim(ctx)
	if err != nil {
		w.logger.Error("claim job", "err", err)
		return Job{}, nil, nil, false
	}
	if !ok {
		return Job{}, nil, nil, false
	}

	jobCtx, cancel := context.WithCancelCause(ctx)
	w.running[job.ID] = cancel
	return job, jobCtx, cancel, true
}

func (w *Worker) runJob(parentCtx, jobCtx context.Context, cancel context.CancelCauseFunc, job Job, wg *sync.WaitGroup) {
	defer wg.Done()

	err := w.handle(jobCtx, job)
	cause := context.Cause(jobCtx)
	cancel(nil)

	finishCtx := context.WithoutCancel(jobCtx)

	switch {
	case errors.Is(cause, errSuperseded):
		if ferr := w.store.Finish(finishCtx, job.ID, StateSuperseded, ""); ferr != nil {
			w.logger.Error("finish superseded job", jobAttrs(job, "err", ferr)...)
		}
	case err != nil && parentCtx.Err() != nil:
		// The handler returned an error caused by the shutdown cancel: leave the job
		// running so RequeueRunning picks it up on restart.
	case err != nil:
		w.logger.Error("job failed", jobAttrs(job, "err", err)...)
		if ferr := w.store.Finish(finishCtx, job.ID, StateFailed, err.Error()); ferr != nil {
			w.logger.Error("finish failed job", jobAttrs(job, "err", ferr)...)
		}
	default:
		if ferr := w.store.Finish(finishCtx, job.ID, StateDone, ""); ferr != nil {
			w.logger.Error("finish done job", jobAttrs(job, "err", ferr)...)
		}
	}

	w.mu.Lock()
	delete(w.running, job.ID)
	w.mu.Unlock()

	w.notify(w.jobDone)
}

// jobAttrs returns the log attributes that identify job, so its lines join with httpapi's delivery_id.
func jobAttrs(job Job, extra ...any) []any {
	attrs := []any{"job_id", job.ID, "key", job.Key, "kind", job.Kind, "delivery_id", job.DeliveryID}
	return append(attrs, extra...)
}

func (w *Worker) runningCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.running)
}

func (w *Worker) notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
