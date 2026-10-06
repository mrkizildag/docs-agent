package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// PullRequest is the subset of a GitHub pull request the gate needs.
type PullRequest struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	BaseSHA        string
	HeadSHA        string
	HeadRef        string // branch name of the head
	Fork           bool   // head lives in another repository, so Apply cannot push to it
	Open           bool   // false once the pull request is closed or merged
}

// Conclusion is a GitHub check run conclusion.
type Conclusion string

const (
	ConclusionSuccess        Conclusion = "success"
	ConclusionNeutral        Conclusion = "neutral"
	ConclusionActionRequired Conclusion = "action_required"
)

// Status is a GitHub check run status.
type Status string

const (
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// CheckRun is a GitHub check run; Conclusion is empty unless Status is completed.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Status     Status
	Conclusion Conclusion
	Title      string
	Summary    string
}

// CommentKind says which GitHub comment API a Comment lives in; the two have
// separate ID spaces and edit endpoints.
type CommentKind string

const (
	CommentKindReview CommentKind = "review"
	CommentKindIssue  CommentKind = "issue"
)

// Comment is a comment on a pull request. Path, StartLine and Line locate a
// review comment and are zero for issue comments and for review comments GitHub
// no longer anchors. Mine is true when the App's bot user wrote the comment;
// only those are ever adopted or edited.
type Comment struct {
	ID        int64
	Mine      bool
	Kind      CommentKind
	URL       string
	Body      string
	Path      string
	StartLine int
	Line      int
}

// ReviewComment is a new review comment on the right side of a file in the
// head commit. StartLine 0 means a single-line comment on Line.
type ReviewComment struct {
	CommitSHA string
	Path      string
	StartLine int
	Line      int
	Body      string
}

// CheckName is the name of the check run pollux reports on every PR.
const CheckName = "pollux-agent"

// WorkflowPath is the target-repo workflow whose presence selects the Actions
// runner and whose completion carries its result.
const WorkflowPath = ".github/workflows/pollux-agent.yml"

// PRState is what the gate remembers about one pull request between events.
type PRState struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string // head commit the gate last reported a check run for; "" if never
	CheckRunID     int64  // check run reported for HeadSHA; 0 if none
	HeadRef        string // branch Apply commits to
	ProposalsSHA   string // head the proposals were last computed at; "" if never
	Fork           bool   // head lives in another repository; Apply is not offered
	Run            *AwaitingRun

	PendingSkip  *SkipAsk      // skip waiting for its reason; nil if none
	Skip         *Skip         // active skip; nil if none
	PendingApply *PendingApply // Apply commit being created; nil if none

	SummaryCommentID int64  // 0 until the summary comment is created
	FailureCause     string // why the last analysis failed, shown in the summary; "" when it did not
	Proposals        []ProposalState
}

// PendingApply is an Apply commit that may exist on GitHub before its proposals
// are saved as applied: the proposals, the message and the parent it was built on.
type PendingApply struct {
	IDs     []string
	Message string
	Parent  string
}

// SkipScope is how long a skip passes the check.
type SkipScope string

const (
	SkipCommit SkipScope = "commit" // the head the skip was made at only
	SkipPR     SkipScope = "pr"     // every later push to the pull request
)

// SkipAsk is a skip the bot asked User for a reason for; User's next comment
// on the pull request becomes the reason.
type SkipAsk struct {
	User  string
	Scope SkipScope
}

// Skip waives the docs check. HeadSHA is the head it was made at.
type Skip struct {
	User    string
	Scope   SkipScope
	Reason  string
	HeadSHA string
}

// AwaitingRun is the external analysis run whose result will conclude the
// check run; PRState.Run is nil when none is awaited.
type AwaitingRun struct {
	RunID    int64
	Nonce    string
	Deadline time.Time
}

// RunCompleted reports that an external analysis run finished.
type RunCompleted struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	RunID          int64
	Conclusion     string
}

// pullRequest is the pull request state describes, at the head it last reported.
func (s PRState) pullRequest() PullRequest {
	return PullRequest{InstallationID: s.InstallationID, Owner: s.Owner, Repo: s.Repo, Number: s.Number, HeadSHA: s.HeadSHA, HeadRef: s.HeadRef, Fork: s.Fork}
}

// PRRef identifies a pull request.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// RerunRequest asks for a fresh analysis of a pull request's current head.
// SummaryCommentID is the comment whose Re-run box was ticked; 0 means any
// request is accepted.
type RerunRequest struct {
	InstallationID   int64
	PRRef            PRRef
	SummaryCommentID int64
}

// OverdueRun is an awaited run whose deadline has passed, as found by a sweep.
type OverdueRun struct {
	PRRef         // Number is 0 for a scaffold
	Scaffold bool // the run writes the repo's scaffold
	Nonce    string
	Deadline time.Time
}

// ProposalStatus is whether a proposal still applies to the latest head.
type ProposalStatus string

const (
	ProposalOpen     ProposalStatus = "open"
	ProposalOutdated ProposalStatus = "outdated"
	ProposalApplied  ProposalStatus = "applied"
)

// ProposalState is one proposal's review comment as the gate remembers it.
type ProposalState struct {
	ID         string // see ProposalID
	DocPath    string
	Section    string
	CommentID  int64 // 0 until the review comment is created
	CommentURL string
	State      ProposalStatus

	Content    string // the section as proposed, heading included
	Original   string // the section text Content replaces; "" for a new doc
	IndexEntry string
	AppliedSHA string // commit that applied it; "" unless Applied
	ReplyID    int64  // reply posted under the comment for AppliedSHA; 0 if none
}

// ProposalID is the stable identity of a proposal across re-runs: a short hash
// of its doc path and normalized section heading (path alone for a new doc).
func ProposalID(docPath, section string) string {
	section = review.NormalizeSection(section)
	sum := sha256.Sum256([]byte(docPath + "\x00" + section))
	return hex.EncodeToString(sum[:6])
}

// Store persists PRState.
type Store interface {
	// LoadPR returns the zero-HeadSHA state (identity fields filled from the args) for a PR never saved.
	LoadPR(ctx context.Context, owner, repo string, number int) (PRState, error)
	SavePR(ctx context.Context, state PRState) error
	// PRForRun returns the pull request an external run was dispatched for.
	PRForRun(ctx context.Context, owner, repo string, runID int64) (number int, ok bool, err error)
	// LoadScaffold returns the Idle zero-Attempt state (identity fields filled from the args) for a repo never saved.
	LoadScaffold(ctx context.Context, owner, repo string) (ScaffoldState, error)
	SaveScaffold(ctx context.Context, state ScaffoldState) error
	// ScaffoldForRun reports whether the repo's scaffold awaits the external run runID.
	ScaffoldForRun(ctx context.Context, owner, repo string, runID int64) (bool, error)
	// RequestScaffold atomically creates the repo's Idle scaffold state if it has
	// none and records waiter, returning the state as it stands. Existing state is
	// untouched except that its installation ID becomes installationID.
	RequestScaffold(ctx context.Context, installationID int64, owner, repo string, waiter ScaffoldWaiter) (ScaffoldState, error)
	// UnlinkedScaffoldWaiters returns the repo's recorded check runs not yet marked linked, oldest first.
	UnlinkedScaffoldWaiters(ctx context.Context, owner, repo string) ([]ScaffoldWaiter, error)
	// MarkScaffoldWaiterLinked records that the check run no longer waits for the scaffold.
	MarkScaffoldWaiterLinked(ctx context.Context, owner, repo string, checkRunID int64) error
}
