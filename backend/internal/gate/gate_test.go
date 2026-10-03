package gate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
)

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

func TestHandlePullRequest(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	svc := gate.NewService(gh)

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

	want := []createCheckRunCall{
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

	if diff := cmp.Diff(want, gh.calls, cmp.AllowUnexported(createCheckRunCall{})); diff != "" {
		t.Errorf("CreateCheckRun calls (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{err: wantErr}
	svc := gate.NewService(gh)

	pr := gate.PullRequest{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}

	err := svc.HandlePullRequest(t.Context(), pr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest(%+v) = %v, want wrapping %v", pr, err, wantErr)
	}
}
