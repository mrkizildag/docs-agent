package httpapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
)

// pullRequestJobKind identifies durable jobs carrying a gate.PullRequest
// payload. webhookHandler encodes jobs with this kind; HandleJob decodes them.
const pullRequestJobKind = "pull_request"

// PullRequestHandler reports the docs-agent check run for a pull request.
type PullRequestHandler interface {
	HandlePullRequest(ctx context.Context, pr gate.PullRequest) error
}

// HandleJob decodes a durable job's payload by its Kind and dispatches it to prs.
func HandleJob(prs PullRequestHandler) jobqueue.Handler {
	return func(ctx context.Context, job jobqueue.Job) error {
		switch job.Kind {
		case pullRequestJobKind:
			var pr gate.PullRequest
			if err := json.Unmarshal(job.Payload, &pr); err != nil {
				return fmt.Errorf("decode job %d payload (kind %s): %w", job.ID, job.Kind, err)
			}
			if err := prs.HandlePullRequest(ctx, pr); err != nil {
				return fmt.Errorf("handle pull request job %d: %w", job.ID, err)
			}
			return nil
		default:
			return fmt.Errorf("job %d: unknown kind %q", job.ID, job.Kind)
		}
	}
}
