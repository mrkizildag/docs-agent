package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
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

// pullRequestJobPayload is a gate.PullRequest to analyze, or when Rerun is set,
// a request to re-analyze the PR's current head (the embedded PullRequest is
// then zero).
type pullRequestJobPayload struct {
	gate.PullRequest
	Rerun *gate.RerunRequest `json:",omitempty"`
}

// workflowRunJobKind identifies durable jobs carrying a gate.RunCompleted payload.
const workflowRunJobKind = "workflow_run"

// runDeadlineJobKind identifies durable jobs carrying a gate.OverdueRun payload.
const runDeadlineJobKind = "run_deadline"

// OverdueSource lists the awaited analysis runs that are past their deadline.
type OverdueSource interface {
	OverdueRuns(ctx context.Context, now time.Time) ([]gate.OverdueRun, error)
}

// EnqueueDeadlineJobs enqueues one deadline job per overdue run. Jobs dedupe
// by nonce and minute: sweeping again within the minute adds nothing, and a
// later minute's sweep retries a run whose earlier job failed.
func EnqueueDeadlineJobs(ctx context.Context, src OverdueSource, jobs Enqueuer, now time.Time) error {
	overdue, err := src.OverdueRuns(ctx, now)
	if err != nil {
		return fmt.Errorf("enqueue deadline jobs: %w", err)
	}
	for _, run := range overdue {
		payload, err := json.Marshal(run)
		if err != nil {
			return fmt.Errorf("encode deadline job payload for %s/%s#%d: %w", run.Owner, run.Repo, run.Number, err)
		}
		if _, err := jobs.Enqueue(ctx, jobqueue.NewJob{
			DeliveryID: "deadline:" + run.Nonce + ":" + now.Truncate(time.Minute).UTC().Format(time.RFC3339),
			Key:        prJobKey(run.Owner, run.Repo, run.Number),
			Kind:       runDeadlineJobKind,
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
	HandleRerun(ctx context.Context, r gate.RerunRequest) error
	HandleRunCompleted(ctx context.Context, rc gate.RunCompleted) error
	HandleDeadline(ctx context.Context, ref gate.PRRef, nonce string, now time.Time) error
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
		default:
			return fmt.Errorf("job %d: unknown kind %q", job.ID, job.Kind)
		}
	}
}
