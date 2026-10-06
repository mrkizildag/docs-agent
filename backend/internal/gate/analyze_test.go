package gate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestHandlePullRequestNoImpact(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Workflow: false}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "docs already cover this"}}}
	svc := newService(gh, newStore(t), gate.Runners{Server: runner}, nil)

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
	if diff := cmp.Diff(want, theCheckRun(t, gh).Latest()); diff != "" {
		t.Errorf("check run (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestProposals(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Workflow: false}
	proposals := review.Proposals{
		{DocPath: "docs/a.md", Reason: "endpoint changed"},
		{DocPath: "docs/b.md", Reason: "config added"},
	}
	runner := &fakeRunner{started: review.Result{Verdict: proposals}}
	svc := newService(gh, newStore(t), gate.Runners{Server: runner}, nil)

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}

	got := theCheckRun(t, gh).Latest()
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

	gh := &gatetest.GitHub{Workflow: true, NextCheckRunID: 555}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{{DocPath: "docs/a.md", Reason: "restored"}}}}
	store := newStore(t)
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if cr := theCheckRun(t, gh); cr.ID != 555 || len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionActionRequired {
		t.Errorf("check run = %+v, want check run 555 concluded once as action_required", cr)
	}
	if got := loadPR(t, store, 7); got.Run != nil {
		t.Errorf("final saved Run = %+v, want nil so the deadline sweep has nothing to conclude", got.Run)
	}
}

func TestHandlePullRequestWorkflowExistsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &gatetest.GitHub{Fail: map[string]error{"WorkflowExists": wantErr}}
	svc := newService(gh, newStore(t), gate.Runners{Server: &fakeRunner{}}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}

func TestHandlePullRequestMergeBase(t *testing.T) {
	t.Parallel()

	pr := testPR()
	gh := &gatetest.GitHub{MergeBaseSHA: "mb1"}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := newService(gh, newStore(t), gate.Runners{Server: runner}, nil)

	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if diff := cmp.Diff([][2]string{{pr.BaseSHA, pr.HeadSHA}}, gh.MergeBaseArgs()); diff != "" {
		t.Errorf("MergeBase (base, head) args (-want +got):\n%s", diff)
	}
	if len(runner.calls) != 1 || runner.calls[0].BaseSHA != "mb1" || runner.calls[0].HeadSHA != pr.HeadSHA {
		t.Errorf("runner calls = %+v, want one with BaseSHA mb1 and HeadSHA %s", runner.calls, pr.HeadSHA)
	}
}

func TestHandlePullRequestMergeBaseError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &gatetest.GitHub{Fail: map[string]error{"MergeBase": wantErr}}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := newService(gh, newStore(t), gate.Runners{Server: runner}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionNeutral {
		t.Errorf("check run = %+v, want one created and concluded neutral", cr)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %d, want 0", len(runner.calls))
	}
}

func TestHandlePullRequestPassesChangedFiles(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 3, End: 9}}, Patch: "@@ -1 +3,7 @@"}}
	gh := &gatetest.GitHub{Changed: changed}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := newService(gh, newStore(t), gate.Runners{Server: runner}, nil)

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
	gh := &gatetest.GitHub{Fail: map[string]error{"ListChangedFiles": wantErr}}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := newService(gh, newStore(t), gate.Runners{Server: runner}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionNeutral {
		t.Errorf("check run = %+v, want one created and concluded neutral", cr)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner calls = %d, want 0", len(runner.calls))
	}
}

func TestHandlePullRequestNoRunnersSkipsWorkflowLookup(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Fail: map[string]error{"WorkflowExists": errors.New("boom")}}
	svc := newService(gh, newStore(t), gate.Runners{}, nil)

	pr := testPR()
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest(%+v) = %v, want nil", pr, err)
	}
	if got := theCheckRun(t, gh).Created; got.Conclusion != gate.ConclusionNeutral {
		t.Errorf("created check run = %+v, want a neutral one", got)
	}
	if n := gh.CallCount("ListChangedFiles"); n != 0 {
		t.Errorf("ListChangedFiles calls = %d, want 0 when no runner is selected", n)
	}
}

func TestHandlePullRequestUnusableResultReportsFailureWithCause(t *testing.T) {
	t.Parallel()

	for name, verdict := range map[string]review.Verdict{"empty proposals": review.Proposals{}, "no verdict": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gh := &gatetest.GitHub{}
			store := newStore(t)
			runner := &fakeRunner{started: review.Result{Verdict: verdict}}
			svc := newService(gh, store, gate.Runners{Server: runner}, nil)

			if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
				t.Fatalf("HandlePullRequest() = %v, want nil: the failure is already reported", err)
			}
			cr := theCheckRun(t, gh)
			if got := cr.Latest(); len(cr.Updates) != 1 || got.Conclusion != gate.ConclusionNeutral || got.Title != "Analysis failed" {
				t.Fatalf("check run = %+v, want one neutral Analysis failed", cr)
			}
			cause := cr.Latest().Summary
			if body := summaryBody(t, gh); !strings.Contains(body, cause) || !strings.Contains(body, "- [ ] Re-run analysis\n") {
				t.Errorf("summary = %q, want it stating %q with an unticked Re-run box", body, cause)
			}
			if saved := loadPR(t, store, 7); saved.FailureCause != cause || saved.Run != nil {
				t.Errorf("saved state = %+v, want FailureCause %q and no awaited run", saved, cause)
			}
		})
	}
}

func TestHandlePullRequestRunnerStartError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &gatetest.GitHub{}
	runner := &fakeRunner{err: wantErr}
	svc := newService(gh, newStore(t), gate.Runners{Server: runner}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}

func TestHandlePullRequestCreateCheckRunError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &gatetest.GitHub{Fail: map[string]error{"CreateCheckRun": wantErr}}
	store := newStore(t)
	svc := newService(gh, store, gate.Runners{}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if saved := loadPR(t, store, 7); saved.HeadSHA != "" {
		t.Errorf("stored state = %+v, want none saved after CreateCheckRun error", saved)
	}
}

func TestHandlePullRequestSavesState(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	store := newStore(t)
	svc := newService(gh, store, gate.Runners{}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	want := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", CheckRunID: theCheckRunID(t, gh)}
	if diff := cmp.Diff(want, loadPR(t, store, 7)); diff != "" {
		t.Errorf("stored state (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestLoadError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &gatetest.GitHub{}
	svc := newService(gh, &hookStore{Store: newStore(t), loadErr: wantErr}, gate.Runners{}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if runs := gh.CheckRuns(); len(runs) != 0 {
		t.Errorf("check runs = %+v, want none after LoadPR error", runs)
	}
}

func TestHandlePullRequestSaveError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	svc := newService(&gatetest.GitHub{}, &hookStore{Store: newStore(t), saveErr: wantErr}, gate.Runners{}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
}

func TestHandlePullRequestActionsStartsRun(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var armed *gate.AwaitingRun
	store := newStore(t)
	gh := &gatetest.GitHub{Workflow: true, NextCheckRunID: 555}
	runner := &fakeRunner{started: review.Pending{RunID: 99, Nonce: "n1", Deadline: deadline}}
	runner.onStart = func() { armed = loadPR(t, store, 7).Run }
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if got := theCheckRun(t, gh); got.Created.Status != gate.StatusInProgress || got.Created.Conclusion != "" || len(got.Updates) != 0 {
		t.Errorf("check run = %+v, want in progress without a conclusion", got)
	}
	if armed == nil || armed.RunID != 0 || armed.Nonce != "check-555" || !armed.Deadline.After(time.Now()) {
		t.Errorf("armed run before the dispatch = %+v, want no run ID, nonce check-555 and a future deadline", armed)
	}
	want := gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", CheckRunID: 555,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1", Deadline: deadline},
	}
	if diff := cmp.Diff(want, loadPR(t, store, 7)); diff != "" {
		t.Errorf("started state (-want +got):\n%s", diff)
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
			wantSummary:    "- docs/a.md: endpoint changed",
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

			gh := &gatetest.GitHub{}
			store := newStore(t, awaitingState())
			svc := newService(gh, store, gate.Runners{Actions: tc.runner}, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun(tc.conclusion)); (err != nil) != tc.wantErr {
				t.Fatalf("HandleRunCompleted() = %v, want error = %v", err, tc.wantErr)
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

			cr := theCheckRun(t, gh)
			wantRun := gate.CheckRun{
				Name: "pollux-agent", HeadSHA: "abc123", Status: gate.StatusCompleted,
				Conclusion: tc.wantConclusion, Title: cr.Latest().Title, Summary: tc.wantSummary,
			}
			if diff := cmp.Diff(gatetest.CheckRun{ID: 555, Updates: []gate.CheckRun{wantRun}}, cr); diff != "" {
				t.Errorf("check run (-want +got):\n%s", diff)
			}

			saved := awaitingState()
			saved.Run = nil
			got := loadPR(t, store, 7)
			got.Proposals, got.SummaryCommentID, got.ProposalsSHA, got.FailureCause = nil, 0, "", ""
			if diff := cmp.Diff(saved, got); diff != "" {
				t.Errorf("stored state (-want +got):\n%s", diff)
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

			gh := &gatetest.GitHub{}
			svc := newService(gh, newStore(t, awaitingState()), gate.Runners{Actions: tc.runner}, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err == nil {
				t.Fatal("HandleRunCompleted() = nil, want the collect detail for the job log")
			}
			if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionNeutral || cr.Latest().Summary != tc.wantSummary {
				t.Errorf("check run = %+v, want one neutral update with summary %q", cr, tc.wantSummary)
			}
		})
	}
}

func TestHandlePullRequestSupersedesAwaitedRun(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Workflow: true, NextCheckRunID: 556}
	runner := &fakeRunner{started: review.Pending{RunID: 100, Nonce: "n2"}}
	store := newStore(t, awaitingState())
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

	pr := testPR()
	pr.HeadSHA = "def4567890"
	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	wantSuperseded := gatetest.CheckRun{ID: 555, Updates: []gate.CheckRun{{
		Name: "pollux-agent", HeadSHA: "abc123", Status: gate.StatusCompleted, Conclusion: gate.ConclusionNeutral,
		Title: "Superseded", Summary: "Superseded by def4567",
	}}}
	if got := gh.CheckRuns()[0]; !cmp.Equal(wantSuperseded, got) {
		t.Errorf("old check run (-want +got):\n%s", cmp.Diff(wantSuperseded, got))
	}
	saved := loadPR(t, store, 7)
	if saved.HeadSHA != "def4567890" || saved.CheckRunID != 556 || saved.Run == nil || saved.Run.RunID != 100 {
		t.Errorf("stored state = %+v, want new head awaiting run 100", saved)
	}

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted(old run) = %v, want nil", err)
	}
	if diff := cmp.Diff(saved, loadPR(t, store, 7)); diff != "" {
		t.Errorf("stored state after the old run completed (-want +got):\n%s", diff)
	}
	if runs := gh.CheckRuns(); len(runs) != 2 || len(runs[0].Updates) != 1 || len(runs[1].Updates) != 0 {
		t.Errorf("check runs = %+v, want the old run closed once and the new one left alone", runs)
	}
}

func TestHandlePullRequestSupersedesAwaitedRunOnSameHead(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Workflow: true, NextCheckRunID: 556}
	runner := &fakeRunner{started: review.Pending{RunID: 100, Nonce: "n2"}}
	svc := newService(gh, newStore(t, awaitingState()), gate.Runners{Actions: runner}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	wantSuperseded := gatetest.CheckRun{ID: 555, Updates: []gate.CheckRun{{
		Name: "pollux-agent", HeadSHA: "abc123", Status: gate.StatusCompleted, Conclusion: gate.ConclusionNeutral,
		Title: "Superseded", Summary: "Superseded by a re-run",
	}}}
	if got := gh.CheckRuns()[0]; !cmp.Equal(wantSuperseded, got) {
		t.Errorf("old check run (-want +got):\n%s", cmp.Diff(wantSuperseded, got))
	}
}

func TestHandlePullRequestSupersedeUpdateError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &gatetest.GitHub{Fail: map[string]error{"UpdateCheckRun": wantErr}}
	svc := newService(gh, newStore(t, awaitingState()), gate.Runners{}, nil)

	pr := testPR()
	pr.HeadSHA = "def4567"
	if err := svc.HandlePullRequest(t.Context(), pr); !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if n := gh.CallCount("CreateCheckRun"); n != 0 {
		t.Errorf("CreateCheckRun calls = %d, want 0 after supersede failure", n)
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

			gh := &gatetest.GitHub{}
			store := newStore(t, tc.state)
			svc := newService(gh, store, gate.Runners{}, nil)

			if err := svc.HandleDeadline(t.Context(), ref, tc.nonce, tc.now); err != nil {
				t.Fatalf("HandleDeadline() = %v, want nil", err)
			}
			if !tc.wantEnds {
				if runs := gh.CheckRuns(); len(runs) != 0 {
					t.Errorf("check runs = %+v, want none touched", runs)
				}
				if diff := cmp.Diff(tc.state, loadPR(t, store, 7)); diff != "" {
					t.Errorf("stored state (-want +got):\n%s", diff)
				}
				return
			}
			if cr := theCheckRun(t, gh); cr.ID != 555 || len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionNeutral ||
				!strings.Contains(cr.Latest().Summary, "before the deadline") {
				t.Errorf("check run = %+v, want 555 concluded once, neutral, naming the deadline", cr)
			}
			if got := loadPR(t, store, 7); got.Run != nil {
				t.Errorf("stored Run = %+v, want it cleared", got.Run)
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

			gh := &gatetest.GitHub{}
			runner := &fakeRunner{}
			store := newStore(t, tc.state)
			svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

			if err := svc.HandleRunCompleted(t.Context(), tc.rc); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}
			if runs := gh.CheckRuns(); len(runs) != 0 || len(runner.collected) != 0 {
				t.Errorf("check runs = %+v, collects = %v, want none", runs, runner.collected)
			}
			if diff := cmp.Diff(tc.state, loadPR(t, store, 7)); diff != "" {
				t.Errorf("stored state (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleRunCompletedTransientCollectError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("download failed")
	gh := &gatetest.GitHub{}
	store := newStore(t, awaitingState())
	runner := &fakeRunner{collectErr: wantErr}
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); !errors.Is(err, wantErr) {
		t.Fatalf("HandleRunCompleted() = %v, want %v", err, wantErr)
	}
	if len(runner.collected) != 3 {
		t.Errorf("Collect attempts = %d, want 3", len(runner.collected))
	}
	cr := theCheckRun(t, gh)
	if len(cr.Updates) != 1 {
		t.Fatalf("check run = %+v, want one update", cr)
	}
	got := cr.Latest()
	if got.Conclusion != gate.ConclusionNeutral || got.Summary != "Pollux could not read the workflow run's result." {
		t.Errorf("check run = %+v, want neutral with the collect error", got)
	}
}

func TestHandleRunCompletedCollectRecovers(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	store := newStore(t, awaitingState())
	runner := &fakeRunner{collectErr: errors.New("502"), failFirst: 2, result: review.Result{Verdict: review.NoImpact{Reason: "fine"}}}
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}
	if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionSuccess {
		t.Errorf("check run = %+v, want one success update", cr)
	}
}

func TestHandlePullRequestActionsCreatesCheckBeforeDispatch(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("dispatch refused")
	gh := &gatetest.GitHub{Workflow: true, NextCheckRunID: 555}
	runner := &fakeRunner{err: wantErr}
	store := newStore(t)
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	cr := theCheckRun(t, gh)
	if cr.Created.Status != gate.StatusInProgress {
		t.Fatalf("created check run = %+v, want one in progress", cr.Created)
	}
	if cr.ID != 555 || len(cr.Updates) != 1 {
		t.Fatalf("check run = %+v, want the created check 555 concluded", cr)
	}
	got := cr.Latest()
	if got.Conclusion != gate.ConclusionNeutral || got.Summary != "The analysis failed unexpectedly." {
		t.Errorf("check run = %+v, want neutral with the generic cause, not the error text", got)
	}
}

func TestHandlePullRequestActionsSurvivesCancelAfterDispatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	gh := &gatetest.GitHub{Workflow: true, NextCheckRunID: 555}
	runner := &fakeRunner{started: review.Pending{RunID: 99, Nonce: "n1"}, onStart: cancel}
	store := &hookStore{Store: newStore(t)}
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

	if err := svc.HandlePullRequest(ctx, testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	last := loadPR(t, store.Store, 7)
	if last.CheckRunID != 555 || last.Run == nil || last.Run.RunID != 99 {
		t.Fatalf("stored state = %+v, want the check run and awaited run", last)
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

			gh := &gatetest.GitHub{}
			store := newStore(t, awaitingState())
			svc := newService(gh, store, gate.Runners{Actions: tt.runner}, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun(tt.conclude)); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}
			got := theCheckRun(t, gh).Latest().Summary
			if len(got) > tt.maxBytes || !strings.HasSuffix(got, "… (truncated)") || !utf8.ValidString(got) {
				t.Errorf("summary = %d bytes valid=%v, want <= %d, valid UTF-8, truncation marker", len(got), utf8.ValidString(got), tt.maxBytes)
			}
		})
	}
}

func proposalService(t *testing.T, gh *gatetest.GitHub, store gate.Store, verdict review.Verdict) {
	t.Helper()
	runner := &fakeRunner{started: review.Result{Verdict: verdict}}
	svc := newService(gh, store, gate.Runners{Server: runner}, nil)
	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
}

func TestHandlePullRequestFirstRunCreatesSummaryFirst(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	proposalService(t, gh, newStore(t), review.Proposals{proposal("docs/a.md", "A"), proposal("docs/b.md", "B")})

	want := []string{"CreateIssueComment", "CreateReviewComment", "CreateReviewComment", "EditIssueComment"}
	if diff := cmp.Diff(want, writeOrder(gh)); diff != "" {
		t.Errorf("write order (-want +got):\n%s", diff)
	}
	comments := gh.Comments()
	if comments[0].Kind != gate.CommentKindIssue {
		t.Fatalf("first comment kind = %s, want the summary", comments[0].Kind)
	}
	for _, c := range comments[1:] {
		if !strings.Contains(comments[0].Body, "[view]("+c.URL+")") {
			t.Errorf("summary missing link %s:\n%s", c.URL, comments[0].Body)
		}
	}
}

func TestHandlePullRequestRerunEditsInPlace(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	store := newStore(t)
	both := review.Proposals{proposal("docs/a.md", "A"), proposal("docs/b.md", "B")}

	proposalService(t, gh, store, both)
	if got, want := callsOf(gh), (commentCalls{createReview: 2, createIssue: 1, editIssue: 1}); got != want {
		t.Fatalf("first run: comment calls = %+v, want %+v", got, want)
	}

	proposalService(t, gh, store, both)
	if got, want := callsOf(gh), (commentCalls{createReview: 2, createIssue: 1, editReview: 2, editIssue: 2}); got != want || len(gh.Comments()) != 3 {
		t.Errorf("same re-run: comment calls = %+v with %d comments, want %+v with 3", got, len(gh.Comments()), want)
	}

	proposalService(t, gh, store, review.Proposals{both[0]})
	if c := gh.Comments(); len(c) != 3 || !strings.Contains(c[2].Body, "Outdated") || !strings.Contains(c[0].Body, "outdated") {
		t.Errorf("partial re-run comments = %+v, want comment 2 and summary outdated, none added", c)
	}

	proposalService(t, gh, store, review.NoImpact{Reason: "x"})
	if c := gh.Comments(); len(c) != 3 || strings.Contains(c[0].Body, "| open") {
		t.Errorf("no-impact re-run comments = %+v, want all outdated, none added", c)
	}
	if n := gh.CallCount("ListComments"); n != 4 {
		t.Errorf("ListComments calls = %d, want 1 per run (4 runs)", n)
	}
	runs := gh.CheckRuns()
	last := runs[len(runs)-1].Latest()
	if last.Conclusion != gate.ConclusionSuccess {
		t.Errorf("last check conclusion = %s, want success", last.Conclusion)
	}
}

func TestHandlePullRequestNoImpactFirstRunPostsNothing(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	proposalService(t, gh, newStore(t), review.NoImpact{Reason: "x"})
	if c := callsOf(gh); gh.CallCount("ListComments") != 0 || c.createReview+c.createIssue != 0 {
		t.Errorf("list calls = %d, comment calls = %+v, want none", gh.CallCount("ListComments"), c)
	}
}

func TestHandlePullRequestRecoversUnrecordedComments(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	p := proposal("docs/a.md", "A")
	id := gate.ProposalID(p.DocPath, p.Section)
	gh.AddComment(gate.CommentKindReview, "<!-- pollux-agent:proposal:"+id+" -->\n\nold")
	gh.AddComment(gate.CommentKindIssue, "<!-- pollux-agent:summary -->")

	proposalService(t, gh, newStore(t), review.Proposals{p})
	if got, want := callsOf(gh), (commentCalls{editReview: 1, editIssue: 1}); got != want || len(gh.Comments()) != 2 {
		t.Errorf("comment calls = %+v with %d comments, want %+v with 2", got, len(gh.Comments()), want)
	}
}

func TestHandlePullRequestOutdatesCommentsPostedByACrashedRun(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Fail: map[string]error{"EditIssueComment": errors.New("boom")}}
	store := newStore(t)
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	svc := newService(gh, store, gate.Runners{Server: runner}, nil).WithRetryBackoff(0)
	if err := svc.HandlePullRequest(t.Context(), testPR()); err == nil {
		t.Fatal("HandlePullRequest() = nil, want the summary edit error")
	}

	gh.Fail = nil
	proposalService(t, gh, store, review.NoImpact{Reason: "x"})
	if c := gh.Comments(); len(c) != 2 || !strings.Contains(c[1].Body, "Outdated") {
		t.Errorf("comments after no-impact run = %+v, want the crashed run's comment marked outdated", c)
	}
}

func TestHandleRunCompletedPostsComments(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := newStore(t, awaitingState())
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}

	if c := callsOf(gh); gh.CallCount("ListChangedFiles") != 1 || c.createReview != 1 || c.createIssue != 1 {
		t.Errorf("changed lists = %d, comment calls = %+v, want one of each", gh.CallCount("ListChangedFiles"), c)
	}
	if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionActionRequired {
		t.Errorf("check run = %+v, want one action_required update", cr)
	}
	if body := summaryBody(t, gh); !strings.Contains(body, "**pollux-agent** proposes 1 doc update.") || strings.Contains(body, "Re-run") {
		t.Errorf("summary = %q, want the singular proposal heading and no Re-run box", body)
	}
	got := loadPR(t, store, 7)
	if got.SummaryCommentID == 0 || len(got.Proposals) != 1 || got.Proposals[0].CommentID == 0 || got.Run != nil {
		t.Errorf("saved state = %+v, want comment IDs recorded and no awaited run", got)
	}
}

func TestHandleRunCompletedRetriesFailedPosts(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{NextCheckRunID: 5, Before: gatetest.FailNth("CreateReviewComment", 2, errors.New("CreateReviewComment failed")), Changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A"), proposal("docs/b.md", "B")}}}
	state := awaitingState()
	state.CheckRunID = 5
	store := newStore(t, state)
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil after the inline retry", err)
	}

	if c := callsOf(gh); c.createReview != 3 || len(gh.ReviewComments()) != 2 || c.createIssue != 1 {
		t.Errorf("review creates = %d (%d succeeded), summary creates = %d, want 3 attempts, 2 comments, 1 summary", c.createReview, len(gh.ReviewComments()), c.createIssue)
	}
	got := loadPR(t, store, 7)
	if got.Run != nil || got.CheckRunID != 5 || got.SummaryCommentID == 0 || len(got.Proposals) != 2 {
		t.Errorf("saved state = %+v, want no awaited run, check run 5, summary and 2 proposals", got)
	}
	summary := summaryBody(t, gh)
	if !strings.Contains(summary, "**pollux-agent** proposes 2 doc updates.") || strings.Contains(summary, "Re-run") {
		t.Errorf("summary = %q, want the plural proposal heading and no Re-run box", summary)
	}
}

func TestHandleRunCompletedGivesUpOnPersistentPostFailure(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{NextCheckRunID: 5, Fail: map[string]error{"CreateIssueComment": errors.New("boom")}, Changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	state := awaitingState()
	state.CheckRunID = 5
	store := newStore(t, state)
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err == nil {
		t.Fatal("HandleRunCompleted() = nil, want the summary failure after the retries")
	}
	if n := gh.CallCount("CreateIssueComment"); n != 3 {
		t.Errorf("summary create attempts = %d, want 3", n)
	}
	if saved := loadPR(t, store, 7); saved.Run == nil || saved.Run.Nonce != "n1" {
		t.Errorf("stored state = %+v, want the run re-armed with nonce n1", saved)
	}
}

func TestHandleRunCompletedChangedFilesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &gatetest.GitHub{Fail: map[string]error{"ListChangedFiles": wantErr}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := newStore(t, awaitingState())
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); !errors.Is(err, wantErr) {
		t.Fatalf("HandleRunCompleted() = %v, want wrapping %v", err, wantErr)
	}
	if runs := gh.CheckRuns(); len(runs) != 0 || callsOf(gh).createReview != 0 {
		t.Errorf("check runs = %+v, review creates = %d, want no writes", runs, callsOf(gh).createReview)
	}
	if diff := cmp.Diff(awaitingState(), loadPR(t, store, 7)); diff != "" {
		t.Errorf("stored state (-want +got):\n%s", diff)
	}
}

func TestHandleRunCompletedFailureKeepsProposals(t *testing.T) {
	t.Parallel()

	id := gate.ProposalID("docs/a.md", "A")
	state := awaitingState()
	state.SummaryCommentID = 2
	state.Proposals = []gate.ProposalState{{ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalOpen}}
	gh := &gatetest.GitHub{}
	store := newStore(t, state)
	svc := newService(gh, store, gate.Runners{Actions: &fakeRunner{}}, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}

	c := callsOf(gh)
	if gh.CallCount("ListChangedFiles") != 0 || c.editReview != 0 || c.createReview != 0 {
		t.Errorf("review comment calls = changed %d, edits %d, creates %d, want none", gh.CallCount("ListChangedFiles"), c.editReview, c.createReview)
	}
	if c.editIssue+c.createIssue != 1 {
		t.Errorf("summary writes = edits %d + creates %d, want 1", c.editIssue, c.createIssue)
	}
	got := loadPR(t, store, 7)
	if diff := cmp.Diff(state.Proposals, got.Proposals); diff != "" || got.Run != nil {
		t.Errorf("saved state = %+v, want proposals untouched and no awaited run (-want +got):\n%s", got, diff)
	}
}

func TestHandlePullRequestPRSkipSkipsAnalysis(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Workflow: true, NextCheckRunID: 888}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "x"}}}
	store := newStore(t, gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111",
		Skip: &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated", HeadSHA: "old111"},
	})
	svc := newService(gh, store, gate.Runners{Actions: runner, Server: runner}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if n := gh.CallCount("ListChangedFiles"); len(runner.calls) != 0 || n != 0 {
		t.Errorf("runner starts = %d, changed-file lists = %d, want no analysis", len(runner.calls), n)
	}
	cr := theCheckRun(t, gh)
	if run := cr.Created; run.HeadSHA != "abc123" || run.Conclusion != gate.ConclusionSuccess || !strings.Contains(run.Summary, "generated") {
		t.Errorf("created check run = %+v, want success on abc123 naming the reason", run)
	}
	if got := loadPR(t, store, 7); got.HeadSHA != "abc123" || got.CheckRunID != 888 || got.Skip == nil || got.Run != nil {
		t.Errorf("stored state = %+v, want head abc123, check run 888, the PR skip kept and no awaited run", got)
	}
}

func TestHandleRunCompletedAfterSkip(t *testing.T) {
	t.Parallel()

	skipped, _ := gate.OnSkip(awaitingState(), gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "typo"})

	t.Run("late run is ignored", func(t *testing.T) {
		t.Parallel()

		gh := &gatetest.GitHub{}
		runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{{DocPath: "docs/a.md", Reason: "x"}}}}
		svc := newService(gh, newStore(t, skipped), gate.Runners{Actions: runner}, nil)

		if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
			t.Fatalf("HandleRunCompleted() = %v, want nil", err)
		}
		if runs := gh.CheckRuns(); len(runs) != 0 || len(runner.collected) != 0 {
			t.Errorf("check runs = %+v, collects = %d, want the late run ignored", runs, len(runner.collected))
		}
	})

	t.Run("active skip beats the analysis result", func(t *testing.T) {
		t.Parallel()

		state := awaitingState()
		state.Skip = skipped.Skip
		gh := &gatetest.GitHub{}
		runner := &fakeRunner{result: review.Result{Verdict: review.NoImpact{Reason: "x"}}}
		store := newStore(t, state)
		svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

		if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err != nil {
			t.Fatalf("HandleRunCompleted() = %v, want nil", err)
		}
		if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionSuccess || !strings.Contains(cr.Latest().Summary, "typo") {
			t.Errorf("check run = %+v, want one update, the skip's success", cr)
		}
		if got := loadPR(t, store, 7); got.Run != nil {
			t.Errorf("stored Run = %+v, want no awaited run", got.Run)
		}
	})
}

func TestHandleDeadlineAfterSkip(t *testing.T) {
	t.Parallel()

	state := awaitingState()
	state.Run.Deadline = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state.Skip = &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "typo", HeadSHA: "abc123"}
	gh := &gatetest.GitHub{}
	svc := newService(gh, newStore(t, state), gate.Runners{}, nil)

	err := svc.HandleDeadline(t.Context(), gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}, "n1", state.Run.Deadline.Add(time.Hour))
	if err != nil {
		t.Fatalf("HandleDeadline() = %v, want nil", err)
	}
	if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionSuccess {
		t.Errorf("check run = %+v, want one update, the skip's success rather than neutral", cr)
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

			gh := &gatetest.GitHub{}
			store := newStore(t, gate.PRState{
				Owner: "acme", Repo: "widgets", Number: 7, Proposals: tt.stored,
			})
			proposalService(t, gh, store, tt.verdict)

			run := theCheckRun(t, gh).Latest()
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

			store := newStore(t, gate.PRState{
				InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7,
				HeadSHA: "abc1234567", PendingSkip: tt.ask,
			})
			var pendingAtNote *gate.SkipAsk
			gh := &gatetest.GitHub{}
			gh.Before = func(ctx context.Context, c gatetest.Call) error {
				if c.Method == "CreateIssueComment" {
					state, err := store.LoadPR(ctx, "acme", "widgets", 7)
					if err != nil {
						return fmt.Errorf("load pr 7: %w", err)
					}
					pendingAtNote = state.PendingSkip
				}
				return nil
			}
			svc := newService(gh, store, gate.Runners{}, nil)

			pr := testPR()
			pr.HeadSHA = tt.head
			for range 2 {
				if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
					t.Fatalf("HandlePullRequest() = %v", err)
				}
			}

			comments := gh.Comments()
			if len(comments) != tt.wantNotes {
				t.Fatalf("comments = %d, want %d", len(comments), tt.wantNotes)
			}
			if tt.wantNotes == 1 {
				want := "@dev, a new push arrived before your reason, so the skip for `abc1234` was cancelled. Tick **" + tt.wantLabel + "** again to skip the new head."
				if comments[0].Body != want {
					t.Errorf("note = %q, want %q", comments[0].Body, want)
				}
				if pendingAtNote != nil {
					t.Errorf("pending skip when the note was posted = %+v, want it already cleared by a save", pendingAtNote)
				}
			}
			if diff := cmp.Diff(tt.wantPending, loadPR(t, store, 7).PendingSkip); diff != "" {
				t.Errorf("PendingSkip (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandlePullRequestSkipCancellationNoteSurvivesFailedAnalysis(t *testing.T) {
	t.Parallel()

	store := newStore(t, gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7,
		HeadSHA: "abc1234567", PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit},
	})
	gh := &gatetest.GitHub{NextCheckRunID: 5}
	failure := &review.FailedError{Cause: review.CauseLimit, Err: errors.New("limit")}
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{err: failure}}, nil)
	pr := testPR()
	pr.HeadSHA = "def4567890"

	for range 2 {
		if err := svc.HandlePullRequest(t.Context(), pr); !errors.Is(err, failure) {
			t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, failure)
		}
	}

	if notes := countComments(gh.Comments(), "was cancelled"); notes != 1 {
		t.Errorf("cancellation notes = %d, want 1 even though the analysis failed", notes)
	}
}

func TestHandlePullRequestServerStartErrorEndsNeutral(t *testing.T) {
	t.Parallel()

	failure := &review.FailedError{Cause: review.CauseLimit, Err: errors.New("model said: leak-me")}
	gh := &gatetest.GitHub{NextCheckRunID: 555}
	store := newStore(t)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{err: failure}}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); !errors.Is(err, failure) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, failure)
	}

	cr := theCheckRun(t, gh)
	if cr.Created.Status != gate.StatusInProgress {
		t.Fatalf("created check run = %+v, want one in progress", cr.Created)
	}
	if cr.ID != 555 || len(cr.Updates) != 1 {
		t.Fatalf("check run = %+v, want the created check 555 concluded", cr)
	}
	if got := cr.Latest(); got.Conclusion != gate.ConclusionNeutral || got.Title != "Analysis failed" || got.Summary != "The analysis hit its step or token limit." {
		t.Errorf("check run = %+v, want neutral Analysis failed with the fixed limit cause", got)
	}

	body := summaryBody(t, gh)
	if !strings.Contains(body, "The analysis hit its step or token limit.") ||
		!strings.Contains(body, "- [ ] Re-run analysis\n") || strings.Contains(body, "leak-me") {
		t.Errorf("summary = %q, want the fixed cause and an unticked Re-run box", body)
	}
	last := loadPR(t, store, 7)
	if last.Run != nil || last.SummaryCommentID != gh.Comments()[0].ID {
		t.Errorf("stored state = %+v, want the run cleared and the summary comment recorded", last)
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

			gh := &gatetest.GitHub{PullRequest: gate.PullRequest{BaseSHA: "tip1", HeadSHA: "new222", Open: true}, MergeBaseSHA: "base1"}
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			svc := newService(gh, newStore(t, stored), gate.Runners{Server: runner}, nil)

			if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: ref, SummaryCommentID: tc.comment}); err != nil {
				t.Fatalf("HandleRerun() = %v, want nil", err)
			}
			if !tc.want {
				if runs := gh.CheckRuns(); len(runner.calls) != 0 || len(runs) != 0 {
					t.Errorf("runner calls = %d, check runs = %+v, want none", len(runner.calls), runs)
				}
				return
			}
			if len(runner.calls) != 1 || runner.calls[0].HeadSHA != "new222" || runner.calls[0].BaseSHA != "base1" {
				t.Errorf("runner calls = %+v, want one on the current head new222 and merge base base1, not the base tip", runner.calls)
			}
			if cr := theCheckRun(t, gh); cr.Created.HeadSHA != "new222" || len(cr.Updates) != 1 {
				t.Errorf("check run = %+v, want a new check run on new222, concluded", cr)
			}
		})
	}
}

func TestHandleRerunUnusableResultIsReportedNotRetried(t *testing.T) {
	t.Parallel()

	stored := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111", SummaryCommentID: 3}
	gh := &gatetest.GitHub{PullRequest: gate.PullRequest{BaseSHA: "tip1", HeadSHA: "new222", Open: true}}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{}}}
	svc := newService(gh, newStore(t, stored), gate.Runners{Server: runner}, nil)

	err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}, SummaryCommentID: 3})
	if err != nil {
		t.Fatalf("HandleRerun() = %v, want nil: the failure is already reported", err)
	}
	if got := theCheckRun(t, gh).Latest(); got.Conclusion != gate.ConclusionNeutral || got.Title != "Analysis failed" {
		t.Errorf("check run = %+v, want neutral Analysis failed", got)
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

			gh := &gatetest.GitHub{PullRequest: tc.pr}
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			store := newStore(t, tc.stored)
			svc := newService(gh, store, gate.Runners{Server: runner}, nil)

			if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: ref}); err != nil {
				t.Fatalf("HandleRerun() = %v, want nil", err)
			}
			if runs := gh.CheckRuns(); len(runner.calls) != 0 || len(runs) != 0 {
				t.Errorf("runner calls = %d, check runs = %+v, want none", len(runner.calls), runs)
			}
			if diff := cmp.Diff(tc.stored, loadPR(t, store, 7)); diff != "" {
				t.Errorf("stored state (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleRerunSupersedesOverdueAnalysisOfSameHead(t *testing.T) {
	t.Parallel()

	stored := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "new222", CheckRunID: 5, Run: &gate.AwaitingRun{Nonce: "n", Deadline: time.Now().Add(-time.Minute)}}
	gh := &gatetest.GitHub{PullRequest: gate.PullRequest{HeadSHA: "new222", Open: true}}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := newService(gh, newStore(t, stored), gate.Runners{Server: runner}, nil)

	if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}}); err != nil {
		t.Fatalf("HandleRerun() = %v, want nil", err)
	}
	if len(runner.calls) != 1 {
		t.Errorf("runner calls = %d, want 1 on the overdue head", len(runner.calls))
	}
	if runs := gh.CheckRuns(); len(runs) < 1 || runs[0].ID != 5 || len(runs[0].Updates) != 1 || runs[0].Latest().Title != "Superseded" {
		t.Errorf("check runs = %+v, want the old check run 5 closed as superseded first", runs)
	}
}

func TestHandlePullRequestServerRunnerRetriesFailedPosts(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Before: gatetest.FailNth("CreateReviewComment", 1, errors.New("CreateReviewComment failed")), Changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := newStore(t)
	svc := newService(gh, store, gate.Runners{Server: runner}, nil).WithRetryBackoff(0)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil after the inline retry", err)
	}
	if n := gh.CallCount("CreateReviewComment"); n != 2 || len(gh.ReviewComments()) != 1 {
		t.Errorf("review creates = %d (%d succeeded), want 2 attempts, 1 comment", n, len(gh.ReviewComments()))
	}
	for _, cr := range gh.CheckRuns() {
		for _, u := range cr.Updates {
			if u.Title == "Analysis failed" || u.Conclusion != gate.ConclusionActionRequired {
				t.Errorf("check run update = %+v, want only action_required", u)
			}
		}
	}
	if got := loadPR(t, store, 7); got.Run != nil || len(got.Proposals) != 1 || got.Proposals[0].CommentID == 0 {
		t.Errorf("saved state = %+v, want no awaited run and the proposal comment recorded", got)
	}
}

func TestHandleRerunSupersedesAnalysisOfOlderHead(t *testing.T) {
	t.Parallel()

	stored := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111", CheckRunID: 5, Run: &gate.AwaitingRun{Nonce: "n"}}
	gh := &gatetest.GitHub{PullRequest: gate.PullRequest{HeadSHA: "new222", Open: true}}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	svc := newService(gh, newStore(t, stored), gate.Runners{Server: runner}, nil)

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

				gh := &gatetest.GitHub{NextCheckRunID: 555, Changed: tc.changed, Workflow: kind == "actions"}
				runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
				runners := gate.Runners{Server: runner}
				if kind == "actions" {
					runners = gate.Runners{Actions: runner}
				}
				svc := newService(gh, newStore(t), runners, nil)

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
				cr := theCheckRun(t, gh)
				if len(cr.Updates) != 1 {
					t.Fatalf("check run = %+v, want one update", cr)
				}
				if got := cr.Latest(); got.Conclusion != gate.ConclusionNeutral || got.Title != "PR too large to analyze" || got.Summary != tc.want {
					t.Errorf("check run = %+v, want neutral %q with summary %q", got, "PR too large to analyze", tc.want)
				}
				if body := summaryBody(t, gh); !strings.Contains(body, tc.want) || !strings.Contains(body, "- [ ] Re-run analysis\n") {
					t.Errorf("summary = %q, want the limit and an unticked Re-run box", body)
				}
			})
		}
	}
}

func TestPostCommentsFailureRearmsRunForDeadlineSweep(t *testing.T) {
	t.Parallel()

	editErr := errors.New("edit failed")
	ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	sweep := func(t *testing.T, gh *gatetest.GitHub, store gate.Store, svc *gate.Service, nonce string) {
		t.Helper()

		gh.Fail = nil
		last := loadPR(t, store, 7)
		if last.Run == nil || last.Run.Nonce != nonce {
			t.Fatalf("stored state = %+v, want the run armed with nonce %q", last, nonce)
		}
		var before gatetest.CheckRun
		for _, cr := range gh.CheckRuns() {
			if cr.ID == last.CheckRunID {
				before = cr
			}
		}
		if err := svc.HandleDeadline(t.Context(), ref, nonce, last.Run.Deadline.Add(time.Second)); err != nil {
			t.Fatalf("HandleDeadline() = %v, want nil", err)
		}
		var after gatetest.CheckRun
		for _, cr := range gh.CheckRuns() {
			if cr.ID == last.CheckRunID {
				after = cr
			}
		}
		if len(after.Updates) != len(before.Updates)+1 {
			t.Fatalf("check run %d updates = %+v, want the deadline to conclude it once more", last.CheckRunID, after.Updates)
		}
		if got := after.Latest(); got.Conclusion != gate.ConclusionNeutral || got.Title != "Analysis failed" {
			t.Errorf("check run = %+v, want neutral %q", got, "Analysis failed")
		}
		if summary := summaryBody(t, gh); !strings.Contains(summary, "- [ ] Re-run analysis\n") {
			t.Errorf("summary = %q, want the failure summary with an unticked Re-run box", summary)
		}
		if final := loadPR(t, store, 7); final.Run != nil {
			t.Errorf("final state = %+v, want the run cleared", final)
		}
	}

	t.Run("server result", func(t *testing.T) {
		t.Parallel()

		gh := &gatetest.GitHub{}
		store := newStore(t)
		proposalService(t, gh, store, review.Proposals{proposal("docs/a.md", "A")})
		gh.Fail = map[string]error{"EditReviewComment": editErr}

		runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
		svc := newService(gh, store, gate.Runners{Server: runner}, nil).WithRetryBackoff(0)
		if err := svc.HandlePullRequest(t.Context(), testPR()); !errors.Is(err, editErr) {
			t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, editErr)
		}
		sweep(t, gh, store, svc, fmt.Sprintf("check-%d", loadPR(t, store, 7).CheckRunID))
	})
	t.Run("run completed", func(t *testing.T) {
		t.Parallel()

		gh := &gatetest.GitHub{Changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}, Patch: "@@"}}}
		runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
		store := newStore(t, awaitingState())
		svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)
		if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
			t.Fatalf("HandleRunCompleted() = %v, want nil", err)
		}
		gh.Fail = map[string]error{"EditReviewComment": editErr}
		if err := store.SavePR(t.Context(), awaitingState()); err != nil {
			t.Fatalf("SavePR() = %v, want nil", err)
		}
		if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); !errors.Is(err, editErr) {
			t.Fatalf("HandleRunCompleted() = %v, want wrapping %v", err, editErr)
		}
		sweep(t, gh, store, svc, "n1")
	})
}

func TestPostCommentsTransientFailureEndsConcluded(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}, Patch: "@@"}}}
	gh.Before = gatetest.FailFirst("CreateIssueComment", 1, errors.New("CreateIssueComment failed"))
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := newStore(t, awaitingState())
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)
	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil after the retry", err)
	}
	if last := loadPR(t, store, 7); last.Run != nil {
		t.Errorf("stored state = %+v, want the run cleared", last)
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
			gh := &gatetest.GitHub{}
			store := newStore(t, state)
			svc := newService(gh, store, gate.Runners{}, nil)

			ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
			if err := svc.HandleDeadline(t.Context(), ref, "n1", deadline.Add(time.Second)); err != nil {
				t.Fatalf("HandleDeadline() = %v, want nil", err)
			}
			if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Summary != tc.want {
				t.Errorf("check run = %+v, want one update with summary %q", cr, tc.want)
			}
			if body := summaryBody(t, gh); !strings.Contains(body, tc.want) || !strings.Contains(body, "- [ ] Re-run analysis\n") {
				t.Errorf("summary = %q, want the cause and an unticked Re-run box", body)
			}
			if last := loadPR(t, store, 7); last.Run != nil || last.SummaryCommentID == 0 {
				t.Errorf("stored state = %+v, want the run cleared and the summary recorded", last)
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
				gh := &gatetest.GitHub{NextCheckRunID: 7}
				tc.runner.onStart = func() { time.Sleep(2 * time.Minute) }
				svc := newService(gh, newStore(t), gate.Runners{Server: tc.runner}, nil)

				_ = svc.HandlePullRequest(t.Context(), testPR())

				if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || cr.Latest().Conclusion != tc.want {
					t.Errorf("check run = %+v, want the check concluded %s after a 2-minute analysis", cr, tc.want)
				}
			})
		})
	}
}
