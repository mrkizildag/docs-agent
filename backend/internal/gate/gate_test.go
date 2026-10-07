package gate_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

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
	resolveErr               error
	resolved                 []int64
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

func (f *fakeGitHub) ResolveReviewThread(_ context.Context, _ int64, _, _ string, _ int, commentID int64) error {
	f.ops = append(f.ops, "resolve-review")
	f.resolved = append(f.resolved, commentID)
	return f.resolveErr
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
	onSave      func()
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
	if f.onSave != nil {
		f.onSave()
	}
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

func TestHandlePullRequestRunnerSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		workflowExists bool
		actions        *fakeRunner
		server         *fakeRunner
		wantActions    bool
		wantServer     bool
	}{
		{
			name:           "workflow present calls actions runner even if server set",
			workflowExists: true,
			actions:        &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}},
			server:         &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}},
			wantActions:    true,
		},
		{
			name:           "no workflow uses server runner",
			workflowExists: false,
			server:         &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}},
			wantServer:     true,
		},
		{
			name:           "neither runner available reports neutral",
			workflowExists: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{workflowExists: tc.workflowExists}
			runners := gate.Runners{}
			if tc.actions != nil {
				runners.Actions = tc.actions
			}
			if tc.server != nil {
				runners.Server = tc.server
			}

			svc := gate.NewService(gh, nil, &fakeStore{}, runners, nil, nil)
			pr := testPR()
			if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
				t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
			}

			if tc.wantActions && len(tc.actions.calls) != 1 {
				t.Errorf("actions runner calls = %d, want 1", len(tc.actions.calls))
			}
			if tc.wantServer && len(tc.server.calls) != 1 {
				t.Errorf("server runner calls = %d, want 1", len(tc.server.calls))
			}
			if !tc.wantActions && tc.actions != nil && len(tc.actions.calls) != 0 {
				t.Errorf("actions runner calls = %d, want 0", len(tc.actions.calls))
			}
			if !tc.wantServer && tc.server != nil && len(tc.server.calls) != 0 {
				t.Errorf("server runner calls = %d, want 0", len(tc.server.calls))
			}

			if len(gh.calls) != 1 {
				t.Fatalf("CreateCheckRun calls = %d, want 1", len(gh.calls))
			}

			if !tc.wantActions && !tc.wantServer {
				got := gh.calls[0].run
				if got.Conclusion != gate.ConclusionNeutral || got.Title != "No analysis runner configured" ||
					!strings.Contains(got.Summary, "docs/guides/setup.md") {
					t.Errorf("check run = %+v, want neutral no-runner-configured", got)
				}
			}
		})
	}
}

func TestHandlePullRequestNoImpact(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: false}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "docs already cover this"}}}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}

	want := gate.CheckRun{
		Name:       "pollux-agent",
		HeadSHA:    "abc123",
		Status:     gate.StatusCompleted,
		Conclusion: gate.ConclusionSuccess,
		Title:      "No doc impact",
		Summary:    "docs already cover this",
	}
	if diff := cmp.Diff(want, gh.updates[0].run); diff != "" {
		t.Errorf("check run (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestProposals(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: false}
	proposals := review.Proposals{
		{DocPath: "docs/a.md", Reason: "endpoint changed"},
		{DocPath: "docs/b.md", Reason: "config added"},
	}
	runner := &fakeRunner{started: review.Result{Verdict: proposals}}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}

	got := gh.updates[0].run
	if got.Conclusion != gate.ConclusionActionRequired || got.Title != "Docs need updating" {
		t.Errorf("check run = %+v, want action_required Docs need updating", got)
	}
	for _, p := range proposals {
		if !strings.Contains(got.Summary, p.DocPath) || !strings.Contains(got.Summary, p.Reason) {
			t.Errorf("summary = %q, want it to mention %q and %q", got.Summary, p.DocPath, p.Reason)
		}
	}
}

func TestHandlePullRequestActionsResultConcludesWithoutAnArmedRun(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: true, checkRunID: 555}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{{DocPath: "docs/a.md", Reason: "restored"}}}}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if len(gh.updates) != 1 || gh.updates[0].id != 555 || gh.updates[0].run.Conclusion != gate.ConclusionActionRequired {
		t.Errorf("UpdateCheckRun calls = %+v, want one action_required conclusion of check run 555", gh.updates)
	}
	if got := store.saveCalls[len(store.saveCalls)-1]; got.Run != nil {
		t.Errorf("final saved Run = %+v, want nil so the deadline sweep has nothing to conclude", got.Run)
	}
}

func TestHandlePullRequestWorkflowExistsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{workflowErr: wantErr}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: &fakeRunner{}}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}

func TestHandlePullRequestMergeBase(t *testing.T) {
	t.Parallel()

	pr := testPR()
	gh := &fakeGitHub{mergeBase: "mb1"}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if diff := cmp.Diff([][2]string{{pr.BaseSHA, pr.HeadSHA}}, gh.mergeBaseArgs); diff != "" {
		t.Errorf("MergeBase (base, head) args (-want +got):\n%s", diff)
	}
	if len(runner.calls) != 1 || runner.calls[0].BaseSHA != "mb1" || runner.calls[0].HeadSHA != pr.HeadSHA {
		t.Errorf("runner calls = %+v, want one with BaseSHA mb1 and HeadSHA %s", runner.calls, pr.HeadSHA)
	}
}

func TestHandlePullRequestMergeBaseError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{mergeBaseErr: wantErr}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.calls) != 1 || len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionNeutral {
		t.Errorf("check run calls = %+v, updates = %+v, want one created and concluded neutral", gh.calls, gh.updates)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %d, want 0", len(runner.calls))
	}
}

func TestHandlePullRequestPassesChangedFiles(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 3, End: 9}}, Patch: "@@ -1 +3,7 @@"}}
	gh := &fakeGitHub{changed: changed}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.calls))
	}
	if diff := cmp.Diff(changed, runner.calls[0].ChangedFiles); diff != "" {
		t.Errorf("Request.ChangedFiles (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestListChangedFilesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{changedErr: wantErr}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.calls) != 1 || len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionNeutral {
		t.Errorf("check run calls = %+v, updates = %+v, want one created and concluded neutral", gh.calls, gh.updates)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %d, want 0", len(runner.calls))
	}
}

func TestHandlePullRequestNoRunnersSkipsWorkflowLookup(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowErr: errors.New("boom")}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{}, nil, nil)

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}
	if len(gh.calls) != 1 || gh.calls[0].run.Conclusion != gate.ConclusionNeutral {
		t.Errorf("CreateCheckRun calls = %+v, want one neutral check run", gh.calls)
	}
	if gh.changedCalls != 0 {
		t.Errorf("ListChangedFiles calls = %d, want 0 when no runner is selected", gh.changedCalls)
	}
}

func TestHandlePullRequestEmptyProposals(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{}}}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if got := gh.updates[0].run; got.Conclusion != gate.ConclusionNeutral || !strings.Contains(got.Summary, "empty proposal list") {
		t.Errorf("check run = %+v, want neutral naming the empty proposal list", got)
	}
}

func TestHandlePullRequestRunnerStartError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{}
	runner := &fakeRunner{err: wantErr}
	svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}

func TestHandlePullRequestCreateCheckRunError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{err: wantErr}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(store.saveCalls) != 0 {
		t.Errorf("SavePR calls = %d, want 0 after CreateCheckRun error", len(store.saveCalls))
	}
}

func TestOnPush(t *testing.T) {
	t.Parallel()

	want := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}

	tests := []struct {
		name  string
		state gate.PRState
	}{
		{name: "fresh state", state: gate.PRState{Owner: "acme", Repo: "widgets", Number: 7}},
		{name: "state with older head", state: gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(want, gate.OnPush(tt.state, testPR())); diff != "" {
				t.Errorf("OnPush() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandlePullRequestSavesState(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	wantLoad := []loadPRCall{{owner: "acme", repo: "widgets", number: 7}}
	if diff := cmp.Diff(wantLoad, store.loadCalls, cmp.AllowUnexported(loadPRCall{})); diff != "" {
		t.Errorf("LoadPR calls (-want +got):\n%s", diff)
	}
	wantSave := []gate.PRState{{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}}
	if diff := cmp.Diff(wantSave, store.saveCalls); diff != "" {
		t.Errorf("SavePR calls (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestLoadError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{}
	svc := gate.NewService(gh, nil, &fakeStore{loadErr: wantErr}, gate.Runners{}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.calls) != 0 {
		t.Errorf("CreateCheckRun calls = %d, want 0 after LoadPR error", len(gh.calls))
	}
}

func TestHandlePullRequestSaveError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	svc := gate.NewService(&fakeGitHub{}, nil, &fakeStore{saveErr: wantErr}, gate.Runners{}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}

func TestHandlePullRequestActionsStartsRun(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	gh := &fakeGitHub{workflowExists: true, checkRunID: 555, mergeBase: "mb1"}
	runner := &fakeRunner{started: review.Pending{RunID: 99, Nonce: "n1", Deadline: deadline}}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if got := gh.calls[0].run; got.Status != gate.StatusInProgress || got.Conclusion != "" {
		t.Errorf("check run = %+v, want in progress without a conclusion", got)
	}
	if len(store.saveCalls) != 2 {
		t.Fatalf("SavePR calls = %d, want the armed state then the started state", len(store.saveCalls))
	}
	if armed := store.saveCalls[0].Run; armed == nil || armed.RunID != 0 || armed.Nonce != "check-555" || !armed.Deadline.After(time.Now()) {
		t.Errorf("armed run = %+v, want no run ID, nonce check-555 and a future deadline", armed)
	}
	if limit := time.Now().Add(gate.AnalysisDeadline); store.saveCalls[0].Run.Deadline.After(limit) {
		t.Errorf("armed deadline = %v, want within the analysis deadline, by %v", store.saveCalls[0].Run.Deadline, limit)
	}
	want := gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", CheckRunID: 555,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1", Deadline: deadline, BaseSHA: "mb1"},
	}
	if diff := cmp.Diff(want, store.saveCalls[1]); diff != "" {
		t.Errorf("started state (-want +got):\n%s", diff)
	}
}

func awaitingState() gate.PRState {
	return gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", CheckRunID: 555,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1", BaseSHA: "mb1"},
	}
}

func completedRun(conclusion string) gate.RunCompleted {
	return gate.RunCompleted{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, RunID: 99, Conclusion: conclusion}
}

func TestHandleRunCompleted(t *testing.T) {
	t.Parallel()

	proposals := review.Proposals{{DocPath: "docs/a.md", Reason: "endpoint changed"}}
	invalid := &review.InvalidResultError{Cause: errors.New("bad nonce")}

	tests := []struct {
		name           string
		conclusion     string
		runner         *fakeRunner
		wantConclusion gate.Conclusion
		wantSummary    string
		wantCollected  bool
		wantErr        bool
	}{
		{
			name:           "no impact",
			conclusion:     "success",
			runner:         &fakeRunner{result: review.Result{Verdict: review.NoImpact{Reason: "fine"}}},
			wantConclusion: gate.ConclusionSuccess,
			wantSummary:    "fine",
			wantCollected:  true,
		},
		{
			name:           "proposals",
			conclusion:     "success",
			runner:         &fakeRunner{result: review.Result{Verdict: proposals}},
			wantConclusion: gate.ConclusionActionRequired,
			wantSummary:    "- `docs/a.md`: endpoint changed",
			wantCollected:  true,
		},
		{
			name:           "invalid result",
			conclusion:     "success",
			runner:         &fakeRunner{collectErr: invalid},
			wantConclusion: gate.ConclusionNeutral,
			wantSummary:    "The pollux-agent workflow run returned an invalid result.",
			wantErr:        true,
			wantCollected:  true,
		},
		{
			name:           "failed run",
			conclusion:     "cancelled",
			runner:         &fakeRunner{},
			wantConclusion: gate.ConclusionNeutral,
			wantSummary:    "The pollux-agent workflow run was cancelled.",
			wantCollected:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			store := &fakeStore{stored: awaitingState()}
			svc := gate.NewService(gh, nil, store, gate.Runners{Actions: tc.runner}, nil, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun(tc.conclusion)); (err != nil) != tc.wantErr {
				t.Fatalf("HandleRunCompleted() = %v, want error = %v", err, tc.wantErr)
			}

			if (len(tc.runner.collected) == 1) != tc.wantCollected {
				t.Errorf("Collect calls = %+v, want called = %v", tc.runner.collected, tc.wantCollected)
			}
			if tc.wantCollected {
				want := review.Completion{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", BaseSHA: "mb1", RunID: 99, Nonce: "n1"}
				if diff := cmp.Diff(want, tc.runner.collected[0]); diff != "" {
					t.Errorf("Collect completion (-want +got):\n%s", diff)
				}
			}

			wantRun := gate.CheckRun{
				Name: "pollux-agent", HeadSHA: "abc123", Status: gate.StatusCompleted,
				Conclusion: tc.wantConclusion, Title: gh.updates[0].run.Title, Summary: tc.wantSummary,
			}
			if diff := cmp.Diff([]updateCheckRunCall{{id: 555, run: wantRun}}, gh.updates, cmp.AllowUnexported(updateCheckRunCall{})); diff != "" {
				t.Errorf("UpdateCheckRun calls (-want +got):\n%s", diff)
			}

			saved := awaitingState()
			saved.Run = nil
			got := store.saveCalls[len(store.saveCalls)-1]
			got.Proposals, got.SummaryCommentID, got.ProposalsSHA, got.FailureCause = nil, 0, "", ""
			if diff := cmp.Diff(saved, got); diff != "" {
				t.Errorf("final SavePR call (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleRunCompletedFailedRunCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		runner      *fakeRunner
		wantSummary string
	}{
		{
			name:        "invalid result adds its cause",
			runner:      &fakeRunner{collectErr: &review.InvalidResultError{Cause: errors.New("claude is_error: 401")}},
			wantSummary: "The pollux-agent workflow run failed.",
		},
		{
			name:        "other collect error falls back to the conclusion",
			runner:      &fakeRunner{collectErr: errors.New("no artifact")},
			wantSummary: "The pollux-agent workflow run failed.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			svc := gate.NewService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: tc.runner}, nil, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err == nil {
				t.Fatal("HandleRunCompleted() = nil, want the collect detail for the job log")
			}
			if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionNeutral || gh.updates[0].run.Summary != tc.wantSummary {
				t.Errorf("UpdateCheckRun calls = %+v, want one neutral with summary %q", gh.updates, tc.wantSummary)
			}
		})
	}
}

func TestHandlePullRequestSupersedesAwaitedRun(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: true, checkRunID: 556}
	runner := &fakeRunner{started: review.Pending{RunID: 100, Nonce: "n2"}}
	store := &fakeStore{stored: awaitingState()}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

	pr := testPR()
	pr.HeadSHA = "def4567890"
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	wantUpdate := updateCheckRunCall{id: 555, run: gate.CheckRun{
		Name: "pollux-agent", HeadSHA: "abc123", Status: gate.StatusCompleted, Conclusion: gate.ConclusionNeutral,
		Title: "Superseded", Summary: "Superseded by def4567",
	}}
	if diff := cmp.Diff([]updateCheckRunCall{wantUpdate}, gh.updates, cmp.AllowUnexported(updateCheckRunCall{})); diff != "" {
		t.Errorf("UpdateCheckRun calls (-want +got):\n%s", diff)
	}
	saved := store.saveCalls[len(store.saveCalls)-1]
	if saved.HeadSHA != "def4567890" || saved.CheckRunID != 556 || saved.Run == nil || saved.Run.RunID != 100 {
		t.Errorf("saved state = %+v, want new head awaiting run 100", saved)
	}

	store.stored = saved
	saves := len(store.saveCalls)
	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted(old run) = %v, want nil", err)
	}
	if len(gh.updates) != 1 || len(store.saveCalls) != saves {
		t.Errorf("updates = %d, saves = %d after the old run completed, want 1 and %d", len(gh.updates), len(store.saveCalls), saves)
	}
}

func TestHandlePullRequestSupersedesAwaitedRunOnSameHead(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: true, checkRunID: 556}
	runner := &fakeRunner{started: review.Pending{RunID: 100, Nonce: "n2"}}
	svc := gate.NewService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: runner}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	wantUpdate := updateCheckRunCall{id: 555, run: gate.CheckRun{
		Name: "pollux-agent", HeadSHA: "abc123", Status: gate.StatusCompleted, Conclusion: gate.ConclusionNeutral,
		Title: "Superseded", Summary: "Superseded by a re-run",
	}}
	if diff := cmp.Diff([]updateCheckRunCall{wantUpdate}, gh.updates, cmp.AllowUnexported(updateCheckRunCall{})); diff != "" {
		t.Errorf("UpdateCheckRun calls (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestSupersedeUpdateError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{updateErr: wantErr}
	svc := gate.NewService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{}, nil, nil)

	pr := testPR()
	pr.HeadSHA = "def4567"
	if err := svc.HandlePullRequest(t.Context(), pr); !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.calls) != 0 {
		t.Errorf("CreateCheckRun calls = %d, want 0 after supersede failure", len(gh.calls))
	}
}

func TestHandleDeadline(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	awaiting := awaitingState()
	awaiting.Run.Deadline = deadline
	pushed := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "def456", CheckRunID: 556}
	concluded := awaitingState()
	concluded.Run = nil

	tests := []struct {
		name     string
		state    gate.PRState
		nonce    string
		now      time.Time
		wantEnds bool
	}{
		{name: "overdue run ends neutral", state: awaiting, nonce: "n1", now: deadline.Add(time.Second), wantEnds: true},
		{name: "not yet overdue", state: awaiting, nonce: "n1", now: deadline.Add(-time.Second)},
		{name: "after completion", state: concluded, nonce: "n1", now: deadline.Add(time.Second)},
		{name: "after a new push", state: pushed, nonce: "n1", now: deadline.Add(time.Second)},
		{name: "other nonce", state: awaiting, nonce: "n0", now: deadline.Add(time.Second)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			store := &fakeStore{stored: tc.state}
			svc := gate.NewService(gh, nil, store, gate.Runners{}, nil, nil)

			if err := svc.HandleDeadline(t.Context(), ref, tc.nonce, tc.now); err != nil {
				t.Fatalf("HandleDeadline() = %v, want nil", err)
			}
			if !tc.wantEnds {
				if len(gh.updates) != 0 || len(store.saveCalls) != 0 {
					t.Errorf("updates = %v, saves = %v, want none", gh.updates, store.saveCalls)
				}
				return
			}
			if len(gh.updates) != 1 || gh.updates[0].id != 555 || gh.updates[0].run.Conclusion != gate.ConclusionNeutral ||
				!strings.Contains(gh.updates[0].run.Summary, "before the deadline") {
				t.Errorf("UpdateCheckRun calls = %+v, want one neutral naming the deadline", gh.updates)
			}
			if len(store.saveCalls) != 1 || store.saveCalls[0].Run != nil {
				t.Errorf("SavePR calls = %+v, want one with the run cleared", store.saveCalls)
			}
		})
	}
}

func TestHandleRunCompletedIgnoresUnmatchedRun(t *testing.T) {
	t.Parallel()

	other := completedRun("success")
	other.RunID = 100

	tests := []struct {
		name  string
		state gate.PRState
		rc    gate.RunCompleted
	}{
		{name: "other run", state: awaitingState(), rc: other},
		{name: "not awaiting", state: gate.PRState{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}, rc: completedRun("success")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			runner := &fakeRunner{}
			store := &fakeStore{stored: tc.state}
			svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

			if err := svc.HandleRunCompleted(t.Context(), tc.rc); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}
			if len(gh.updates) != 0 || len(store.saveCalls) != 0 || len(runner.collected) != 0 {
				t.Errorf("updates = %v, saves = %v, collects = %v, want none", gh.updates, store.saveCalls, runner.collected)
			}
		})
	}
}

func TestHandleRunCompletedTransientCollectError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("download failed")
	gh := &fakeGitHub{}
	store := &fakeStore{stored: awaitingState()}
	runner := &fakeRunner{collectErr: wantErr}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); !errors.Is(err, wantErr) {
		t.Fatalf("HandleRunCompleted() = %v, want %v", err, wantErr)
	}
	if len(runner.collected) != 3 {
		t.Errorf("Collect attempts = %d, want 3", len(runner.collected))
	}
	if len(gh.updates) != 1 {
		t.Fatalf("updates = %v, want one", gh.updates)
	}
	got := gh.updates[0].run
	if got.Conclusion != gate.ConclusionNeutral || got.Summary != "Pollux could not read the workflow run's result." {
		t.Errorf("check run = %+v, want neutral with the collect error", got)
	}
}

func TestHandleRunCompletedCollectRecovers(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{stored: awaitingState()}
	runner := &fakeRunner{collectErr: errors.New("502"), failFirst: 2, result: review.Result{Verdict: review.NoImpact{Reason: "fine"}}}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}
	if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("updates = %+v, want one success", gh.updates)
	}
}

func TestHandlePullRequestActionsCreatesCheckBeforeDispatch(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("dispatch refused")
	gh := &fakeGitHub{workflowExists: true, checkRunID: 555}
	runner := &fakeRunner{err: wantErr}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.calls) != 1 || gh.calls[0].run.Status != gate.StatusInProgress {
		t.Fatalf("CreateCheckRun calls = %+v, want one in progress", gh.calls)
	}
	if len(gh.updates) != 1 || gh.updates[0].id != 555 {
		t.Fatalf("updates = %+v, want the created check concluded", gh.updates)
	}
	got := gh.updates[0].run
	if got.Conclusion != gate.ConclusionNeutral || got.Summary != "The analysis failed unexpectedly." {
		t.Errorf("check run = %+v, want neutral with the generic cause, not the error text", got)
	}
}

func TestHandlePullRequestActionsSurvivesCancelAfterDispatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	gh := &fakeGitHub{workflowExists: true, checkRunID: 555}
	runner := &fakeRunner{started: review.Pending{RunID: 99, Nonce: "n1"}, onStart: cancel}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

	if err := svc.HandlePullRequest(ctx, testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	last := store.saveCalls[len(store.saveCalls)-1]
	if last.CheckRunID != 555 || last.Run == nil || last.Run.RunID != 99 {
		t.Fatalf("SavePR calls = %+v, want the last with the check run and awaited run", store.saveCalls)
	}
	for i, err := range store.saveCtxErrs {
		if err != nil {
			t.Errorf("SavePR call %d ctx error = %v, want a context unaffected by the job's cancellation", i, err)
		}
	}
}

func TestHandleRunCompletedCapsText(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 70000)
	tests := []struct {
		name     string
		runner   *fakeRunner
		conclude string
		maxBytes int
	}{
		{name: "summary", runner: &fakeRunner{result: review.Result{Verdict: review.NoImpact{Reason: long}}}, conclude: "success", maxBytes: 65535},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			store := &fakeStore{stored: awaitingState()}
			svc := gate.NewService(gh, nil, store, gate.Runners{Actions: tt.runner}, nil, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun(tt.conclude)); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}
			got := gh.updates[0].run.Summary
			if len(got) > tt.maxBytes || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
				t.Errorf("summary = %d bytes valid=%v, want <= %d, valid UTF-8, cut mark", len(got), utf8.ValidString(got), tt.maxBytes)
			}
		})
	}
}

func TestOnPushDropsAwaitedRun(t *testing.T) {
	t.Parallel()

	got := gate.OnPush(awaitingState(), testPR())
	if got.Run != nil || got.CheckRunID != 0 {
		t.Errorf("OnPush() = %+v, want no awaited run and no check run", got)
	}
}

func TestMatchesRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state gate.PRState
		runID int64
		want  bool
	}{
		{name: "same run", state: awaitingState(), runID: 99, want: true},
		{name: "other run", state: awaitingState(), runID: 100},
		{name: "zero run id", state: awaitingState(), runID: 0},
		{name: "not awaiting", state: gate.PRState{}, runID: 99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := gate.MatchesRun(tc.state, gate.RunCompleted{RunID: tc.runID}); got != tc.want {
				t.Errorf("MatchesRun(%+v, run %d) = %v, want %v", tc.state, tc.runID, got, tc.want)
			}
		})
	}
}

func proposal(doc, section string) review.Proposal {
	return review.Proposal{
		DocPath: doc, Section: section, Reason: "why " + section,
		Anchor:  review.Anchor{File: "a.go", Line: 4},
		Content: "## " + section + "\nnew\n", Original: "## " + section + "\nold\n",
	}
}

func proposalIDs(state gate.PRState) map[string]gate.ProposalStatus {
	got := map[string]gate.ProposalStatus{}
	for _, p := range state.Proposals {
		got[p.DocPath+"#"+p.Section] = p.State
	}
	return got
}

func countWrites(writes []gate.CommentWrite) (creates, edits int) {
	for _, w := range writes {
		if w.ID == 0 {
			creates++
		} else {
			edits++
		}
	}
	return creates, edits
}

func TestReconcile(t *testing.T) {
	t.Parallel()

	pr := gate.PullRequest{InstallationID: 1, Owner: "o", Repo: "r", Number: 3, HeadSHA: "0123456789"}
	a, b := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")
	aNew := a
	aNew.Content = "## A\nnewer\n"
	idA, idB := gate.ProposalID("docs/a.md", "A"), gate.ProposalID("docs/b.md", "B")
	prev := gate.PRState{SummaryCommentID: 90, Proposals: []gate.ProposalState{
		{ID: idA, DocPath: "docs/a.md", Section: "A", CommentID: 1, CommentURL: "u1", State: gate.ProposalOpen, Content: a.Content},
		{ID: idB, DocPath: "docs/b.md", Section: "B", CommentID: 2, CommentURL: "u2", State: gate.ProposalOpen, Content: b.Content},
	}}
	old := []gate.Comment{
		{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + idA + " -->\n\nold A body"},
		{ID: 2, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + idB + " -->\n\nold B body"},
		{ID: 90, Mine: true, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->"},
	}

	tests := []struct {
		name         string
		prev         gate.PRState
		verdict      review.Verdict
		existing     []gate.Comment
		wantCreates  int
		wantEdits    int
		wantStates   map[string]gate.ProposalStatus
		wantSummary  bool
		wantSumID    int64
		wantOutdated []string
	}{
		{
			name: "first run", verdict: review.Proposals{a, b},
			wantCreates: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true,
		},
		{
			name: "same proposals edit only", prev: prev, verdict: review.Proposals{a, b}, existing: old,
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "some gone are outdated", prev: prev, verdict: review.Proposals{a}, existing: old,
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "outdated"}, wantSummary: true, wantSumID: 90,
			wantOutdated: []string{"old B body"},
		},
		{
			name: "no impact outdates all", prev: prev, verdict: review.NoImpact{Reason: "x"}, existing: old,
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "outdated", "docs/b.md#B": "outdated"}, wantSummary: true, wantSumID: 90,
			wantOutdated: []string{"old A body", "old B body"},
		},
		{
			name: "no impact without prior state writes nothing", verdict: review.NoImpact{Reason: "x"}, wantStates: map[string]gate.ProposalStatus{},
		},
		{
			name: "duplicate ids keep first", verdict: review.Proposals{a, a},
			wantCreates: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open"}, wantSummary: true,
		},
		{
			name:    "outdated returns and reopens",
			prev:    gate.PRState{SummaryCommentID: 90, Proposals: []gate.ProposalState{{ID: idA, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOutdated}}},
			verdict: review.Proposals{a}, existing: []gate.Comment{old[0], old[2]},
			wantCreates: 1, wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "open returns changed gets a new comment", verdict: review.Proposals{aNew, b}, prev: prev, existing: old,
			wantCreates: 1, wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "deleted open proposal is recreated", prev: prev, verdict: review.Proposals{a, b}, existing: []gate.Comment{old[0], old[2]},
			wantCreates: 1, wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "deleted outdated proposal is not written", prev: prev, verdict: review.Proposals{a}, existing: []gate.Comment{old[0], old[2]},
			wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "outdated"}, wantSummary: true, wantSumID: 90,
		},
		{
			name: "deleted summary is recreated", prev: prev, verdict: review.Proposals{a, b}, existing: old[:2],
			wantCreates: 1, wantEdits: 2, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true,
		},
		{
			name: "crash recovery reuses marked comments", verdict: review.Proposals{a, b},
			existing:  append(slices.Clone(old), gate.Comment{ID: 91, Mine: true, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->\nrows"}),
			wantEdits: 3, wantStates: map[string]gate.ProposalStatus{"docs/a.md#A": "open", "docs/b.md#B": "open"}, wantSummary: true, wantSumID: 90,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state, writes := gate.Reconcile(tc.prev, pr, tc.verdict, nil, tc.existing)
			creates, edits := countWrites(writes)
			if creates != tc.wantCreates || edits != tc.wantEdits {
				t.Errorf("writes = %d creates, %d edits, want %d, %d", creates, edits, tc.wantCreates, tc.wantEdits)
			}
			if diff := cmp.Diff(tc.wantStates, proposalIDs(state)); diff != "" {
				t.Errorf("states (-want +got):\n%s", diff)
			}
			hasSummary := len(writes) > 0 && writes[len(writes)-1].Summary
			if hasSummary != tc.wantSummary || state.SummaryCommentID != tc.wantSumID {
				t.Errorf("summary write = %v id %d, want %v id %d", hasSummary, state.SummaryCommentID, tc.wantSummary, tc.wantSumID)
			}
			for _, want := range tc.wantOutdated {
				found := false
				for _, w := range writes {
					found = found || (w.Resolve && strings.Contains(w.Body, want) && strings.HasPrefix(w.Body, "<!-- pollux-agent:superseded:"))
				}
				if !found {
					t.Errorf("no outdated write keeping %q in %+v", want, writes)
				}
			}
		})
	}
}

func TestReconcileReopenedAppliedGetsNewComment(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	p.Content = "## A\nnewer\n"
	id := gate.ProposalID("docs/a.md", "A")
	prev := gate.PRState{Proposals: []gate.ProposalState{{
		ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, CommentURL: "u1", State: gate.ProposalApplied,
		Content: "## A\nold\n", AppliedSHA: "abc", ReplyID: 7,
	}}}
	oldBody := "<!-- pollux-agent:proposal:" + id + " -->\n\nreason\n\n- [x] Apply this change\n"
	existing := []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: oldBody}}

	state, writes := gate.Reconcile(prev, testPR(), review.Proposals{p}, nil, existing)

	var proposalWrites []gate.CommentWrite
	for _, w := range writes {
		if !w.Summary {
			proposalWrites = append(proposalWrites, w)
		}
	}
	if len(proposalWrites) != 2 {
		t.Fatalf("proposal writes = %+v, want a superseding edit then a create", proposalWrites)
	}
	if w := proposalWrites[0]; w.ID != 1 || !w.Resolve || w.Body != strings.Replace(oldBody, "proposal", "superseded", 1) {
		t.Errorf("first write = %+v, want edit of comment 1 with only its marker swapped, resolving its thread", w)
	}
	if w := proposalWrites[1]; w.ID != 0 || w.Review.Body == "" {
		t.Errorf("second write = %+v, want a create", w)
	}
	got := state.Proposals[0]
	if got.State != gate.ProposalOpen || got.CommentID != 0 || got.CommentURL != "" || got.AppliedSHA != "" || got.ReplyID != 0 {
		t.Errorf("proposal = %+v, want open with no comment, applied SHA or reply", got)
	}
}

func TestReconcileNeverAdoptsSupersededComment(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	id := gate.ProposalID("docs/a.md", "A")
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", State: gate.ProposalOpen}}}
	existing := []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:superseded:" + id + " -->\n\nold"}}

	state, writes := gate.Reconcile(prev, testPR(), review.Proposals{p}, nil, existing)

	if got := state.Proposals[0].CommentID; got != 0 {
		t.Errorf("CommentID = %d, want 0: a superseded comment is not adopted", got)
	}
	for _, w := range writes {
		if w.ID == 1 {
			t.Errorf("write %+v targets the superseded comment", w)
		}
	}
}

func TestReconcileKeepsRowOrder(t *testing.T) {
	t.Parallel()

	prev := gate.PRState{Proposals: []gate.ProposalState{
		{ID: gate.ProposalID("docs/a.md", "A"), DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen},
	}}
	state, _ := gate.Reconcile(prev, testPR(), review.Proposals{proposal("docs/z.md", "Z"), proposal("docs/a.md", "A")}, nil, nil)
	if len(state.Proposals) != 2 || state.Proposals[0].DocPath != "docs/a.md" || state.Proposals[1].DocPath != "docs/z.md" {
		t.Errorf("rows = %+v, want existing first, new appended", state.Proposals)
	}
}

func TestReconcileVariants(t *testing.T) {
	t.Parallel()

	suggest := review.Proposal{
		DocPath: "docs/a.md", Section: "Mid", Reason: "r", Anchor: review.Anchor{File: "a.go", Line: 4},
		Original: "## Mid\nmid body\n\n", Content: "## Mid\nnew ```go\nx\n```\n", Lines: review.LineRange{Start: 9, End: 11},
	}
	single := suggest
	single.Lines = review.LineRange{Start: 9, End: 9}
	newDoc := review.Proposal{DocPath: "docs/n.md", Anchor: review.Anchor{File: "a.go", Line: 4}, Reason: "r", Content: "# N\n", IndexEntry: "- n"}
	diffFile := func(path string, hunks ...review.LineRange) []review.ChangedFile {
		return []review.ChangedFile{{Path: path, Hunks: hunks}, {Path: "a.go", Hunks: []review.LineRange{{Start: 1, End: 10}}}}
	}

	tests := []struct {
		name        string
		p           review.Proposal
		changed     []review.ChangedFile
		wantPath    string
		wantStart   int
		wantLine    int
		wantSuggest bool
	}{
		{"in hunk", suggest, diffFile("docs/a.md", review.LineRange{Start: 1, End: 20}), "docs/a.md", 9, 11, true},
		{"single line has no start", single, diffFile("docs/a.md", review.LineRange{Start: 9, End: 9}), "docs/a.md", 0, 9, true},
		{"partly outside hunk", suggest, diffFile("docs/a.md", review.LineRange{Start: 10, End: 20}), "a.go", 0, 4, false},
		{"doc not in diff", suggest, diffFile("other.md", review.LineRange{Start: 1, End: 20}), "a.go", 0, 4, false},
		{"new doc", newDoc, diffFile("docs/n.md", review.LineRange{Start: 1, End: 20}), "a.go", 0, 4, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{tc.p}, tc.changed, nil)
			rc := writes[0].Review
			if rc.Path != tc.wantPath || rc.StartLine != tc.wantStart || rc.Line != tc.wantLine || rc.CommitSHA != "abc123" {
				t.Errorf("comment = %+v, want %s %d-%d on abc123", rc, tc.wantPath, tc.wantStart, tc.wantLine)
			}
			if got := strings.Contains(rc.Body, "suggestion\n"); got != tc.wantSuggest {
				t.Errorf("suggestion block = %v, want %v:\n%s", got, tc.wantSuggest, rc.Body)
			}
			if got := strings.Contains(rc.Body, "- [ ] Apply this change"); got == tc.wantSuggest {
				t.Errorf("checkbox = %v, want %v", got, !tc.wantSuggest)
			}
		})
	}

	_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{suggest}, diffFile("docs/a.md", review.LineRange{Start: 1, End: 20}), nil)
	body := writes[0].Review.Body
	want := "````suggestion\n## Mid\nnew ```go\nx\n```\n\n````\n"
	if !strings.HasSuffix(body, want) {
		t.Errorf("suggestion body = %q, want suffix %q (fence grown, trailing blank line kept)", body, want)
	}
}

func TestReconcileEditKeepsVariantSafe(t *testing.T) {
	t.Parallel()

	p := review.Proposal{
		DocPath: "docs/a.md", Section: "Mid", Reason: "r", Anchor: review.Anchor{File: "a.go", Line: 4},
		Original: "## Mid\nbody\n", Content: "## Mid\nnew\n", Lines: review.LineRange{Start: 9, End: 10},
	}
	id := gate.ProposalID(p.DocPath, p.Section)
	inDiff := []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: p.DocPath, Section: p.Section, CommentID: 1, State: gate.ProposalOpen, Content: p.Content}}}
	onCode := gate.Comment{ID: 1, Mine: true, Kind: gate.CommentKindReview, Path: "a.go", Line: 4}
	onDoc := gate.Comment{ID: 1, Mine: true, Kind: gate.CommentKindReview, Path: "docs/a.md", StartLine: 9, Line: 10}
	moved := gate.Comment{ID: 1, Mine: true, Kind: gate.CommentKindReview, Path: "docs/a.md", StartLine: 5, Line: 6}

	tests := []struct {
		name        string
		existing    gate.Comment
		changed     []review.ChangedFile
		wantSuggest bool
	}{
		{"checkbox on code stays checkbox when section enters the diff", onCode, inDiff, false},
		{"suggestion on same lines stays suggestion", onDoc, inDiff, true},
		{"suggestion whose lines moved becomes checkbox", moved, inDiff, false},
		{"suggestion leaving the diff becomes checkbox", onDoc, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, writes := gate.Reconcile(prev, testPR(), review.Proposals{p}, tc.changed, []gate.Comment{tc.existing})
			if writes[0].ID != 1 {
				t.Fatalf("write = %+v, want an edit of comment 1", writes[0])
			}
			if got := strings.Contains(writes[0].Body, "suggestion\n"); got != tc.wantSuggest {
				t.Errorf("suggestion body = %v, want %v:\n%s", got, tc.wantSuggest, writes[0].Body)
			}
		})
	}
}

func proposalService(t *testing.T, gh *fakeGitHub, store *fakeStore, verdict review.Verdict) {
	t.Helper()
	runner := &fakeRunner{started: review.Result{Verdict: verdict}}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)
	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
}

func TestHandlePullRequestFirstRunCreatesSummaryFirst(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	proposalService(t, gh, &fakeStore{}, review.Proposals{proposal("docs/a.md", "A"), proposal("docs/b.md", "B")})

	want := []string{"create-issue", "create-review", "create-review", "edit-issue"}
	if diff := cmp.Diff(want, gh.ops); diff != "" {
		t.Errorf("write order (-want +got):\n%s", diff)
	}
	if gh.comments[0].Kind != gate.CommentKindIssue {
		t.Fatalf("first comment kind = %s, want the summary", gh.comments[0].Kind)
	}
	for _, c := range gh.comments[1:] {
		if !strings.Contains(gh.comments[0].Body, "[view]("+c.URL+")") {
			t.Errorf("summary missing link %s:\n%s", c.URL, gh.comments[0].Body)
		}
	}
}

func TestHandlePullRequestRerunEditsInPlace(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	both := review.Proposals{proposal("docs/a.md", "A"), proposal("docs/b.md", "B")}

	proposalService(t, gh, store, both)
	if gh.createReview != 2 || gh.createIssue != 1 || gh.editReview != 0 || gh.editIssue != 1 {
		t.Fatalf("first run: create review/issue = %d/%d, edits = %d/%d, want 2/1, 0/1", gh.createReview, gh.createIssue, gh.editReview, gh.editIssue)
	}

	proposalService(t, gh, store, both)
	if gh.createReview != 2 || gh.createIssue != 1 || gh.editReview != 2 || gh.editIssue != 2 || len(gh.comments) != 3 {
		t.Errorf("same re-run: creates %d/%d edits %d/%d comments %d, want 2/1 2/2 3", gh.createReview, gh.createIssue, gh.editReview, gh.editIssue, len(gh.comments))
	}

	proposalService(t, gh, store, review.Proposals{both[0]})
	if len(gh.comments) != 3 || !strings.HasPrefix(gh.comments[2].Body, "<!-- pollux-agent:superseded:") || !slices.Contains(gh.resolved, gh.comments[2].ID) || !strings.Contains(gh.comments[0].Body, "outdated") {
		t.Errorf("partial re-run comments = %+v (resolved %v), want comment 2 retired and resolved, summary outdated, none added", gh.comments, gh.resolved)
	}

	proposalService(t, gh, store, review.NoImpact{Reason: "x"})
	if len(gh.comments) != 3 || strings.Contains(gh.comments[0].Body, "| open") {
		t.Errorf("no-impact re-run comments = %+v, want all outdated, none added", gh.comments)
	}
	if gh.listCalls != 4 {
		t.Errorf("ListComments calls = %d, want 1 per run (4 runs)", gh.listCalls)
	}
	last := gh.updates[len(gh.updates)-1].run
	if last.Conclusion != gate.ConclusionSuccess {
		t.Errorf("last check conclusion = %s, want success", last.Conclusion)
	}
}

func TestHandlePullRequestNoImpactFirstRunPostsNothing(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	proposalService(t, gh, &fakeStore{}, review.NoImpact{Reason: "x"})
	if gh.listCalls+gh.createReview+gh.createIssue != 0 {
		t.Errorf("list/create calls = %d/%d/%d, want none", gh.listCalls, gh.createReview, gh.createIssue)
	}
}

func TestHandlePullRequestRecoversUnrecordedComments(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	p := proposal("docs/a.md", "A")
	id := gate.ProposalID(p.DocPath, p.Section)
	gh.addComment(gate.CommentKindReview, "<!-- pollux-agent:proposal:"+id+" -->\n\nold")
	gh.addComment(gate.CommentKindIssue, "<!-- pollux-agent:summary -->")

	proposalService(t, gh, &fakeStore{}, review.Proposals{p})
	if gh.createReview+gh.createIssue != 0 || gh.editReview != 1 || gh.editIssue != 1 || len(gh.comments) != 2 {
		t.Errorf("creates %d/%d edits %d/%d comments %d, want 0/0 1/1 2", gh.createReview, gh.createIssue, gh.editReview, gh.editIssue, len(gh.comments))
	}
}

func TestHandlePullRequestResolveFailureDoesNotBlockProposals(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{checkRunID: 555}
	store := &fakeStore{}
	proposalService(t, gh, store, review.Proposals{proposal("docs/a.md", "A")})

	gh.resolveErr = errors.New("resolve failed")
	changed := proposal("docs/a.md", "A")
	changed.Content = "## A\nnewer\n"
	proposalService(t, gh, store, review.Proposals{changed})

	if gh.createReview != 2 || len(gh.comments) != 3 || !strings.HasPrefix(gh.comments[1].Body, "<!-- pollux-agent:superseded:") {
		t.Errorf("create review = %d, comments = %+v, want the old comment retired and a new one created", gh.createReview, gh.comments)
	}
	if n := len(gh.updates); n == 0 || gh.updates[n-1].run.Conclusion != gate.ConclusionActionRequired {
		t.Errorf("check updates = %+v, want the last to conclude action_required", gh.updates)
	}
}

func TestHandlePullRequestOutdatesCommentsPostedByACrashedRun(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{editIssueErr: errors.New("boom")}
	store := &fakeStore{}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil).WithCollectBackoff(0)
	if err := svc.HandlePullRequest(t.Context(), testPR()); err == nil {
		t.Fatal("HandlePullRequest() = nil, want the summary edit error")
	}

	gh.editIssueErr = nil
	proposalService(t, gh, store, review.NoImpact{Reason: "x"})
	if len(gh.comments) != 2 || !strings.HasPrefix(gh.comments[1].Body, "<!-- pollux-agent:superseded:") || !slices.Contains(gh.resolved, gh.comments[1].ID) {
		t.Errorf("comments after no-impact run = %+v (resolved %v), want the crashed run's comment retired and resolved", gh.comments, gh.resolved)
	}
}

func TestHandleRunCompletedPostsComments(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := &fakeStore{stored: awaitingState()}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}

	if gh.changedCalls != 1 || gh.createReview != 1 || gh.createIssue != 1 {
		t.Errorf("changed lists = %d, review creates = %d, summary creates = %d, want 1 each", gh.changedCalls, gh.createReview, gh.createIssue)
	}
	if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionActionRequired {
		t.Errorf("UpdateCheckRun calls = %+v, want one action_required", gh.updates)
	}
	if !strings.Contains(gh.comments[0].Body, "**pollux-agent** proposes 1 doc update.") || strings.Contains(gh.comments[0].Body, "Re-run") {
		t.Errorf("summary = %q, want the singular proposal heading and no Re-run box", gh.comments[0].Body)
	}
	got := store.saveCalls[len(store.saveCalls)-1]
	if got.SummaryCommentID == 0 || len(got.Proposals) != 1 || got.Proposals[0].CommentID == 0 || got.Run != nil {
		t.Errorf("saved state = %+v, want comment IDs recorded and no awaited run", got)
	}
}

func TestHandleRunCompletedRetriesFailedPosts(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{checkRunID: 5, failReviewCreate: 2, changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A"), proposal("docs/b.md", "B")}}}
	state := awaitingState()
	state.CheckRunID = 5
	store := &fakeStore{stored: state, live: true}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil after the inline retry", err)
	}

	if gh.createReview != 3 || len(gh.reviewComments) != 2 || gh.createIssue != 1 {
		t.Errorf("review creates = %d (%d succeeded), summary creates = %d, want 3 attempts, 2 comments, 1 summary", gh.createReview, len(gh.reviewComments), gh.createIssue)
	}
	got := store.saveCalls[len(store.saveCalls)-1]
	if got.Run != nil || got.CheckRunID != 5 || got.SummaryCommentID == 0 || len(got.Proposals) != 2 {
		t.Errorf("saved state = %+v, want no awaited run, check run 5, summary and 2 proposals", got)
	}
	var summary string
	for _, c := range gh.comments {
		if c.Kind == gate.CommentKindIssue {
			summary = c.Body
		}
	}
	if !strings.Contains(summary, "**pollux-agent** proposes 2 doc updates.") || strings.Contains(summary, "Re-run") {
		t.Errorf("summary = %q, want the plural proposal heading and no Re-run box", summary)
	}
}

func TestHandleRunCompletedGivesUpOnPersistentPostFailure(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{checkRunID: 5, createIssueErr: errors.New("boom"), changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	state := awaitingState()
	state.CheckRunID = 5
	store := &fakeStore{stored: state}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err == nil {
		t.Fatal("HandleRunCompleted() = nil, want the summary failure after the retries")
	}
	if gh.createIssue != 3 {
		t.Errorf("summary create attempts = %d, want 3", gh.createIssue)
	}
	if saved := store.saveCalls[len(store.saveCalls)-1]; saved.Run == nil || saved.Run.Nonce != "n1" {
		t.Errorf("saved state = %+v, want the run re-armed with nonce n1", saved)
	}
}

func TestReconcileIgnoresForeignMarkers(t *testing.T) {
	t.Parallel()

	a := proposal("docs/a.md", "A")
	id := gate.ProposalID("docs/a.md", "A")
	marker := "<!-- pollux-agent:proposal:" + id + " -->"
	tests := []struct {
		name     string
		existing gate.Comment
	}{
		{"forged proposal marker", gate.Comment{ID: 7, Kind: gate.CommentKindReview, Body: marker + "\n\nfake"}},
		{"forged summary marker", gate.Comment{ID: 8, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->\nfake"}},
		{"own comment with marker not on first line", gate.Comment{ID: 9, Mine: true, Kind: gate.CommentKindReview, Body: "text\n" + marker}},
		{"own summary with marker not on first line", gate.Comment{ID: 10, Mine: true, Kind: gate.CommentKindIssue, Body: "text <!-- pollux-agent:summary -->"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{a}, nil, []gate.Comment{tc.existing})
			creates, edits := countWrites(writes)
			if creates != 2 || edits != 0 || state.SummaryCommentID != 0 || state.Proposals[0].CommentID != 0 {
				t.Errorf("writes = %d creates, %d edits, state = %+v, want 2 creates, nothing adopted", creates, edits, state)
			}
		})
	}
}

func TestReconcileRecreatesStateCommentNotMine(t *testing.T) {
	t.Parallel()

	a := proposal("docs/a.md", "A")
	id := gate.ProposalID("docs/a.md", "A")
	prev := gate.PRState{SummaryCommentID: 90, Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen}}}
	existing := []gate.Comment{
		{ID: 1, Kind: gate.CommentKindReview, Body: "x"},
		{ID: 90, Kind: gate.CommentKindIssue, Body: "y"},
	}

	state, writes := gate.Reconcile(prev, testPR(), review.Proposals{a}, nil, existing)
	creates, edits := countWrites(writes)
	if creates != 2 || edits != 0 || state.SummaryCommentID != 0 || state.Proposals[0].CommentID != 0 {
		t.Errorf("writes = %d creates, %d edits, state = %+v, want 2 creates, no edits of foreign comments", creates, edits, state)
	}
}

func TestHandleRunCompletedChangedFilesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{changedErr: wantErr}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := &fakeStore{stored: awaitingState()}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); !errors.Is(err, wantErr) {
		t.Fatalf("HandleRunCompleted() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.updates) != 0 || gh.createReview != 0 || len(store.saveCalls) != 0 {
		t.Errorf("updates = %d, review creates = %d, saves = %d, want no writes", len(gh.updates), gh.createReview, len(store.saveCalls))
	}
}

func TestHandleRunCompletedFailureKeepsProposals(t *testing.T) {
	t.Parallel()

	id := gate.ProposalID("docs/a.md", "A")
	state := awaitingState()
	state.SummaryCommentID = 2
	state.Proposals = []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen}}
	gh := &fakeGitHub{}
	store := &fakeStore{stored: state}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: &fakeRunner{}}, nil, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}

	if gh.changedCalls != 0 || gh.editReview != 0 || gh.createReview != 0 {
		t.Errorf("review comment calls = changed %d, edits %d, creates %d, want none", gh.changedCalls, gh.editReview, gh.createReview)
	}
	if gh.editIssue+gh.createIssue != 1 {
		t.Errorf("summary writes = edits %d + creates %d, want 1", gh.editIssue, gh.createIssue)
	}
	got := store.saveCalls[len(store.saveCalls)-1]
	if diff := cmp.Diff(state.Proposals, got.Proposals); diff != "" || got.Run != nil {
		t.Errorf("saved state = %+v, want proposals untouched and no awaited run (-want +got):\n%s", got, diff)
	}
}

func TestReconcileRestoresOmittedHeading(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "Behavior")
	p.Original = "## Behavior\n\nold text\n"
	p.Content = "new text\n"

	_, writes := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, nil, nil)
	if len(writes) == 0 {
		t.Fatal("Reconcile() returned no writes, want a review comment create")
	}
	want := "-## Behavior\n-\n-old text\n+## Behavior\n+\n+new text\n"
	if body := writes[0].Review.Body; !strings.Contains(body, want) {
		t.Errorf("review comment body = %q, want diff %q", body, want)
	}

	p.Content = "### Install\n\nnew text\n"
	_, writes = gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, nil, nil)
	want = "+## Behavior\n+\n+### Install\n+\n+new text\n"
	if body := writes[0].Review.Body; !strings.Contains(body, want) {
		t.Errorf("subsection content: review comment body = %q, want diff %q", body, want)
	}
	p.Content = "## Behaviour\n\nnew text\n"
	_, writes = gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, nil, nil)
	if body := writes[0].Review.Body; strings.Contains(body, "+## Behavior\n") || !strings.Contains(body, "+## Behaviour\n") {
		t.Errorf("renamed heading: review comment body = %q, want the model's heading kept and no second heading", body)
	}
}

func TestOnPushSkips(t *testing.T) {
	t.Parallel()

	commit := &gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "r", HeadSHA: "old111"}
	pr := &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "r", HeadSHA: "old111"}
	tests := []struct {
		name        string
		prev        gate.PRState
		wantSkip    *gate.Skip
		wantPending *gate.SkipAsk
	}{
		{name: "commit skip cleared", prev: gate.PRState{Skip: commit, PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}}},
		{name: "PR skip kept, its pending ask cancelled by the new head", prev: gate.PRState{HeadSHA: "old111", Skip: pr, PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}}, wantSkip: pr},
		{name: "pending PR ask kept for the same head", prev: gate.PRState{HeadSHA: "abc123", PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}}, wantPending: &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}},
		{name: "pending commit ask kept for the same head", prev: gate.PRState{HeadSHA: "abc123", PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}}, wantPending: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}},
		{name: "commit skip kept for the same head", prev: gate.PRState{Skip: &gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "r", HeadSHA: "abc123"}}, wantSkip: &gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "r", HeadSHA: "abc123"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := gate.OnPush(tt.prev, testPR())
			if diff := cmp.Diff(tt.wantSkip, got.Skip); diff != "" {
				t.Errorf("Skip (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantPending, got.PendingSkip); diff != "" {
				t.Errorf("PendingSkip (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOnPushDropsPendingApply(t *testing.T) {
	t.Parallel()

	prev := gate.PRState{HeadSHA: "old111", PendingApply: &gate.PendingApply{IDs: []string{"p1"}, Message: "m", Parent: "old111"}}
	if got := gate.OnPush(prev, testPR()); got.PendingApply != nil {
		t.Errorf("OnPush().PendingApply = %+v, want nil", got.PendingApply)
	}
}

func TestOnPushCopiesFork(t *testing.T) {
	t.Parallel()

	pr := testPR()
	pr.Fork = true
	if got := gate.OnPush(gate.PRState{}, pr); !got.Fork {
		t.Error("OnPush().Fork = false, want true")
	}
}

func TestHandlePullRequestPRSkipSkipsAnalysis(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: true, checkRunID: 888}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "x"}}}
	store := &fakeStore{stored: gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111",
		Skip: &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated", HeadSHA: "old111"},
	}}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner, Server: runner}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if len(runner.calls) != 0 || gh.changedCalls != 0 {
		t.Errorf("runner starts = %d, changed-file lists = %d, want no analysis", len(runner.calls), gh.changedCalls)
	}
	if len(gh.calls) != 1 {
		t.Fatalf("created check runs = %d, want 1", len(gh.calls))
	}
	if run := gh.calls[0].run; run.HeadSHA != "abc123" || run.Conclusion != gate.ConclusionSuccess || !strings.Contains(run.Summary, "generated") {
		t.Errorf("check run = %+v, want success on abc123 naming the reason", run)
	}
	if got := store.saved; got == nil || got.HeadSHA != "abc123" || got.CheckRunID != 888 || got.Skip == nil || got.Run != nil {
		t.Errorf("saved state = %+v, want head abc123, check run 888, the PR skip kept and no awaited run", got)
	}
}

func TestHandleRunCompletedAfterSkip(t *testing.T) {
	t.Parallel()

	skipped, _ := gate.OnSkip(awaitingState(), gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "typo"})

	t.Run("late run is ignored", func(t *testing.T) {
		t.Parallel()

		gh := &fakeGitHub{}
		runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{{DocPath: "docs/a.md", Reason: "x"}}}}
		svc := gate.NewService(gh, nil, &fakeStore{stored: skipped}, gate.Runners{Actions: runner}, nil, nil)

		if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
			t.Fatalf("HandleRunCompleted() = %v, want nil", err)
		}
		if len(gh.updates) != 0 || len(runner.collected) != 0 {
			t.Errorf("updates = %d, collects = %d, want the late run ignored", len(gh.updates), len(runner.collected))
		}
	})

	t.Run("active skip beats the analysis result", func(t *testing.T) {
		t.Parallel()

		state := awaitingState()
		state.Skip = skipped.Skip
		gh := &fakeGitHub{}
		runner := &fakeRunner{result: review.Result{Verdict: review.NoImpact{Reason: "x"}}}
		store := &fakeStore{stored: state}
		svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

		if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err != nil {
			t.Fatalf("HandleRunCompleted() = %v, want nil", err)
		}
		if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionSuccess || !strings.Contains(gh.updates[0].run.Summary, "typo") {
			t.Errorf("updates = %+v, want the skip's success", gh.updates)
		}
		if store.saved == nil || store.saved.Run != nil {
			t.Errorf("saved state = %+v, want no awaited run", store.saved)
		}
	})
}

func TestHandleDeadlineAfterSkip(t *testing.T) {
	t.Parallel()

	state := awaitingState()
	state.Run.Deadline = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state.Skip = &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "typo", HeadSHA: "abc123"}
	gh := &fakeGitHub{}
	svc := gate.NewService(gh, nil, &fakeStore{stored: state}, gate.Runners{}, nil, nil)

	err := svc.HandleDeadline(t.Context(), gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}, "n1", state.Run.Deadline.Add(time.Hour))
	if err != nil {
		t.Fatalf("HandleDeadline() = %v, want nil", err)
	}
	if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("updates = %+v, want the skip's success rather than neutral", gh.updates)
	}
}

func TestHandlePullRequestRerunSkipsAppliedProposals(t *testing.T) {
	t.Parallel()

	a, b := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")
	applied := func(p review.Proposal, content string) gate.ProposalState {
		return gate.ProposalState{
			ID: gate.ProposalID(p.DocPath, p.Section), DocPath: p.DocPath, Section: p.Section,
			State: gate.ProposalApplied, Content: content, Original: p.Original,
		}
	}
	changed := a
	changed.Content = "## A\nrewritten\n"

	tests := []struct {
		name        string
		stored      []gate.ProposalState
		verdict     review.Proposals
		wantConc    gate.Conclusion
		wantTitle   string
		wantSummary []string
		notSummary  []string
	}{
		{
			name:      "all applied",
			stored:    []gate.ProposalState{applied(a, a.Content), applied(b, b.Content)},
			verdict:   review.Proposals{a, b},
			wantConc:  gate.ConclusionSuccess,
			wantTitle: "Docs up to date",
		},
		{
			name:        "one applied one new",
			stored:      []gate.ProposalState{applied(a, a.Content)},
			verdict:     review.Proposals{a, b},
			wantConc:    gate.ConclusionActionRequired,
			wantTitle:   "Docs need updating",
			wantSummary: []string{"docs/b.md"},
			notSummary:  []string{"docs/a.md"},
		},
		{
			name:        "applied but content changed",
			stored:      []gate.ProposalState{applied(a, a.Content)},
			verdict:     review.Proposals{changed},
			wantConc:    gate.ConclusionActionRequired,
			wantTitle:   "Docs need updating",
			wantSummary: []string{"docs/a.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			store := &fakeStore{stored: gate.PRState{
				Owner: "acme", Repo: "widgets", Number: 7, Proposals: tt.stored,
			}}
			proposalService(t, gh, store, tt.verdict)

			run := gh.updates[len(gh.updates)-1].run
			if run.Conclusion != tt.wantConc || run.Title != tt.wantTitle {
				t.Errorf("check = %s %q, want %s %q", run.Conclusion, run.Title, tt.wantConc, tt.wantTitle)
			}
			for _, s := range tt.wantSummary {
				if !strings.Contains(run.Summary, s) {
					t.Errorf("summary = %q, want it to mention %q", run.Summary, s)
				}
			}
			for _, s := range tt.notSummary {
				if strings.Contains(run.Summary, s) {
					t.Errorf("summary = %q, want it not to mention %q", run.Summary, s)
				}
			}
		})
	}
}

func TestHandlePullRequestSkipCancellationNote(t *testing.T) {
	t.Parallel()

	commitAsk := &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}
	prAsk := &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}
	tests := []struct {
		name        string
		ask         *gate.SkipAsk
		head        string
		wantNotes   int
		wantLabel   string
		wantPending *gate.SkipAsk
	}{
		{name: "new head cancels commit ask", ask: commitAsk, head: "def4567890", wantNotes: 1, wantLabel: "Skip this commit"},
		{name: "same head redelivery keeps the ask", ask: commitAsk, head: "abc1234567", wantPending: commitAsk},
		{name: "new head cancels PR ask", ask: prAsk, head: "def4567890", wantNotes: 1, wantLabel: "Skip this PR"},
		{name: "same head redelivery keeps the PR ask", ask: prAsk, head: "abc1234567", wantPending: prAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeStore{stored: gate.PRState{
				InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7,
				HeadSHA: "abc1234567", PendingSkip: tt.ask,
			}}
			gh := &fakeGitHub{}
			var savesAtNote int
			gh.onCreateIssue = func() { savesAtNote = len(store.saveCalls) }
			svc := gate.NewService(gh, nil, store, gate.Runners{}, nil, nil)

			pr := testPR()
			pr.HeadSHA = tt.head
			for range 2 {
				if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
					t.Fatalf("HandlePullRequest() = %v", err)
				}
				store.stored = *store.saved
			}

			if len(gh.comments) != tt.wantNotes {
				t.Fatalf("comments = %d, want %d", len(gh.comments), tt.wantNotes)
			}
			if tt.wantNotes == 1 {
				want := "@dev, a new push arrived before your reason, so the skip for `abc1234` was cancelled. Tick **" + tt.wantLabel + "** again to skip the new head."
				if gh.comments[0].Body != want {
					t.Errorf("note = %q, want %q", gh.comments[0].Body, want)
				}
				if savesAtNote == 0 {
					t.Error("note posted before SavePR")
				}
			}
			if diff := cmp.Diff(tt.wantPending, store.saved.PendingSkip); diff != "" {
				t.Errorf("PendingSkip (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandlePullRequestSkipCancellationNoteSurvivesFailedAnalysis(t *testing.T) {
	t.Parallel()

	store := &fakeStore{live: true, stored: gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7,
		HeadSHA: "abc1234567", PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit},
	}}
	gh := &fakeGitHub{checkRunID: 5}
	failure := &review.FailedError{Cause: review.CauseLimit, Err: errors.New("limit")}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: &fakeRunner{err: failure}}, nil, nil)
	pr := testPR()
	pr.HeadSHA = "def4567890"

	for range 2 {
		if err := svc.HandlePullRequest(t.Context(), pr); !errors.Is(err, failure) {
			t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, failure)
		}
	}

	notes := 0
	for _, c := range gh.comments {
		if strings.Contains(c.Body, "was cancelled") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("cancellation notes = %d, want 1 even though the analysis failed", notes)
	}
}

func TestHandlePullRequestServerStartErrorEndsNeutral(t *testing.T) {
	t.Parallel()

	failure := &review.FailedError{Cause: review.CauseLimit, Err: errors.New("model said: leak-me")}
	gh := &fakeGitHub{checkRunID: 555}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: &fakeRunner{err: failure}}, nil, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); !errors.Is(err, failure) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, failure)
	}

	if len(gh.calls) != 1 || gh.calls[0].run.Status != gate.StatusInProgress {
		t.Fatalf("CreateCheckRun calls = %+v, want one in progress", gh.calls)
	}
	if len(gh.updates) != 1 || gh.updates[0].id != 555 {
		t.Fatalf("updates = %+v, want the created check concluded", gh.updates)
	}
	if got := gh.updates[0].run; got.Conclusion != gate.ConclusionNeutral || got.Title != "Analysis failed" || got.Summary != "The analysis hit its step or token limit." {
		t.Errorf("check run = %+v, want neutral Analysis failed with the fixed limit cause", got)
	}

	if len(gh.comments) != 1 || !strings.Contains(gh.comments[0].Body, "The analysis hit its step or token limit.") ||
		!strings.Contains(gh.comments[0].Body, "- [ ] Re-run analysis\n") || strings.Contains(gh.comments[0].Body, "leak-me") {
		t.Errorf("comments = %+v, want one summary with the fixed cause and an unticked Re-run box", gh.comments)
	}
	last := store.saveCalls[len(store.saveCalls)-1]
	if last.Run != nil || last.SummaryCommentID != gh.comments[0].ID {
		t.Errorf("saved state = %+v, want the run cleared and the summary comment recorded", last)
	}
}

func TestHandleRerun(t *testing.T) {
	t.Parallel()

	ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	stored := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111", SummaryCommentID: 3}
	tests := []struct {
		name    string
		comment int64
		want    bool
	}{
		{name: "summary comment", comment: 3, want: true},
		{name: "other comment", comment: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{pullRequest: gate.PullRequest{BaseSHA: "tip1", HeadSHA: "new222", Open: true}, mergeBase: "base1"}
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			svc := gate.NewService(gh, nil, &fakeStore{stored: stored}, gate.Runners{Server: runner}, nil, nil)

			if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: ref, SummaryCommentID: tc.comment}); err != nil {
				t.Fatalf("HandleRerun() = %v, want nil", err)
			}
			if !tc.want {
				if len(runner.calls) != 0 || len(gh.calls) != 0 {
					t.Errorf("runner calls = %d, check runs = %d, want none", len(runner.calls), len(gh.calls))
				}
				return
			}
			if len(runner.calls) != 1 || runner.calls[0].HeadSHA != "new222" || runner.calls[0].BaseSHA != "base1" {
				t.Errorf("runner calls = %+v, want one on the current head new222 and merge base base1, not the base tip", runner.calls)
			}
			if len(gh.calls) != 1 || gh.calls[0].run.HeadSHA != "new222" || len(gh.updates) != 1 {
				t.Errorf("check runs = %+v, updates = %+v, want a new check run on new222, concluded", gh.calls, gh.updates)
			}
		})
	}
}

func TestHandleRerunSkips(t *testing.T) {
	t.Parallel()

	ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	tests := []struct {
		name   string
		stored gate.PRState
		pr     gate.PullRequest
	}{
		{
			name:   "pull request closed",
			stored: gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111"},
			pr:     gate.PullRequest{HeadSHA: "new222"},
		},
		{
			name:   "analysis in progress for the head",
			stored: gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "new222", CheckRunID: 5, Run: &gate.AwaitingRun{Nonce: "n", Deadline: time.Now().Add(time.Hour)}},
			pr:     gate.PullRequest{HeadSHA: "new222", Open: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{pullRequest: tc.pr}
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			store := &fakeStore{stored: tc.stored}
			svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)

			if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: ref}); err != nil {
				t.Fatalf("HandleRerun() = %v, want nil", err)
			}
			if len(runner.calls) != 0 || len(gh.calls) != 0 || len(gh.updates) != 0 || len(store.saveCalls) != 0 {
				t.Errorf("runner calls = %d, check runs = %d, updates = %d, saves = %d, want none", len(runner.calls), len(gh.calls), len(gh.updates), len(store.saveCalls))
			}
		})
	}
}

func TestHandleRerunSupersedesOverdueAnalysisOfSameHead(t *testing.T) {
	t.Parallel()

	stored := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "new222", CheckRunID: 5, Run: &gate.AwaitingRun{Nonce: "n", Deadline: time.Now().Add(-time.Minute)}}
	gh := &fakeGitHub{pullRequest: gate.PullRequest{HeadSHA: "new222", Open: true}}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := gate.NewService(gh, nil, &fakeStore{stored: stored}, gate.Runners{Server: runner}, nil, nil)

	if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}}); err != nil {
		t.Fatalf("HandleRerun() = %v, want nil", err)
	}
	if len(runner.calls) != 1 {
		t.Errorf("runner calls = %d, want 1 on the overdue head", len(runner.calls))
	}
	if len(gh.updates) < 1 || gh.updates[0].id != 5 || gh.updates[0].run.Title != "Superseded" {
		t.Errorf("updates = %+v, want the old check run 5 closed as superseded first", gh.updates)
	}
}

func TestHandlePullRequestServerRunnerRetriesFailedPosts(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{failReviewCreate: 1, changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil).WithCollectBackoff(0)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil after the inline retry", err)
	}
	if gh.createReview != 2 || len(gh.reviewComments) != 1 {
		t.Errorf("review creates = %d (%d succeeded), want 2 attempts, 1 comment", gh.createReview, len(gh.reviewComments))
	}
	for _, u := range gh.updates {
		if u.run.Title == "Analysis failed" || u.run.Conclusion != gate.ConclusionActionRequired {
			t.Errorf("check run update = %+v, want only action_required", u.run)
		}
	}
	if got := store.saveCalls[len(store.saveCalls)-1]; got.Run != nil || len(got.Proposals) != 1 || got.Proposals[0].CommentID == 0 {
		t.Errorf("saved state = %+v, want no awaited run and the proposal comment recorded", got)
	}
}

func TestHandleRerunSupersedesAnalysisOfOlderHead(t *testing.T) {
	t.Parallel()

	stored := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111", CheckRunID: 5, Run: &gate.AwaitingRun{Nonce: "n"}}
	gh := &fakeGitHub{pullRequest: gate.PullRequest{HeadSHA: "new222", Open: true}}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := gate.NewService(gh, nil, &fakeStore{stored: stored}, gate.Runners{Server: runner}, nil, nil)

	if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}}); err != nil {
		t.Fatalf("HandleRerun() = %v, want nil", err)
	}
	if len(runner.calls) != 1 {
		t.Errorf("runner calls = %d, want 1 on the new head", len(runner.calls))
	}
}

func manyFiles(n int) []review.ChangedFile {
	files := make([]review.ChangedFile, n)
	for i := range files {
		files[i] = review.ChangedFile{Path: fmt.Sprintf("f%d.go", i), Hunks: []review.LineRange{{Start: 1, End: 2}}, Patch: "@@"}
	}
	return files
}

func TestHandlePullRequestSizeLimit(t *testing.T) {
	t.Parallel()

	bigPatch := strings.Repeat("x", 1<<20)
	tests := []struct {
		name    string
		changed []review.ChangedFile
		want    string // summary of the too-large check; empty means the PR is analyzed
	}{
		{name: "50 files", changed: manyFiles(50)},
		{name: "51 files", changed: manyFiles(51), want: "51 changed files; the limit is 50."},
		{name: "patch at 1 MiB", changed: []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 1, End: 2}}, Patch: bigPatch}}},
		{
			name: "patch over 1 MiB across files",
			changed: []review.ChangedFile{
				{Path: "a.go", Hunks: []review.LineRange{{Start: 1, End: 2}}, Patch: bigPatch},
				{Path: "b.go", Hunks: []review.LineRange{{Start: 1, End: 2}}, Patch: "@@"},
			},
			want: "1048578 bytes of patch text; the limit is 1048576 bytes.",
		},
		{
			name:    "patch omitted by GitHub",
			changed: []review.ChangedFile{{Path: "big.go", Changes: 5000}},
			want:    "GitHub omitted the diff of a changed file; the limit is 1048576 bytes of patch text.",
		},
		{name: "binary file without patch", changed: []review.ChangedFile{{Path: "img.png"}}},
	}
	for _, tc := range tests {
		for _, kind := range []string{"server", "actions"} {
			t.Run(tc.name+"/"+kind, func(t *testing.T) {
				t.Parallel()

				gh := &fakeGitHub{checkRunID: 555, changed: tc.changed, workflowExists: kind == "actions"}
				runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
				runners := gate.Runners{Server: runner}
				if kind == "actions" {
					runners = gate.Runners{Actions: runner}
				}
				svc := gate.NewService(gh, nil, &fakeStore{}, runners, nil, nil)

				err := svc.HandlePullRequest(t.Context(), testPR())
				if tc.want == "" {
					if err != nil || len(runner.calls) != 1 {
						t.Fatalf("HandlePullRequest() = %v, runner calls = %d, want nil and 1", err, len(runner.calls))
					}
					return
				}
				if err == nil {
					t.Fatal("HandlePullRequest() = nil, want the too-large error")
				}
				if len(runner.calls) != 0 {
					t.Errorf("runner Start calls = %d, want none", len(runner.calls))
				}
				if len(gh.updates) != 1 {
					t.Fatalf("updates = %+v, want one", gh.updates)
				}
				if got := gh.updates[0].run; got.Conclusion != gate.ConclusionNeutral || got.Title != "PR too large to analyze" || got.Summary != tc.want {
					t.Errorf("check run = %+v, want neutral %q with summary %q", got, "PR too large to analyze", tc.want)
				}
				if len(gh.comments) != 1 || !strings.Contains(gh.comments[0].Body, tc.want) || !strings.Contains(gh.comments[0].Body, "- [ ] Re-run analysis\n") {
					t.Errorf("comments = %+v, want one summary with the limit and an unticked Re-run box", gh.comments)
				}
			})
		}
	}
}

func TestPostCommentsFailureRearmsRunForDeadlineSweep(t *testing.T) {
	t.Parallel()

	editErr := errors.New("edit failed")
	ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	sweep := func(t *testing.T, gh *fakeGitHub, store *fakeStore, svc *gate.Service, nonce string) {
		t.Helper()

		gh.editReviewErr = nil
		last := store.saveCalls[len(store.saveCalls)-1]
		if last.Run == nil || last.Run.Nonce != nonce {
			t.Fatalf("saved state = %+v, want the run armed with nonce %q", last, nonce)
		}
		store.stored = last
		updates := len(gh.updates)
		if err := svc.HandleDeadline(t.Context(), ref, nonce, last.Run.Deadline.Add(time.Second)); err != nil {
			t.Fatalf("HandleDeadline() = %v, want nil", err)
		}
		if len(gh.updates) != updates+1 {
			t.Fatalf("updates = %+v, want the deadline to conclude once more", gh.updates)
		}
		if got := gh.updates[updates].run; got.Conclusion != gate.ConclusionNeutral || got.Title != "Analysis failed" {
			t.Errorf("check run = %+v, want neutral %q", got, "Analysis failed")
		}
		if gh.updates[updates].id != last.CheckRunID {
			t.Errorf("concluded check run %d, want the same check run %d", gh.updates[updates].id, last.CheckRunID)
		}
		var summary string
		for _, c := range gh.comments {
			if c.Kind == gate.CommentKindIssue {
				summary = c.Body
			}
		}
		if !strings.Contains(summary, "- [ ] Re-run analysis\n") {
			t.Errorf("summary = %q, want the failure summary with an unticked Re-run box", summary)
		}
		if final := store.saveCalls[len(store.saveCalls)-1]; final.Run != nil {
			t.Errorf("final state = %+v, want the run cleared", final)
		}
	}

	t.Run("server result", func(t *testing.T) {
		t.Parallel()

		gh := &fakeGitHub{}
		store := &fakeStore{}
		proposalService(t, gh, store, review.Proposals{proposal("docs/a.md", "A")})
		gh.editReviewErr = editErr
		store.saveCalls = nil

		runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
		svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil).WithCollectBackoff(0)
		if err := svc.HandlePullRequest(t.Context(), testPR()); !errors.Is(err, editErr) {
			t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, editErr)
		}
		sweep(t, gh, store, svc, fmt.Sprintf("check-%d", store.saveCalls[len(store.saveCalls)-1].CheckRunID))
	})
	t.Run("run completed", func(t *testing.T) {
		t.Parallel()

		gh := &fakeGitHub{changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}, Patch: "@@"}}}
		runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
		store := &fakeStore{stored: awaitingState()}
		svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)
		if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
			t.Fatalf("HandleRunCompleted() = %v, want nil", err)
		}
		gh.editReviewErr = editErr
		store.stored = awaitingState()
		store.saveCalls = nil
		if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); !errors.Is(err, editErr) {
			t.Fatalf("HandleRunCompleted() = %v, want wrapping %v", err, editErr)
		}
		sweep(t, gh, store, svc, "n1")
	})
}

func TestPostCommentsTransientFailureEndsConcluded(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}, Patch: "@@"}}}
	gh.createIssueFailures = 1
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := &fakeStore{stored: awaitingState(), live: true}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)
	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil after the retry", err)
	}
	if last := store.saveCalls[len(store.saveCalls)-1]; last.Run != nil {
		t.Errorf("saved state = %+v, want the run cleared", last)
	}
}

func TestHandleDeadlineWritesFailureSummary(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name  string
		runID int64
		want  string
	}{
		{name: "server run", want: "The analysis did not report a result before the deadline."},
		{name: "actions run", runID: 99, want: "The pollux-agent workflow run did not report a result before the deadline."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := awaitingState()
			state.Run = &gate.AwaitingRun{RunID: tc.runID, Nonce: "n1", Deadline: deadline}
			gh := &fakeGitHub{}
			store := &fakeStore{stored: state}
			svc := gate.NewService(gh, nil, store, gate.Runners{}, nil, nil)

			ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
			if err := svc.HandleDeadline(t.Context(), ref, "n1", deadline.Add(time.Second)); err != nil {
				t.Fatalf("HandleDeadline() = %v, want nil", err)
			}
			if len(gh.updates) != 1 || gh.updates[0].run.Summary != tc.want {
				t.Errorf("updates = %+v, want one with summary %q", gh.updates, tc.want)
			}
			if len(gh.comments) != 1 || !strings.Contains(gh.comments[0].Body, tc.want) || !strings.Contains(gh.comments[0].Body, "- [ ] Re-run analysis\n") {
				t.Errorf("comments = %+v, want one summary with the cause and an unticked Re-run box", gh.comments)
			}
			if last := store.saveCalls[len(store.saveCalls)-1]; last.Run != nil || last.SummaryCommentID == 0 {
				t.Errorf("saved state = %+v, want the run cleared and the summary recorded", last)
			}
		})
	}
}

func TestHandlePullRequestConcludesAfterALongAnalysis(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		runner *fakeRunner
		want   gate.Conclusion
	}{
		{name: "failure", runner: &fakeRunner{err: &review.FailedError{Cause: review.CauseProvider, Err: errors.New("503")}}, want: gate.ConclusionNeutral},
		{name: "no impact", runner: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "refactor"}}}, want: gate.ConclusionSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				gh := &fakeGitHub{checkRunID: 7}
				tc.runner.onStart = func() { time.Sleep(2 * time.Minute) }
				svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: tc.runner}, nil, nil)

				_ = svc.HandlePullRequest(t.Context(), testPR())

				if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != tc.want {
					t.Errorf("updates = %+v, want the check concluded %s after a 2-minute analysis", gh.updates, tc.want)
				}
			})
		})
	}
}

func TestReconcileRetiresOutdatedCommentThatStillHasTheLiveMarker(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	id := gate.ProposalID("docs/a.md", "A")
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOutdated, Content: p.Content}}}
	body := "<!-- pollux-agent:proposal:" + id + " -->\n\nold"
	existing := []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: body}}

	state, writes := gate.Reconcile(prev, testPR(), review.Proposals{p}, nil, existing)

	if len(writes) < 2 || writes[0].ID != 1 || !writes[0].Resolve || writes[0].Body != strings.Replace(body, "proposal", "superseded", 1) || writes[1].ID != 0 {
		t.Errorf("writes = %+v, want a retire of comment 1 then a create", writes)
	}
	if got := state.Proposals[0]; got.State != gate.ProposalOpen || got.CommentID != 0 {
		t.Errorf("proposal = %+v, want open with no comment", got)
	}
}

func TestReconcileEmitsRetiresFirst(t *testing.T) {
	t.Parallel()

	a, b := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")
	bNew := b
	bNew.Content = "## B\nnewer\n"
	idA, idB := gate.ProposalID("docs/a.md", "A"), gate.ProposalID("docs/b.md", "B")
	prev := gate.PRState{SummaryCommentID: 90, Proposals: []gate.ProposalState{
		{ID: idA, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen, Content: a.Content},
		{ID: idB, DocPath: "docs/b.md", Section: "B", CommentID: 2, State: gate.ProposalOpen, Content: b.Content},
	}}
	existing := []gate.Comment{
		{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + idA + " -->\nA"},
		{ID: 2, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + idB + " -->\nB"},
		{ID: 90, Mine: true, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->"},
	}

	_, writes := gate.Reconcile(prev, testPR(), review.Proposals{a, bNew}, nil, existing)

	if len(writes) != 4 || !writes[0].Resolve || writes[0].ID != 2 || writes[1].ID != 1 || writes[2].ID != 0 || !writes[3].Summary {
		t.Errorf("writes = %+v, want retire of 2, edit of 1, create, summary", writes)
	}
}

func TestReconcileSameContent(t *testing.T) {
	t.Parallel()

	section := func(content string) review.Proposal {
		p := proposal("docs/a.md", "A")
		p.Content = content
		return p
	}
	newDoc := func(content string) review.Proposal {
		return review.Proposal{DocPath: "docs/c.md", Reason: "why", Anchor: review.Anchor{File: "a.go", Line: 4}, Content: content}
	}
	tests := []struct {
		name        string
		prior, got  review.Proposal
		wantCreates int
	}{
		{name: "section trailing newlines", prior: section("## A\nnew\n\n"), got: section("## A\nnew"), wantCreates: 0},
		{name: "section trailing whitespace", prior: section("## A\nnew  \n"), got: section("## A\nnew\n\n\n"), wantCreates: 0},
		{name: "section heading marks", prior: section("### A\nnew\n"), got: section("## A \nnew\n"), wantCreates: 0},
		{name: "section body differs", prior: section("## A\nnew\n"), got: section("## A\nnewer\n"), wantCreates: 1},
		{name: "new doc trailing newlines", prior: newDoc("# C\nx\n\n"), got: newDoc("# C\nx"), wantCreates: 0},
		{name: "new doc heading marks differ", prior: newDoc("## C\nx\n"), got: newDoc("# C\nx\n"), wantCreates: 1},
		{name: "new doc heading text differs", prior: newDoc("# C\nx\n"), got: newDoc("# D\nx\n"), wantCreates: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id := gate.ProposalID(tc.got.DocPath, tc.got.Section)
			prev := gate.PRState{SummaryCommentID: 2, Proposals: []gate.ProposalState{{ID: id, DocPath: tc.got.DocPath, Section: tc.got.Section, CommentID: 1, State: gate.ProposalOpen, Content: tc.prior.Content}}}
			existing := []gate.Comment{
				{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + id + " -->\nold"},
				{ID: 2, Mine: true, Kind: gate.CommentKindIssue, Body: "<!-- pollux-agent:summary -->\nold"},
			}

			_, writes := gate.Reconcile(prev, testPR(), review.Proposals{tc.got}, nil, existing)

			if creates, _ := countWrites(writes); creates != tc.wantCreates {
				t.Errorf("creates = %d, want %d", creates, tc.wantCreates)
			}
		})
	}
}

func TestReconcileAppliedIgnoresTrailingWhitespace(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	p.Content = "## A\nnew"
	id := gate.ProposalID("docs/a.md", "A")
	prev := gate.PRState{Proposals: []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalApplied, Content: "## A\nnew\n\n", AppliedSHA: "abc", ReplyID: 7}}}

	state, writes := gate.Reconcile(prev, testPR(), review.Proposals{p}, nil, nil)

	if got := state.Proposals[0]; got.State != gate.ProposalApplied || got.ReplyID != 7 {
		t.Errorf("proposal = %+v, want it to stay applied", got)
	}
	for _, w := range writes {
		if !w.Summary {
			t.Errorf("write %+v, want none for an applied proposal repeated", w)
		}
	}
}

func TestRetryReconcilesFromTheLatestSavedState(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	p, q := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")
	proposalService(t, gh, store, review.Proposals{p})

	changed := p
	changed.Content = "## A\nnewer\n"
	gh.failReviewCreate = gh.createReview + 2
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{changed, q}}}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil).WithCollectBackoff(0)
	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil after the inline retry", err)
	}

	for _, section := range []string{"A", "B"} {
		id := gate.ProposalID("docs/"+strings.ToLower(section)+".md", section)
		var live []int64
		for _, c := range gh.comments {
			if strings.HasPrefix(c.Body, "<!-- pollux-agent:proposal:"+id+" -->") {
				live = append(live, c.ID)
			}
		}
		var stored gate.ProposalState
		for _, ps := range store.saved.Proposals {
			if ps.ID == id {
				stored = ps
			}
		}
		if len(live) != 1 || stored.CommentID != live[0] {
			t.Errorf("proposal %s: live comments %v, state points at %d, want exactly one and state at it", section, live, stored.CommentID)
		}
	}
}
