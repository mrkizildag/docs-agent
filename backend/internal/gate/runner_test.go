package gate_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

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
