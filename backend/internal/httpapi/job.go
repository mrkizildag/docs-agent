package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
)

// prJobKey is the per-PR queue key that serializes a pull request's jobs.
func prJobKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// pullRequestJobKind identifies durable jobs carrying a pullRequestJobPayload.
// webhookHandler encodes jobs with this kind; HandleJob decodes them.
const pullRequestJobKind = "pull_request"

// commentJobKind identifies durable jobs carrying a gate.CommentEvent payload.
const commentJobKind = "comment"

// pullRequestJobPayload is a gate.PullRequest to analyze, or when Rerun is set,
// a request to re-analyze the PR's current head, or when Comment is set, a
// summary Re-run tick to act on (the embedded PullRequest is then zero).
type pullRequestJobPayload struct {
	gate.PullRequest
	Rerun   *gate.RerunRequest `json:",omitempty"`
	Comment *gate.CommentEvent `json:",omitempty"`
}

// scaffoldJobKind identifies durable jobs carrying a gate.RepoRef payload.
const scaffoldJobKind = "scaffold"

// scaffoldJobKey is the per-repo queue key that serializes a repo's scaffold jobs.
func scaffoldJobKey(ref gate.RepoRef) string {
	return fmt.Sprintf("%s/%s#scaffold", ref.Owner, ref.Repo)
}

// JobStore is the durable job store's Enqueue, which a ScaffoldQueue calls
// directly: the worker that handles scaffold jobs is built after the gate.
type JobStore interface {
	Enqueue(ctx context.Context, job jobqueue.NewJob) (enqueued bool, supersededRunning []int64, err error)
}

// ScaffoldQueue implements gate.ScaffoldQueue over a durable job store. Its jobs
// never supersede; the worker claims them on its next poll.
type ScaffoldQueue struct {
	jobs JobStore
}

var _ gate.ScaffoldQueue = (*ScaffoldQueue)(nil)

// NewScaffoldQueue returns a ScaffoldQueue that enqueues into jobs.
func NewScaffoldQueue(jobs JobStore) *ScaffoldQueue {
	return &ScaffoldQueue{jobs: jobs}
}

// EnqueueScaffold enqueues the scaffold job of ref's repo for attempt. A job
// for an attempt already enqueued is a no-op.
func (q *ScaffoldQueue) EnqueueScaffold(ctx context.Context, ref gate.RepoRef, attempt int) error {
	payload, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("encode scaffold job payload for %s/%s: %w", ref.Owner, ref.Repo, err)
	}
	if _, _, err := q.jobs.Enqueue(ctx, jobqueue.NewJob{
		DeliveryID: fmt.Sprintf("scaffold:%s/%s:%d", ref.Owner, ref.Repo, attempt),
		Key:        scaffoldJobKey(ref),
		Kind:       scaffoldJobKind,
		Payload:    payload,
	}); err != nil {
		return fmt.Errorf("enqueue scaffold job for %s/%s: %w", ref.Owner, ref.Repo, err)
	}
	return nil
}

// workflowRunJobKind identifies durable jobs carrying a gate.RunCompleted payload.
const workflowRunJobKind = "workflow_run"

// scaffoldRunJobKind identifies durable jobs carrying the gate.RunCompleted of a
// scaffold's workflow run, on the repo's scaffold key.
const scaffoldRunJobKind = "scaffold_run"

// scaffoldDeadlineJobKind identifies durable jobs carrying a gate.OverdueRun of a
// scaffold, on the repo's scaffold key.
const scaffoldDeadlineJobKind = "scaffold_deadline"

// runDeadlineJobKind identifies durable jobs carrying a gate.OverdueRun payload.
const runDeadlineJobKind = "run_deadline"

// OverdueSource lists the awaited analysis runs that are past their deadline.
type OverdueSource interface {
	OverdueRuns(ctx context.Context, now time.Time) ([]gate.OverdueRun, error)
}

const (
	// deadlineRetryCap is how long past its deadline a run is still retried.
	deadlineRetryCap = 24 * time.Hour
	// deadlineTailEvery is the retry interval once the power-of-two buckets reach deadlineTailFrom.
	deadlineTailEvery = 240
	deadlineTailFrom  = 1024
	// DeadlineSweepEvery is how often cmd/server runs EnqueueDeadlineJobs; the
	// give-up warn window matches it so the warn logs once.
	DeadlineSweepEvery = 30 * time.Second
)

// EnqueueDeadlineJobs enqueues deadline jobs for overdue runs. A run's jobs
// dedupe by nonce and by the power-of-two bucket of whole minutes past its
// deadline, so a run whose job failed is retried at about 0, 1, 2, 4, 8, ...
// minutes overdue, then every 240 minutes from 1024. Runs more than deadlineRetryCap overdue are dropped.
func EnqueueDeadlineJobs(ctx context.Context, src OverdueSource, jobs Enqueuer, logger *slog.Logger, now time.Time) error {
	overdue, err := src.OverdueRuns(ctx, now)
	if err != nil {
		return fmt.Errorf("enqueue deadline jobs: %w", err)
	}
	for _, run := range overdue {
		late := now.Sub(run.Deadline)
		if late > deadlineRetryCap {
			if late < deadlineRetryCap+DeadlineSweepEvery {
				logger.Warn("giving up on overdue run", "owner", run.Owner, "repo", run.Repo, "number", run.Number, "nonce", run.Nonce)
			}
			continue
		}
		payload, err := json.Marshal(run)
		if err != nil {
			return fmt.Errorf("encode deadline job payload for %s/%s#%d: %w", run.Owner, run.Repo, run.Number, err)
		}
		minutes := int(late / time.Minute)
		bucket := 0
		if minutes >= deadlineTailFrom {
			bucket = 11 + (minutes-deadlineTailFrom)/deadlineTailEvery
		} else {
			for m := minutes; m > 0; m >>= 1 {
				bucket++
			}
		}
		key, kind := prJobKey(run.Owner, run.Repo, run.Number), runDeadlineJobKind
		if run.Scaffold {
			key, kind = scaffoldJobKey(gate.RepoRef{Owner: run.Owner, Repo: run.Repo}), scaffoldDeadlineJobKind
		}
		if _, err := jobs.Enqueue(ctx, jobqueue.NewJob{
			DeliveryID: fmt.Sprintf("deadline:%s:%d", run.Nonce, bucket),
			Key:        key,
			Kind:       kind,
			Payload:    payload,
		}); err != nil {
			return fmt.Errorf("enqueue deadline job for %s/%s#%d: %w", run.Owner, run.Repo, run.Number, err)
		}
	}
	return nil
}

// PullRequestHandler reports the pollux-agent check run for a pull request.
type PullRequestHandler interface {
	HandlePullRequest(ctx context.Context, pr gate.PullRequest) error
	HandleComment(ctx context.Context, ev gate.CommentEvent) error
	HandleRerun(ctx context.Context, r gate.RerunRequest) error
	HandleRunCompleted(ctx context.Context, rc gate.RunCompleted) error
	HandleDeadline(ctx context.Context, ref gate.PRRef, nonce string, now time.Time) error
	HandleScaffold(ctx context.Context, ref gate.RepoRef) error
	HandleScaffoldRun(ctx context.Context, rc gate.RunCompleted) error
	HandleScaffoldDeadline(ctx context.Context, ref gate.RepoRef, nonce string, now time.Time) error
}

// HandleJob decodes a durable job's payload by its Kind and dispatches it to prs.
func HandleJob(prs PullRequestHandler) jobqueue.Handler {
	return func(ctx context.Context, job jobqueue.Job) error {
		switch job.Kind {
		case pullRequestJobKind:
			var payload pullRequestJobPayload
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if payload.Comment != nil {
				if err := prs.HandleComment(ctx, *payload.Comment); err != nil {
					return fmt.Errorf("handle rerun comment job %d: %w", job.ID, err)
				}
				return nil
			}
			if payload.Rerun != nil {
				if err := prs.HandleRerun(ctx, *payload.Rerun); err != nil {
					return fmt.Errorf("handle rerun job %d: %w", job.ID, err)
				}
				return nil
			}
			if err := prs.HandlePullRequest(ctx, payload.PullRequest); err != nil {
				return fmt.Errorf("handle pull request job %d: %w", job.ID, err)
			}
			return nil
		case commentJobKind:
			var ev gate.CommentEvent
			if err := json.Unmarshal(job.Payload, &ev); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if err := prs.HandleComment(ctx, ev); err != nil {
				return fmt.Errorf("handle comment job %d: %w", job.ID, err)
			}
			return nil
		case workflowRunJobKind:
			var rc gate.RunCompleted
			if err := json.Unmarshal(job.Payload, &rc); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if err := prs.HandleRunCompleted(ctx, rc); err != nil {
				return fmt.Errorf("handle workflow run job %d: %w", job.ID, err)
			}
			return nil
		case runDeadlineJobKind:
			var run gate.OverdueRun
			if err := json.Unmarshal(job.Payload, &run); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if err := prs.HandleDeadline(ctx, run.PRRef, run.Nonce, time.Now()); err != nil {
				return fmt.Errorf("handle deadline job %d: %w", job.ID, err)
			}
			return nil
		case scaffoldJobKind:
			var ref gate.RepoRef
			if err := json.Unmarshal(job.Payload, &ref); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if err := prs.HandleScaffold(ctx, ref); err != nil {
				return fmt.Errorf("handle scaffold job %d: %w", job.ID, err)
			}
			return nil
		case scaffoldRunJobKind:
			var rc gate.RunCompleted
			if err := json.Unmarshal(job.Payload, &rc); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if err := prs.HandleScaffoldRun(ctx, rc); err != nil {
				return fmt.Errorf("handle scaffold run job %d: %w", job.ID, err)
			}
			return nil
		case scaffoldDeadlineJobKind:
			var run gate.OverdueRun
			if err := json.Unmarshal(job.Payload, &run); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if err := prs.HandleScaffoldDeadline(ctx, gate.RepoRef{Owner: run.Owner, Repo: run.Repo}, run.Nonce, time.Now()); err != nil {
				return fmt.Errorf("handle scaffold deadline job %d: %w", job.ID, err)
			}
			return nil
		default:
			return fmt.Errorf("job %d: unknown kind %q", job.ID, job.Kind)
		}
	}
}
