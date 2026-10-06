// Package gate is the domain of the pollux-agent GitHub App: what check run a
// pull request gets, which proposal and summary comments it carries, and what
// acting on those comments does. HandlePullRequest, HandleRunCompleted and
// HandleDeadline report the check run; HandleComment applies proposals to the
// head branch (Apply), waives the check (skip) and re-runs the analysis;
// HandleScaffold and its siblings write and propose the docs scaffold of a repo
// that has no docs/ folder. State transitions are pure functions over PRState and
// ScaffoldState; the Service performs their I/O through the GitHub,
// ScaffoldQueue and Store ports it declares.
package gate

import (
	"context"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// setupGuideURL is linked from the neutral check when no runner is available.
const setupGuideURL = "https://github.com/mrkizildag/pollux-agent/blob/main/docs/guides/setup.md"

// GitHub is everything the domain needs from GitHub on behalf of an
// installation: check runs, pull request state and comments, repository files
// and commits, and the branches and pull requests of a scaffold.
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
	// Permission reports whether user may write to the repository.
	Permission(ctx context.Context, installationID int64, owner, repo, user string) (canWrite bool, err error)
	// FileAtRef returns the file at ref; ok is false when it does not exist there.
	// A file it will not read for its size is an error wrapping
	// review.ErrFileTooLarge, never ok=false.
	FileAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (content []byte, ok bool, err error)
	// CommitFiles commits files on top of parentSHA and moves branch to the new
	// commit without forcing. It returns ErrBranchMoved when branch is no longer at parentSHA.
	CommitFiles(ctx context.Context, installationID int64, owner, repo, branch, parentSHA string, files []FileChange, message string) (sha string, err error)
	// BranchCommit returns the commit branch points at.
	BranchCommit(ctx context.Context, installationID int64, owner, repo, branch string) (Commit, error)
	// CommitAt returns the commit sha.
	CommitAt(ctx context.Context, installationID int64, owner, repo, sha string) (Commit, error)
	// React adds reaction to comment id of kind and returns the reaction's ID;
	// adding one that already exists returns the existing ID.
	React(ctx context.Context, installationID int64, owner, repo string, kind CommentKind, id int64, reaction Reaction) (int64, error)
	// Unreact removes reaction reactionID from comment id of kind.
	Unreact(ctx context.Context, installationID int64, owner, repo string, kind CommentKind, id, reactionID int64) error
	// ReplyToReviewComment posts a reply in the thread of review comment inReplyTo.
	ReplyToReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, inReplyTo int64, body string) (Comment, error)
	// DefaultBranch returns the default branch's name and tip commit.
	DefaultBranch(ctx context.Context, installationID int64, owner, repo string) (name, sha string, err error)
	// CreateBranch creates branch at sha; it returns ErrBranchExists when branch exists.
	CreateBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error
	// ResetBranch force-moves an existing branch to sha.
	ResetBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error
	CreatePullRequest(ctx context.Context, installationID int64, owner, repo string, pr NewPullRequest) (ScaffoldPR, error)
	// FindPullRequest returns the pull request opened from branch, preferring the bot's (open first), then an open one, over the rest.
	FindPullRequest(ctx context.Context, installationID int64, owner, repo, branch string) (pr ScaffoldPR, ok bool, err error)
}

// Service decides and reports the pollux-agent check run for a pull request.
type Service struct {
	gh            GitHub
	store         Store
	runners       Runners
	scaffoldQueue ScaffoldQueue
	// retryBackoff is the wait before the first retry of a Collect or a post; it doubles.
	retryBackoff time.Duration
}

// NewService returns a Service that reaches GitHub through gh, persists state
// through store, selects among runners for analysis, and schedules scaffold
// jobs on scaffoldQueue. Every dependency is required.
func NewService(gh GitHub, store Store, runners Runners, scaffoldQueue ScaffoldQueue) *Service {
	return &Service{gh: gh, store: store, runners: runners, scaffoldQueue: scaffoldQueue, retryBackoff: time.Second}
}
