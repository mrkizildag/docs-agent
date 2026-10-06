package gate_test

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

type fakeGitHub struct {
	calls          []createCheckRunCall
	updates        []updateCheckRunCall
	checkRunID     int64
	err            error
	updateErr      error
	workflowExists bool
	workflowErr    error
	noDocs         bool
	changed        []review.ChangedFile
	changedErr     error
	mergeBase      string
	mergeBaseErr   error
	mergeBaseArgs  [][2]string
	changedCalls   int
	pullRequest    gate.PullRequest
	pullRequestErr error
	reviewComments []gate.ReviewComment

	comments                 []gate.Comment
	listCalls                int
	createReview, editReview int
	createIssue, editIssue   int
	createIssueErr           error
	ops                      []string // create/edit calls in order, as "create-issue" etc.
	failReviewCreate         int      // the nth CreateReviewComment call fails once; 0 means never
	onCreateIssue            func()
	createIssueFailures      int // creates that fail before they start succeeding
	editReviewErr            error
	editIssueErr             error
}

func (f *fakeGitHub) addComment(kind gate.CommentKind, body string) gate.Comment {
	c := gate.Comment{ID: int64(len(f.comments) + 1), Mine: true, Kind: kind, Body: body}
	c.URL = fmt.Sprintf("https://gh/%s/%d", kind, c.ID)
	f.comments = append(f.comments, c)
	return c
}

func (f *fakeGitHub) edit(id int64, body string) {
	for i := range f.comments {
		if f.comments[i].ID == id {
			f.comments[i].Body = body
		}
	}
}

type createCheckRunCall struct {
	installationID int64
	owner          string
	repo           string
	run            gate.CheckRun
}

type updateCheckRunCall struct {
	id  int64
	run gate.CheckRun
}

func (f *fakeGitHub) CreateCheckRun(_ context.Context, installationID int64, owner, repo string, run gate.CheckRun) (int64, error) {
	f.calls = append(f.calls, createCheckRunCall{installationID: installationID, owner: owner, repo: repo, run: run})
	return f.checkRunID, f.err
}

func (f *fakeGitHub) UpdateCheckRun(ctx context.Context, _ int64, _, _ string, id int64, run gate.CheckRun) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update check run %d: %w", id, err)
	}
	f.updates = append(f.updates, updateCheckRunCall{id: id, run: run})
	return f.updateErr
}

func (f *fakeGitHub) GetPullRequest(_ context.Context, installationID int64, owner, repo string, number int) (gate.PullRequest, error) {
	pr := f.pullRequest
	pr.InstallationID, pr.Owner, pr.Repo, pr.Number = installationID, owner, repo, number
	return pr, f.pullRequestErr
}

func (f *fakeGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return f.workflowExists, f.workflowErr
}

func (f *fakeGitHub) MergeBase(_ context.Context, _ int64, _, _, base, head string) (string, error) {
	f.mergeBaseArgs = append(f.mergeBaseArgs, [2]string{base, head})
	return f.mergeBase, f.mergeBaseErr
}

func (f *fakeGitHub) DocsExist(context.Context, int64, string, string, string) (bool, error) {
	return !f.noDocs, nil
}

func (f *fakeGitHub) ListChangedFiles(_ context.Context, _ int64, _, _ string, _ int) ([]review.ChangedFile, error) {
	f.changedCalls++
	return f.changed, f.changedErr
}

func (f *fakeGitHub) ListComments(context.Context, int64, string, string, int) ([]gate.Comment, error) {
	f.listCalls++
	return slices.Clone(f.comments), nil
}

func (f *fakeGitHub) CreateReviewComment(_ context.Context, _ int64, _, _ string, _ int, c gate.ReviewComment) (gate.Comment, error) {
	f.createReview++
	f.ops = append(f.ops, "create-review")
	if f.createReview == f.failReviewCreate {
		return gate.Comment{}, errors.New("create review comment failed")
	}
	f.reviewComments = append(f.reviewComments, c)
	return f.addComment(gate.CommentKindReview, c.Body), nil
}

func (f *fakeGitHub) EditReviewComment(_ context.Context, _ int64, _, _ string, id int64, body string) error {
	f.editReview++
	if f.editReviewErr != nil {
		return f.editReviewErr
	}
	f.edit(id, body)
	return nil
}

func (f *fakeGitHub) CreateIssueComment(_ context.Context, _ int64, _, _ string, _ int, body string) (gate.Comment, error) {
	f.createIssue++
	f.ops = append(f.ops, "create-issue")
	if f.onCreateIssue != nil {
		f.onCreateIssue()
	}
	if f.createIssueErr != nil {
		return gate.Comment{}, f.createIssueErr
	}
	if f.createIssueFailures > 0 {
		f.createIssueFailures--
		return gate.Comment{}, errors.New("transient")
	}
	return f.addComment(gate.CommentKindIssue, body), nil
}

func (f *fakeGitHub) EditIssueComment(_ context.Context, _ int64, _, _ string, id int64, body string) error {
	f.editIssue++
	f.ops = append(f.ops, "edit-issue")
	if f.editIssueErr != nil {
		return f.editIssueErr
	}
	f.edit(id, body)
	return nil
}

type fakeRunner struct {
	calls      []review.Request
	started    review.Started
	err        error
	collected  []review.Completion
	result     review.Result
	collectErr error
	failFirst  int // Collect returns collectErr only for the first failFirst calls; 0 means always
	onStart    func()
}

func (f *fakeRunner) Collect(_ context.Context, c review.Completion) (review.Result, error) {
	f.collected = append(f.collected, c)
	if f.failFirst > 0 && len(f.collected) > f.failFirst {
		return f.result, nil
	}
	return f.result, f.collectErr
}

func (f *fakeRunner) Start(_ context.Context, req review.Request) (review.Started, error) {
	f.calls = append(f.calls, req)
	if f.onStart != nil {
		f.onStart()
	}
	return f.started, f.err
}

func (f *fakeRunner) StartScaffold(context.Context, review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	return nil, errors.New("fakeRunner does not scaffold")
}

func (f *fakeRunner) CollectScaffold(context.Context, review.Completion) (review.Scaffold, error) {
	return review.Scaffold{}, errors.New("fakeRunner does not scaffold")
}

type fakeStore struct {
	loadCalls   []loadPRCall
	saveCalls   []gate.PRState
	loadErr     error
	saveErr     error
	stored      gate.PRState
	saved       *gate.PRState
	saveCtxErrs []error
	live        bool // SavePR also replaces stored, as a real store would
}

type loadPRCall struct {
	owner  string
	repo   string
	number int
}

func (f *fakeStore) LoadPR(_ context.Context, owner, repo string, number int) (gate.PRState, error) {
	f.loadCalls = append(f.loadCalls, loadPRCall{owner: owner, repo: repo, number: number})
	if f.loadErr != nil {
		return gate.PRState{}, f.loadErr
	}
	if f.stored.Number == number {
		return f.stored, nil
	}
	if f.saved != nil {
		return *f.saved, nil
	}
	return gate.PRState{Owner: owner, Repo: repo, Number: number}, nil
}

func (f *fakeStore) PRForRun(context.Context, string, string, int64) (int, bool, error) {
	return 0, false, nil
}

func (f *fakeStore) LoadScaffold(_ context.Context, owner, repo string) (gate.ScaffoldState, error) {
	return gate.ScaffoldState{Owner: owner, Repo: repo, Phase: gate.ScaffoldIdle}, nil
}

func (f *fakeStore) SaveScaffold(context.Context, gate.ScaffoldState) error { return nil }

func (f *fakeStore) ScaffoldForRun(context.Context, string, string, int64) (bool, error) {
	return false, nil
}

func (f *fakeStore) RequestScaffold(_ context.Context, installationID int64, owner, repo string, _ gate.ScaffoldWaiter) (gate.ScaffoldState, error) {
	return gate.ScaffoldState{Owner: owner, Repo: repo, InstallationID: installationID, Phase: gate.ScaffoldIdle}, nil
}

func (f *fakeStore) UnlinkedScaffoldWaiters(context.Context, string, string) ([]gate.ScaffoldWaiter, error) {
	return nil, nil
}

func (f *fakeStore) MarkScaffoldWaiterLinked(context.Context, string, string, int64) error {
	return nil
}

func (f *fakeStore) SavePR(ctx context.Context, state gate.PRState) error {
	f.saveCtxErrs = append(f.saveCtxErrs, ctx.Err())
	f.saveCalls = append(f.saveCalls, state)
	f.saved = &state
	if f.live {
		f.stored = state
	}
	return f.saveErr
}

func testPR() gate.PullRequest {
	return gate.PullRequest{
		InstallationID: 42,
		Owner:          "acme",
		Repo:           "widgets",
		Number:         7,
		BaseSHA:        "base123",
		HeadSHA:        "abc123",
	}
}
