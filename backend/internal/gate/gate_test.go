package gate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

type fakeGitHub struct {
	calls          []createCheckRunCall
	updates        []updateCheckRunCall
	checkRunID     int64
	err            error
	updateErr      error
	workflowExists bool
	workflowErr    error
	changed        []review.ChangedFile
	changedErr     error
	changedCalls   int
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

func (f *fakeGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, id int64, run gate.CheckRun) error {
	f.updates = append(f.updates, updateCheckRunCall{id: id, run: run})
	return f.updateErr
}

func (f *fakeGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return f.workflowExists, f.workflowErr
}

func (f *fakeGitHub) ListChangedFiles(_ context.Context, _ int64, _, _ string, _ int) ([]review.ChangedFile, error) {
	f.changedCalls++
	return f.changed, f.changedErr
}

type fakeRunner struct {
	calls      []review.Request
	started    review.Started
	err        error
	collected  []review.Completion
	result     review.Result
	collectErr error
}

func (f *fakeRunner) Collect(_ context.Context, c review.Completion) (review.Result, error) {
	f.collected = append(f.collected, c)
	return f.result, f.collectErr
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
	stored    gate.PRState
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
	return gate.PRState{Owner: owner, Repo: repo, Number: number}, nil
}

func (f *fakeStore) PRForRun(context.Context, string, string, int64) (int, bool, error) {
	return 0, false, nil
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
		Status:     gate.StatusCompleted,
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

func TestHandlePullRequestPassesChangedFiles(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 3, End: 9}}, Patch: "@@ -1 +3,7 @@"}}
	gh := &fakeGitHub{changed: changed}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: runner})

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
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: runner})

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.calls) != 0 {
		t.Errorf("CreateCheckRun calls = %+v, want none", gh.calls)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %d, want 0", len(runner.calls))
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
	if gh.changedCalls != 0 {
		t.Errorf("ListChangedFiles calls = %d, want 0 when no runner is selected", gh.changedCalls)
	}
}

func TestHandlePullRequestEmptyProposals(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{}}}
	svc := gate.NewService(gh, &fakeStore{}, gate.Runners{Server: runner})

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if got := gh.calls[0].run; got.Conclusion != gate.ConclusionNeutral || !strings.Contains(got.Summary, "empty proposal list") {
		t.Errorf("check run = %+v, want neutral naming the empty proposal list", got)
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

func TestHandlePullRequestActionsStartsRun(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	gh := &fakeGitHub{workflowExists: true, checkRunID: 555}
	runner := &fakeRunner{started: review.Pending{RunID: 99, Nonce: "n1", Deadline: deadline}}
	store := &fakeStore{}
	svc := gate.NewService(gh, store, gate.Runners{Actions: runner})

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if got := gh.calls[0].run; got.Status != gate.StatusInProgress || got.Conclusion != "" {
		t.Errorf("check run = %+v, want in progress without a conclusion", got)
	}
	want := []gate.PRState{{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", CheckRunID: 555,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1", Deadline: deadline},
	}}
	if diff := cmp.Diff(want, store.saveCalls); diff != "" {
		t.Errorf("SavePR calls (-want +got):\n%s", diff)
	}
}

func awaitingState() gate.PRState {
	return gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", CheckRunID: 555,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1"},
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
			wantSummary:    "- docs/a.md: endpoint changed",
			wantCollected:  true,
		},
		{
			name:           "invalid result",
			conclusion:     "success",
			runner:         &fakeRunner{collectErr: invalid},
			wantConclusion: gate.ConclusionNeutral,
			wantSummary:    invalid.Error(),
			wantCollected:  true,
		},
		{
			name:           "failed run",
			conclusion:     "cancelled",
			runner:         &fakeRunner{},
			wantConclusion: gate.ConclusionNeutral,
			wantSummary:    "workflow run cancelled",
			wantCollected:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			store := &fakeStore{stored: awaitingState()}
			svc := gate.NewService(gh, store, gate.Runners{Actions: tc.runner})

			if err := svc.HandleRunCompleted(t.Context(), completedRun(tc.conclusion)); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}

			if (len(tc.runner.collected) == 1) != tc.wantCollected {
				t.Errorf("Collect calls = %+v, want called = %v", tc.runner.collected, tc.wantCollected)
			}
			if tc.wantCollected {
				want := review.Completion{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", RunID: 99, Nonce: "n1"}
				if diff := cmp.Diff(want, tc.runner.collected[0]); diff != "" {
					t.Errorf("Collect completion (-want +got):\n%s", diff)
				}
			}

			wantRun := gate.CheckRun{
				Name: "docs-agent", HeadSHA: "abc123", Status: gate.StatusCompleted,
				Conclusion: tc.wantConclusion, Title: gh.updates[0].run.Title, Summary: tc.wantSummary,
			}
			if diff := cmp.Diff([]updateCheckRunCall{{id: 555, run: wantRun}}, gh.updates, cmp.AllowUnexported(updateCheckRunCall{})); diff != "" {
				t.Errorf("UpdateCheckRun calls (-want +got):\n%s", diff)
			}

			saved := awaitingState()
			saved.Run = nil
			if diff := cmp.Diff([]gate.PRState{saved}, store.saveCalls); diff != "" {
				t.Errorf("SavePR calls (-want +got):\n%s", diff)
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
			wantSummary: "workflow run failure: claude is_error: 401",
		},
		{
			name:        "other collect error falls back to the conclusion",
			runner:      &fakeRunner{collectErr: errors.New("no artifact")},
			wantSummary: "workflow run failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			svc := gate.NewService(gh, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: tc.runner})

			if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
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
	svc := gate.NewService(gh, store, gate.Runners{Actions: runner})

	pr := testPR()
	pr.HeadSHA = "def4567890"
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	wantUpdate := updateCheckRunCall{id: 555, run: gate.CheckRun{
		Name: "docs-agent", HeadSHA: "abc123", Status: gate.StatusCompleted, Conclusion: gate.ConclusionNeutral,
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
	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted(old run) = %v, want nil", err)
	}
	if len(gh.updates) != 1 || len(store.saveCalls) != 1 {
		t.Errorf("updates = %d, saves = %d after the old run completed, want 1 and 1", len(gh.updates), len(store.saveCalls))
	}
}

func TestHandlePullRequestSupersedesAwaitedRunOnSameHead(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: true, checkRunID: 556}
	runner := &fakeRunner{started: review.Pending{RunID: 100, Nonce: "n2"}}
	svc := gate.NewService(gh, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: runner})

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	wantUpdate := updateCheckRunCall{id: 555, run: gate.CheckRun{
		Name: "docs-agent", HeadSHA: "abc123", Status: gate.StatusCompleted, Conclusion: gate.ConclusionNeutral,
		Title: "Superseded", Summary: "Superseded by abc123",
	}}
	if diff := cmp.Diff([]updateCheckRunCall{wantUpdate}, gh.updates, cmp.AllowUnexported(updateCheckRunCall{})); diff != "" {
		t.Errorf("UpdateCheckRun calls (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestSupersedeUpdateError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{updateErr: wantErr}
	svc := gate.NewService(gh, &fakeStore{stored: awaitingState()}, gate.Runners{})

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
			svc := gate.NewService(gh, store, gate.Runners{})

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
			svc := gate.NewService(gh, store, gate.Runners{Actions: runner})

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
	svc := gate.NewService(gh, store, gate.Runners{Actions: &fakeRunner{collectErr: wantErr}})

	err := svc.HandleRunCompleted(t.Context(), completedRun("success"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandleRunCompleted() = %v, want wrapping %v", err, wantErr)
	}
	if len(gh.updates) != 0 || len(store.saveCalls) != 0 {
		t.Errorf("updates = %v, saves = %v, want none so the job can retry", gh.updates, store.saveCalls)
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
