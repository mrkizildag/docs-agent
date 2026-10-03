package gate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
)

func TestOnPush(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		state     gate.PRState
		pr        gate.PullRequest
		wantState gate.PRState
		wantRun   gate.CheckRun
	}{
		{
			name:  "fresh state",
			state: gate.PRState{Owner: "acme", Repo: "widgets", Number: 7},
			pr: gate.PullRequest{
				InstallationID: 42,
				Owner:          "acme",
				Repo:           "widgets",
				Number:         7,
				HeadSHA:        "abc123",
			},
			wantState: gate.PRState{
				InstallationID: 42,
				Owner:          "acme",
				Repo:           "widgets",
				Number:         7,
				HeadSHA:        "abc123",
			},
			wantRun: gate.CheckRun{
				Name:       "docs-agent",
				HeadSHA:    "abc123",
				Conclusion: gate.ConclusionSuccess,
				Title:      "docs-agent tracer",
				Summary:    "Analysis not implemented yet.",
			},
		},
		{
			name: "state with older head",
			state: gate.PRState{
				InstallationID: 42,
				Owner:          "acme",
				Repo:           "widgets",
				Number:         7,
				HeadSHA:        "old111",
			},
			pr: gate.PullRequest{
				InstallationID: 42,
				Owner:          "acme",
				Repo:           "widgets",
				Number:         7,
				HeadSHA:        "new222",
			},
			wantState: gate.PRState{
				InstallationID: 42,
				Owner:          "acme",
				Repo:           "widgets",
				Number:         7,
				HeadSHA:        "new222",
			},
			wantRun: gate.CheckRun{
				Name:       "docs-agent",
				HeadSHA:    "new222",
				Conclusion: gate.ConclusionSuccess,
				Title:      "docs-agent tracer",
				Summary:    "Analysis not implemented yet.",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotState, gotRun := gate.OnPush(tc.state, tc.pr)

			if diff := cmp.Diff(tc.wantState, gotState); diff != "" {
				t.Errorf("OnPush(%+v, %+v) state (-want +got):\n%s", tc.state, tc.pr, diff)
			}
			if diff := cmp.Diff(tc.wantRun, gotRun); diff != "" {
				t.Errorf("OnPush(%+v, %+v) run (-want +got):\n%s", tc.state, tc.pr, diff)
			}
		})
	}
}

type fakeGitHub struct {
	calls []createCheckRunCall
	err   error
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

func TestHandlePullRequest(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	svc := gate.NewService(gh, store)

	pr := gate.PullRequest{
		InstallationID: 42,
		Owner:          "acme",
		Repo:           "widgets",
		Number:         7,
		HeadSHA:        "abc123",
	}

	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}

	wantLoadCalls := []loadPRCall{{owner: "acme", repo: "widgets", number: 7}}
	if diff := cmp.Diff(wantLoadCalls, store.loadCalls, cmp.AllowUnexported(loadPRCall{})); diff != "" {
		t.Errorf("LoadPR calls (-want +got):\n%s", diff)
	}

	wantCreateCalls := []createCheckRunCall{
		{
			installationID: 42,
			owner:          "acme",
			repo:           "widgets",
			run: gate.CheckRun{
				Name:       "docs-agent",
				HeadSHA:    "abc123",
				Conclusion: gate.ConclusionSuccess,
				Title:      "docs-agent tracer",
				Summary:    "Analysis not implemented yet.",
			},
		},
	}
	if diff := cmp.Diff(wantCreateCalls, gh.calls, cmp.AllowUnexported(createCheckRunCall{})); diff != "" {
		t.Errorf("CreateCheckRun calls (-want +got):\n%s", diff)
	}

	wantSaveCalls := []gate.PRState{
		{
			InstallationID: 42,
			Owner:          "acme",
			Repo:           "widgets",
			Number:         7,
			HeadSHA:        "abc123",
		},
	}
	if diff := cmp.Diff(wantSaveCalls, store.saveCalls); diff != "" {
		t.Errorf("SavePR calls (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestLoadError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{}
	store := &fakeStore{loadErr: wantErr}
	svc := gate.NewService(gh, store)

	pr := gate.PullRequest{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}

	err := svc.HandlePullRequest(t.Context(), pr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest(%+v) = %v, want wrapping %v", pr, err, wantErr)
	}
	if len(gh.calls) != 0 {
		t.Errorf("CreateCheckRun calls = %d, want 0 after LoadPR error", len(gh.calls))
	}
}

func TestHandlePullRequestCreateCheckRunError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{err: wantErr}
	store := &fakeStore{}
	svc := gate.NewService(gh, store)

	pr := gate.PullRequest{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}

	err := svc.HandlePullRequest(t.Context(), pr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest(%+v) = %v, want wrapping %v", pr, err, wantErr)
	}
	if len(store.saveCalls) != 0 {
		t.Errorf("SavePR calls = %d, want 0 after CreateCheckRun error", len(store.saveCalls))
	}
}

func TestHandlePullRequestSaveError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{}
	store := &fakeStore{saveErr: wantErr}
	svc := gate.NewService(gh, store)

	pr := gate.PullRequest{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}

	err := svc.HandlePullRequest(t.Context(), pr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest(%+v) = %v, want wrapping %v", pr, err, wantErr)
	}
}
