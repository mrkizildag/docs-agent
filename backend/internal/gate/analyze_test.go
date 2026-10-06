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
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestHandlePullRequestNoImpact(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: false}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "docs already cover this"}}}
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: &fakeRunner{}}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{}, nil, nil)

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

func TestHandlePullRequestUnusableResultReportsFailureWithCause(t *testing.T) {
	t.Parallel()

	for name, verdict := range map[string]review.Verdict{"empty proposals": review.Proposals{}, "no verdict": nil} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			store := &fakeStore{}
			runner := &fakeRunner{started: review.Result{Verdict: verdict}}
			svc := newService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)

			if err := svc.HandlePullRequest(t.Context(), testPR()); err == nil {
				t.Fatal("HandlePullRequest() = nil, want the failure")
			}
			if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionNeutral || gh.updates[0].run.Title != "Analysis failed" {
				t.Fatalf("updates = %+v, want one neutral Analysis failed", gh.updates)
			}
			cause := gh.updates[0].run.Summary
			if len(gh.comments) != 1 || !strings.Contains(gh.comments[0].Body, cause) ||
				!strings.Contains(gh.comments[0].Body, "- [ ] Re-run analysis\n") {
				t.Errorf("comments = %+v, want one summary stating %q with an unticked Re-run box", gh.comments, cause)
			}
			if store.saved == nil || store.saved.FailureCause != cause || store.saved.Run != nil {
				t.Errorf("saved state = %+v, want FailureCause %q and no awaited run", store.saved, cause)
			}
		})
	}
}

func TestHandlePullRequestRunnerStartError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{}
	runner := &fakeRunner{err: wantErr}
	svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{}, nil, nil)

	err := svc.HandlePullRequest(t.Context(), testPR())
	if !errors.Is(err, wantErr) {
		t.Fatalf("HandlePullRequest() = %v, want wrapping %v", err, wantErr)
	}
	if len(store.saveCalls) != 0 {
		t.Errorf("SavePR calls = %d, want 0 after CreateCheckRun error", len(store.saveCalls))
	}
}

func TestHandlePullRequestSavesState(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	svc := newService(gh, nil, store, gate.Runners{}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{loadErr: wantErr}, gate.Runners{}, nil, nil)

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
	svc := newService(&fakeGitHub{}, nil, &fakeStore{saveErr: wantErr}, gate.Runners{}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
	want := gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123", CheckRunID: 555,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1", Deadline: deadline},
	}
	if diff := cmp.Diff(want, store.saveCalls[1]); diff != "" {
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

			gh := &fakeGitHub{}
			store := &fakeStore{stored: awaitingState()}
			svc := newService(gh, nil, store, gate.Runners{Actions: tc.runner}, nil, nil)

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
			svc := newService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: tc.runner}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{}, nil, nil)

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
			svc := newService(gh, nil, store, gate.Runners{}, nil, nil)

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
			svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithRetryBackoff(0)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithRetryBackoff(0)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
			svc := newService(gh, nil, store, gate.Runners{Actions: tt.runner}, nil, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun(tt.conclude)); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}
			got := gh.updates[0].run.Summary
			if len(got) > tt.maxBytes || !strings.HasSuffix(got, "… (truncated)") || !utf8.ValidString(got) {
				t.Errorf("summary = %d bytes valid=%v, want <= %d, valid UTF-8, truncation marker", len(got), utf8.ValidString(got), tt.maxBytes)
			}
		})
	}
}

func proposalService(t *testing.T, gh *fakeGitHub, store *fakeStore, verdict review.Verdict) {
	t.Helper()
	runner := &fakeRunner{started: review.Result{Verdict: verdict}}
	svc := newService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)
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
	if len(gh.comments) != 3 || !strings.Contains(gh.comments[2].Body, "Outdated") || !strings.Contains(gh.comments[0].Body, "outdated") {
		t.Errorf("partial re-run comments = %+v, want comment 2 and summary outdated, none added", gh.comments)
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

func TestHandlePullRequestOutdatesCommentsPostedByACrashedRun(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{editIssueErr: errors.New("boom")}
	store := &fakeStore{}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	svc := newService(gh, nil, store, gate.Runners{Server: runner}, nil, nil).WithRetryBackoff(0)
	if err := svc.HandlePullRequest(t.Context(), testPR()); err == nil {
		t.Fatal("HandlePullRequest() = nil, want the summary edit error")
	}

	gh.editIssueErr = nil
	proposalService(t, gh, store, review.NoImpact{Reason: "x"})
	if len(gh.comments) != 2 || !strings.Contains(gh.comments[1].Body, "Outdated") {
		t.Errorf("comments after no-impact run = %+v, want the crashed run's comment marked outdated", gh.comments)
	}
}

func TestHandleRunCompletedPostsComments(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := &fakeStore{stored: awaitingState()}
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithRetryBackoff(0)

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
	store := &fakeStore{stored: state}
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithRetryBackoff(0)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithRetryBackoff(0)

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

func TestHandleRunCompletedChangedFilesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	gh := &fakeGitHub{changedErr: wantErr}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	store := &fakeStore{stored: awaitingState()}
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Actions: &fakeRunner{}}, nil, nil)

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

func TestHandlePullRequestPRSkipSkipsAnalysis(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{workflowExists: true, checkRunID: 888}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "x"}}}
	store := &fakeStore{stored: gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111",
		Skip: &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated", HeadSHA: "old111"},
	}}
	svc := newService(gh, nil, store, gate.Runners{Actions: runner, Server: runner}, nil, nil)

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
		svc := newService(gh, nil, &fakeStore{stored: skipped}, gate.Runners{Actions: runner}, nil, nil)

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
		svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{stored: state}, gate.Runners{}, nil, nil)

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
			svc := newService(gh, nil, store, gate.Runners{}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Server: &fakeRunner{err: failure}}, nil, nil)
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
	svc := newService(gh, nil, store, gate.Runners{Server: &fakeRunner{err: failure}}, nil, nil)

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
			svc := newService(gh, nil, &fakeStore{stored: stored}, gate.Runners{Server: runner}, nil, nil)

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
			svc := newService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, &fakeStore{stored: stored}, gate.Runners{Server: runner}, nil, nil)

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
	svc := newService(gh, nil, store, gate.Runners{Server: runner}, nil, nil).WithRetryBackoff(0)

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
	svc := newService(gh, nil, &fakeStore{stored: stored}, gate.Runners{Server: runner}, nil, nil)

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
				svc := newService(gh, nil, &fakeStore{}, runners, nil, nil)

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
		svc := newService(gh, nil, store, gate.Runners{Server: runner}, nil, nil).WithRetryBackoff(0)
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
		svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithRetryBackoff(0)
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
	store := &fakeStore{stored: awaitingState()}
	svc := newService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithRetryBackoff(0)
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
			svc := newService(gh, nil, store, gate.Runners{}, nil, nil)

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
				svc := newService(gh, nil, &fakeStore{}, gate.Runners{Server: tc.runner}, nil, nil)

				_ = svc.HandlePullRequest(t.Context(), testPR())

				if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != tc.want {
					t.Errorf("updates = %+v, want the check concluded %s after a 2-minute analysis", gh.updates, tc.want)
				}
			})
		})
	}
}
