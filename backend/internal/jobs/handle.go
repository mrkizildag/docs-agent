package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
)

// JobHandler is what HandleJob dispatches to: the gate's handlers for pull
// request and scaffold jobs.
type JobHandler interface {
	HandlePullRequest(ctx context.Context, pr gate.PullRequest) error
	HandleComment(ctx context.Context, ev gate.CommentEvent) error
	HandleRerun(ctx context.Context, r gate.RerunRequest) error
	HandleRunCompleted(ctx context.Context, rc gate.RunCompleted) error
	HandleDeadline(ctx context.Context, ref gate.PRRef, nonce string, now time.Time) error
	HandleScaffold(ctx context.Context, ref gate.RepoRef) error
	HandleScaffoldRun(ctx context.Context, rc gate.RunCompleted) error
	HandleScaffoldDeadline(ctx context.Context, ref gate.RepoRef, nonce string, now time.Time) error
}

// decodeAndHandle decodes job's payload as T and passes it to handle.
func decodeAndHandle[T any](ctx context.Context, job jobqueue.Job, handle func(context.Context, T) error) error {
	var payload T
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode job %d (%s) payload (kind %s): %w", job.ID, job.Key, job.Kind, err)
	}
	if err := handle(ctx, payload); err != nil {
		return fmt.Errorf("handle %s job %d (%s): %w", job.Kind, job.ID, job.Key, err)
	}
	return nil
}

// HandleJob decodes a durable job's payload by its Kind and dispatches it to h.
func HandleJob(h JobHandler) jobqueue.Handler {
	return func(ctx context.Context, job jobqueue.Job) error {
		switch jobKind(job.Kind) {
		case pullRequestJobKind:
			return decodeAndHandle(ctx, job, func(ctx context.Context, p legacyPullRequestPayload) error {
				switch {
				case p.Comment != nil:
					return h.HandleComment(ctx, *p.Comment)
				case p.Rerun != nil:
					return h.HandleRerun(ctx, *p.Rerun)
				default:
					return h.HandlePullRequest(ctx, p.PullRequest)
				}
			})
		case rerunJobKind:
			return decodeAndHandle(ctx, job, h.HandleRerun)
		case commentJobKind, commentRerunJobKind:
			return decodeAndHandle(ctx, job, h.HandleComment)
		case workflowRunJobKind:
			return decodeAndHandle(ctx, job, h.HandleRunCompleted)
		case runDeadlineJobKind:
			return decodeAndHandle(ctx, job, func(ctx context.Context, o gate.OverdueRun) error {
				return h.HandleDeadline(ctx, o.PRRef, o.Nonce, time.Now())
			})
		case scaffoldJobKind:
			return decodeAndHandle(ctx, job, h.HandleScaffold)
		case scaffoldRunJobKind:
			return decodeAndHandle(ctx, job, h.HandleScaffoldRun)
		case scaffoldDeadlineJobKind:
			return decodeAndHandle(ctx, job, func(ctx context.Context, o gate.OverdueRun) error {
				return h.HandleScaffoldDeadline(ctx, gate.RepoRef{Owner: o.Owner, Repo: o.Repo}, o.Nonce, time.Now())
			})
		default:
			return fmt.Errorf("job %d (%s): unknown kind %q", job.ID, job.Key, job.Kind)
		}
	}
}
