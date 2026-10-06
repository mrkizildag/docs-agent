// Package gatetest is an in-memory gate.GitHub for tests. It is stateful: what
// the service writes (check runs, comments, reactions, commits, branches, pull
// requests) is what the service reads back, and tests assert on that state.
package gatetest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// Call is one call into the fake, passed to Before. N counts the calls of that
// method from 1; ID is the check run or comment the call names, 0 if none.
type Call struct {
	Method         string
	N              int
	InstallationID int64
	ID             int64
}

// CheckRun is a check run the fake knows: as created, then each update. A run
// updated without ever being created here (the test seeded its ID in a store)
// has a zero Created.
type CheckRun struct {
	ID      int64
	Created gate.CheckRun
	Updates []gate.CheckRun
}

// Latest is the check run as GitHub shows it now.
func (c CheckRun) Latest() gate.CheckRun {
	if n := len(c.Updates); n > 0 {
		return c.Updates[n-1]
	}
	return c.Created
}

// Commit is a commit made through CommitFiles.
type Commit struct {
	Branch  string
	Parent  string
	Message string
	SHA     string
	Files   []gate.FileChange
}

// Reply is a reply posted under a review comment.
type Reply struct {
	To   int64
	Body string
}

// GitHub implements gate.GitHub. The zero value is an empty repository with
// docs/, no workflow, and no write access for anyone; set the exported fields
// before the first call.
type GitHub struct {
	// PullRequest is returned by GetPullRequest, with the identity of the request filled in.
	PullRequest gate.PullRequest
	// Workflow is whether WorkflowExists reports the Actions workflow.
	Workflow bool
	// NoDocs makes DocsExist report no docs/ at any ref.
	NoDocs bool
	// Changed is the pull request's changed files.
	Changed []review.ChangedFile
	// MergeBaseSHA is what MergeBase returns.
	MergeBaseSHA string
	// Files are the repository files by path, at every ref.
	Files map[string]string
	// FileErrs makes FileAtRef of a path fail with its error.
	FileErrs map[string]error
	// CanWrite is what Permission reports for every user.
	CanWrite bool
	// Branches are the branch tips by name. BranchCommit of an unknown branch is the zero Commit.
	Branches map[string]gate.Commit
	// KnownCommits are the commits CommitAt finds by SHA, besides the branch tips and the commits made through CommitFiles.
	KnownCommits map[string]gate.Commit
	// CommitSHA is the SHA of the next commit CommitFiles makes; "abcdef1234567" when empty.
	CommitSHA string
	// NextCheckRunID is the ID the next created check run gets; 1 when zero.
	NextCheckRunID int64
	// ExistingPR is the pull request FindPullRequest reports for any branch, if any.
	ExistingPR *gate.ScaffoldPR
	// Fail makes every call of a method fail with the error, by method name.
	Fail map[string]error
	// Before runs before each call is applied, outside the fake's lock; an
	// error fails the call, and it may block. It is the hook for failing the
	// nth call, a call naming one ID, or holding a call while the test acts.
	Before func(Call) error

	mu          sync.Mutex
	counts      map[string]int
	calls       []Call
	checkRuns   []CheckRun
	comments    []gate.Comment
	reviewNew   []gate.ReviewComment
	replies     []Reply
	reactions   map[commentKey]map[int64]gate.Reaction
	reactionLog []gate.Reaction
	nextReact   int64
	commits     []Commit
	resets      []string
	newPRs      []gate.NewPullRequest
	mergeBases  [][2]string
}

type commentKey struct {
	kind gate.CommentKind
	id   int64
}

var _ gate.GitHub = (*GitHub)(nil)

// AddComment seeds a comment of the bot, as if it had been posted earlier. It
// counts as no call.
func (g *GitHub) AddComment(kind gate.CommentKind, body string) gate.Comment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addComment(kind, body)
}

// SetCommentBody seeds an edit of comment id, as if a user had made it. It
// counts as no call.
func (g *GitHub) SetCommentBody(id int64, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setBody(id, body)
}

// CheckRuns returns the check runs in the order they were first seen.
func (g *GitHub) CheckRuns() []CheckRun {
	g.mu.Lock()
	defer g.mu.Unlock()
	runs := slices.Clone(g.checkRuns)
	for i := range runs {
		runs[i].Updates = slices.Clone(runs[i].Updates)
	}
	return runs
}

// Comments returns the pull request's comments as they stand now, bodies edited.
func (g *GitHub) Comments() []gate.Comment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.comments)
}

// ReviewComments returns the review comments created, as requested.
func (g *GitHub) ReviewComments() []gate.ReviewComment {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reviewNew)
}

// Replies returns the replies posted under review comments.
func (g *GitHub) Replies() []Reply {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.replies)
}

// Reactions returns the reactions now on a comment, oldest first.
func (g *GitHub) Reactions(kind gate.CommentKind, id int64) []gate.Reaction {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.reactions[commentKey{kind, id}]
	ids := make([]int64, 0, len(m))
	for rid := range m {
		ids = append(ids, rid)
	}
	slices.Sort(ids)
	got := make([]gate.Reaction, 0, len(ids))
	for _, rid := range ids {
		got = append(got, m[rid])
	}
	return got
}

// ReactionLog returns every reaction added, in order, whichever comment it is on.
func (g *GitHub) ReactionLog() []gate.Reaction {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reactionLog)
}

// Committed returns the commits made through CommitFiles.
func (g *GitHub) Committed() []Commit {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.commits)
}

// Resets returns "branch@sha" for every ResetBranch.
func (g *GitHub) Resets() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.resets)
}

// PullRequests returns the pull requests opened through CreatePullRequest.
func (g *GitHub) PullRequests() []gate.NewPullRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.newPRs)
}

// MergeBaseArgs returns the (base, head) of every MergeBase call.
func (g *GitHub) MergeBaseArgs() [][2]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.mergeBases)
}

// Calls returns every call in order, failed ones included.
func (g *GitHub) Calls() []Call {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// CallCount is how many times method was called, failed calls included.
func (g *GitHub) CallCount(method string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counts[method]
}

// begin records the call, then returns the error the test scripted for it.
func (g *GitHub) begin(ctx context.Context, method string, installationID, id int64) error {
	g.mu.Lock()
	if g.counts == nil {
		g.counts = map[string]int{}
	}
	g.counts[method]++
	call := Call{Method: method, N: g.counts[method], InstallationID: installationID, ID: id}
	g.calls = append(g.calls, call)
	fail, before := g.Fail[method], g.Before
	g.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	if fail != nil {
		return fail
	}
	if before != nil {
		return before(call)
	}
	return nil
}

func (g *GitHub) addComment(kind gate.CommentKind, body string) gate.Comment {
	c := gate.Comment{ID: int64(len(g.comments) + 1), Mine: true, Kind: kind, Body: body}
	c.URL = fmt.Sprintf("https://gh/%s/%d", kind, c.ID)
	g.comments = append(g.comments, c)
	return c
}

func (g *GitHub) setBody(id int64, body string) {
	for i := range g.comments {
		if g.comments[i].ID == id {
			g.comments[i].Body = body
		}
	}
}

func (g *GitHub) checkRun(id int64) *CheckRun {
	for i := range g.checkRuns {
		if g.checkRuns[i].ID == id {
			return &g.checkRuns[i]
		}
	}
	g.checkRuns = append(g.checkRuns, CheckRun{ID: id})
	return &g.checkRuns[len(g.checkRuns)-1]
}

func (g *GitHub) CreateCheckRun(ctx context.Context, installationID int64, _, _ string, run gate.CheckRun) (int64, error) {
	if err := g.begin(ctx, "CreateCheckRun", installationID, 0); err != nil {
		return 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.NextCheckRunID == 0 {
		g.NextCheckRunID = 1
	}
	id := g.NextCheckRunID
	g.NextCheckRunID++
	g.checkRun(id).Created = run
	return id, nil
}

func (g *GitHub) UpdateCheckRun(ctx context.Context, installationID int64, _, _ string, id int64, run gate.CheckRun) error {
	if err := g.begin(ctx, "UpdateCheckRun", installationID, id); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cr := g.checkRun(id)
	cr.Updates = append(cr.Updates, run)
	return nil
}

func (g *GitHub) GetPullRequest(ctx context.Context, installationID int64, owner, repo string, number int) (gate.PullRequest, error) {
	if err := g.begin(ctx, "GetPullRequest", installationID, 0); err != nil {
		return gate.PullRequest{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := g.PullRequest
	pr.InstallationID, pr.Owner, pr.Repo, pr.Number = installationID, owner, repo, number
	return pr, nil
}

func (g *GitHub) WorkflowExists(ctx context.Context, installationID int64, _, _ string) (bool, error) {
	if err := g.begin(ctx, "WorkflowExists", installationID, 0); err != nil {
		return false, err
	}
	return g.Workflow, nil
}

func (g *GitHub) MergeBase(ctx context.Context, installationID int64, _, _, base, head string) (string, error) {
	if err := g.begin(ctx, "MergeBase", installationID, 0); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mergeBases = append(g.mergeBases, [2]string{base, head})
	return g.MergeBaseSHA, nil
}

func (g *GitHub) DocsExist(ctx context.Context, installationID int64, _, _, _ string) (bool, error) {
	if err := g.begin(ctx, "DocsExist", installationID, 0); err != nil {
		return false, err
	}
	return !g.NoDocs, nil
}

func (g *GitHub) ListChangedFiles(ctx context.Context, installationID int64, _, _ string, _ int) ([]review.ChangedFile, error) {
	if err := g.begin(ctx, "ListChangedFiles", installationID, 0); err != nil {
		return nil, err
	}
	return g.Changed, nil
}

func (g *GitHub) ListComments(ctx context.Context, installationID int64, _, _ string, _ int) ([]gate.Comment, error) {
	if err := g.begin(ctx, "ListComments", installationID, 0); err != nil {
		return nil, err
	}
	return g.Comments(), nil
}

func (g *GitHub) CreateReviewComment(ctx context.Context, installationID int64, _, _ string, _ int, c gate.ReviewComment) (gate.Comment, error) {
	if err := g.begin(ctx, "CreateReviewComment", installationID, 0); err != nil {
		return gate.Comment{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reviewNew = append(g.reviewNew, c)
	return g.addComment(gate.CommentKindReview, c.Body), nil
}

func (g *GitHub) EditReviewComment(ctx context.Context, installationID int64, _, _ string, id int64, body string) error {
	return g.edit(ctx, "EditReviewComment", installationID, id, body)
}

func (g *GitHub) CreateIssueComment(ctx context.Context, installationID int64, _, _ string, _ int, body string) (gate.Comment, error) {
	if err := g.begin(ctx, "CreateIssueComment", installationID, 0); err != nil {
		return gate.Comment{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.addComment(gate.CommentKindIssue, body), nil
}

func (g *GitHub) EditIssueComment(ctx context.Context, installationID int64, _, _ string, id int64, body string) error {
	return g.edit(ctx, "EditIssueComment", installationID, id, body)
}

func (g *GitHub) edit(ctx context.Context, method string, installationID, id int64, body string) error {
	if err := g.begin(ctx, method, installationID, id); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.setBody(id, body)
	return nil
}

func (g *GitHub) Permission(ctx context.Context, installationID int64, _, _, _ string) (bool, error) {
	if err := g.begin(ctx, "Permission", installationID, 0); err != nil {
		return false, err
	}
	return g.CanWrite, nil
}

func (g *GitHub) FileAtRef(ctx context.Context, installationID int64, _, _, path, _ string) ([]byte, bool, error) {
	if err := g.begin(ctx, "FileAtRef", installationID, 0); err != nil {
		return nil, false, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.FileErrs[path]; err != nil {
		return nil, false, err
	}
	content, ok := g.Files[path]
	return []byte(content), ok, nil
}

// CommitFiles records the commit and moves branch to it; it does not check parentSHA.
func (g *GitHub) CommitFiles(ctx context.Context, installationID int64, _, _, branch, parentSHA string, files []gate.FileChange, message string) (string, error) {
	if err := g.begin(ctx, "CommitFiles", installationID, 0); err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	sha := g.CommitSHA
	if sha == "" {
		sha = "abcdef1234567"
	}
	g.commits = append(g.commits, Commit{Branch: branch, Parent: parentSHA, Message: message, SHA: sha, Files: slices.Clone(files)})
	tip := gate.Commit{SHA: sha, Message: message, Parents: []string{parentSHA}, Mine: true}
	if g.KnownCommits == nil {
		g.KnownCommits = map[string]gate.Commit{}
	}
	g.KnownCommits[sha] = tip
	g.setBranch(branch, tip)
	return sha, nil
}

func (g *GitHub) BranchCommit(ctx context.Context, installationID int64, _, _, branch string) (gate.Commit, error) {
	if err := g.begin(ctx, "BranchCommit", installationID, 0); err != nil {
		return gate.Commit{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Branches[branch], nil
}

func (g *GitHub) CommitAt(ctx context.Context, installationID int64, _, _, sha string) (gate.Commit, error) {
	if err := g.begin(ctx, "CommitAt", installationID, 0); err != nil {
		return gate.Commit{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.KnownCommits[sha]; ok {
		return c, nil
	}
	for _, c := range g.Branches {
		if c.SHA == sha {
			return c, nil
		}
	}
	return gate.Commit{}, nil
}

func (g *GitHub) React(ctx context.Context, installationID int64, _, _ string, kind gate.CommentKind, id int64, reaction gate.Reaction) (int64, error) {
	if err := g.begin(ctx, "React", installationID, id); err != nil {
		return 0, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := commentKey{kind, id}
	for rid, r := range g.reactions[key] {
		if r == reaction {
			return rid, nil
		}
	}
	if g.reactions == nil {
		g.reactions = map[commentKey]map[int64]gate.Reaction{}
	}
	if g.reactions[key] == nil {
		g.reactions[key] = map[int64]gate.Reaction{}
	}
	g.nextReact++
	g.reactions[key][g.nextReact] = reaction
	g.reactionLog = append(g.reactionLog, reaction)
	return g.nextReact, nil
}

func (g *GitHub) Unreact(ctx context.Context, installationID int64, _, _ string, kind gate.CommentKind, id, reactionID int64) error {
	if err := g.begin(ctx, "Unreact", installationID, id); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.reactions[commentKey{kind, id}], reactionID)
	return nil
}

// ReplyToReviewComment adds the reply as a review comment of the bot.
func (g *GitHub) ReplyToReviewComment(ctx context.Context, installationID int64, _, _ string, _ int, inReplyTo int64, body string) (gate.Comment, error) {
	if err := g.begin(ctx, "ReplyToReviewComment", installationID, inReplyTo); err != nil {
		return gate.Comment{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.replies = append(g.replies, Reply{To: inReplyTo, Body: body})
	return g.addComment(gate.CommentKindReview, body), nil
}

// DefaultBranch is always "main" at "tip".
func (g *GitHub) DefaultBranch(ctx context.Context, installationID int64, _, _ string) (string, string, error) {
	if err := g.begin(ctx, "DefaultBranch", installationID, 0); err != nil {
		return "", "", err
	}
	return "main", "tip", nil
}

func (g *GitHub) CreateBranch(ctx context.Context, installationID int64, _, _, branch, sha string) error {
	if err := g.begin(ctx, "CreateBranch", installationID, 0); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.Branches[branch]; ok {
		return gate.ErrBranchExists
	}
	g.setBranch(branch, gate.Commit{SHA: sha})
	return nil
}

func (g *GitHub) ResetBranch(ctx context.Context, installationID int64, _, _, branch, sha string) error {
	if err := g.begin(ctx, "ResetBranch", installationID, 0); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resets = append(g.resets, branch+"@"+sha)
	g.setBranch(branch, gate.Commit{SHA: sha})
	return nil
}

func (g *GitHub) setBranch(branch string, tip gate.Commit) {
	if g.Branches == nil {
		g.Branches = map[string]gate.Commit{}
	}
	g.Branches[branch] = tip
}

// CreatePullRequest numbers the pull requests from 9.
func (g *GitHub) CreatePullRequest(ctx context.Context, installationID int64, _, _ string, pr gate.NewPullRequest) (gate.ScaffoldPR, error) {
	if err := g.begin(ctx, "CreatePullRequest", installationID, 0); err != nil {
		return gate.ScaffoldPR{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.newPRs = append(g.newPRs, pr)
	n := 8 + len(g.newPRs)
	return gate.ScaffoldPR{Number: n, URL: fmt.Sprintf("https://gh/pull/%d", n)}, nil
}

func (g *GitHub) FindPullRequest(ctx context.Context, installationID int64, _, _, _ string) (gate.ScaffoldPR, bool, error) {
	if err := g.begin(ctx, "FindPullRequest", installationID, 0); err != nil {
		return gate.ScaffoldPR{}, false, err
	}
	if g.ExistingPR == nil {
		return gate.ScaffoldPR{}, false, nil
	}
	return *g.ExistingPR, true, nil
}
