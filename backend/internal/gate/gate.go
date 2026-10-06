// Package gate is the domain of the pollux-agent GitHub App: what check run a
// pull request gets, which proposal and summary comments it carries, and what
// acting on those comments does. HandlePullRequest, HandleRunCompleted and
// HandleDeadline report the check run; HandleComment applies proposals to the
// head branch (Apply), waives the check (skip) and re-runs the analysis;
// HandleScaffold and its siblings write and propose the docs scaffold of a repo
// that has no docs/ folder. State transitions are pure functions over PRState and
// ScaffoldState; the Service performs their I/O through the GitHub,
// CommentGitHub, ScaffoldGitHub, ScaffoldQueue and Store ports it declares.
package gate

import (
	"context"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// setupGuideURL is linked from the neutral check when no runner is available.
const setupGuideURL = "https://github.com/mrkizildag/pollux-agent/blob/main/docs/guides/setup.md"

// GitHub creates check runs and inspects repository state on behalf of an
// installation.
type GitHub interface {
	// CreateCheckRun returns the ID of the check run it created.
	CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run CheckRun) (int64, error)
	// GetPullRequest returns the pull request's current base and head commits.
	GetPullRequest(ctx context.Context, installationID int64, owner, repo string, number int) (PullRequest, error)
	UpdateCheckRun(ctx context.Context, installationID int64, owner, repo string, id int64, run CheckRun) error
	// WorkflowExists reports whether the repo's default branch has the
	// pollux-agent Actions workflow.
	WorkflowExists(ctx context.Context, installationID int64, owner, repo string) (bool, error)
	// MergeBase returns the merge base commit of base and head, the commit the pull request's diff starts from.
	MergeBase(ctx context.Context, installationID int64, owner, repo, base, head string) (string, error)
	// DocsExist reports whether an entry named docs exists at ref, be it a
	// directory, a file or a submodule.
	DocsExist(ctx context.Context, installationID int64, owner, repo, ref string) (bool, error)
	// ListChangedFiles returns the files in the pull request's diff with their head-side hunk ranges.
	ListChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error)
	// ListComments returns the pull request's review comments and issue comments.
	ListComments(ctx context.Context, installationID int64, owner, repo string, number int) ([]Comment, error)
	CreateReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, c ReviewComment) (Comment, error)
	EditReviewComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error
	CreateIssueComment(ctx context.Context, installationID int64, owner, repo string, number int, body string) (Comment, error)
	EditIssueComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error
}

// Service decides and reports the pollux-agent check run for a pull request.
type Service struct {
	gh            GitHub
	store         Store
	runners       Runners
	comments      CommentGitHub
	scaffoldGH    ScaffoldGitHub
	scaffoldQueue ScaffoldQueue
	// retryBackoff is the wait before the first retry of a Collect or a post; it doubles.
	retryBackoff time.Duration
}

// NewService returns a Service that reports check runs through gh, acts on
// comments through comments, persists state through store, selects among
// runners for analysis, and writes scaffolds through scaffoldGH, scheduling
// their jobs on scaffoldQueue.
func NewService(gh GitHub, comments CommentGitHub, store Store, runners Runners, scaffoldGH ScaffoldGitHub, scaffoldQueue ScaffoldQueue) *Service {
	return &Service{gh: gh, comments: comments, store: store, runners: runners, scaffoldGH: scaffoldGH, scaffoldQueue: scaffoldQueue, retryBackoff: time.Second}
}
