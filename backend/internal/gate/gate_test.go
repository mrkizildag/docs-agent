package gate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

type fakeGitHub struct {
	calls          []createCheckRunCall
	err            error
	workflowExists bool
	workflowErr    error
}

type createCheckRunCall struct {
	installationID int64
	owner          string
	repo           string
	run            gate.CheckRun
}

func (f *fakeGitHub) CreateCheckRun(_ context.Context, installationID int64, owner, repo string, run gate.CheckRun) error {
	f.calls = append(f.calls, createCheckRunCall{installationID: installationID, owner: owner, repo: repo, run: run})
	return f.err
}

func (f *fakeGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return f.workflowExists, f.workflowErr
}

type fakeRunner struct {
	calls   []review.Request
	started review.Started
	err     error
}

func (f *fakeRunner) Start(_ context.Context, req review.Request) (review.Started, error) {
	f.calls = append(f.calls, req)
	return f.started, f.err
}

type fakeStore struct {
	loadCalls []loadPRCall
	saveCalls []gate.PRState
	loadErr   error
	saveErr   error
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
	return gate.PRState{Owner: owner, Repo: repo, Number: number}, nil
}

func (f *fakeStore) SavePR(_ context.Context, state gate.PRState) error {
	f.saveCalls = append(f.saveCalls, state)
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

			svc := gate.NewService(gh, &fakeStore{}, runners)
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
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: runner})

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}

	want := gate.CheckRun{
		Name:       "docs-agent",
		HeadSHA:    "abc123",
		Conclusion: gate.ConclusionSuccess,
		Title:      "No doc impact",
		Summary:    "docs already cover this",
	}
	if diff := cmp.Diff(want, gh.calls[0].run); diff != "" {
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
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: runner})

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}

	got := gh.calls[0].run
	if got.Conclusion != gate.ConclusionActionRequired || got.Title != "Docs need updating" {
		t.Errorf("check run = %+v, want action_required Docs need updating", got)
	}
	for _, p := range proposals {
		if !strings.Contains(got.Summary, p.DocPath) || !strings.Contains(got.Summary, p.Reason) {
			t.Errorf("summary = %q, want it to mention %q and %q", got.Summary, p.DocPath, p.Reason)
		}
	}
}

func TestHandlePullRequestWorkflowExistsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{workflowErr: wantErr}
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: &fakeRunner{}})

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}

func TestHandlePullRequestNoRunnersSkipsWorkflowLookup(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowErr: errors.New("boom")}
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{})

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}
	if len(gh.calls) != 1 || gh.calls[0].run.Conclusion != gate.ConclusionNeutral {
		t.Errorf("CreateCheckRun calls = %+v, want one neutral check run", gh.calls)
	}
}

func TestHandlePullRequestEmptyProposals(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{}}}
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: runner})

	if err := svc.HandlePullRequest(t.Context(), testPR()); err == nil {
		t.Fatalf("HandlePullRequest() = nil, want error for an empty proposal list")
	}
	if len(gh.calls) != 0 {
		t.Errorf("CreateCheckRun calls = %+v, want none", gh.calls)
	}
}

func TestHandlePullRequestRunnerStartError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{}
	runner := &fakeRunner{err: wantErr}
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: runner})

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
	svc := gate.NewService(gh, store, gate.Runners{})

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
	svc := gate.NewService(gh, store, gate.Runners{})

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
	svc := gate.NewService(gh, &fakeStore{loadErr: wantErr}, gate.Runners{})

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
	svc := gate.NewService(&fakeGitHub{}, &fakeStore{saveErr: wantErr}, gate.Runners{})

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}
