package gate_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestScaffoldTransitions(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	run := &gate.AwaitingRun{RunID: 5, Nonce: "n", Deadline: deadline}
	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	idle := gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldIdle, Attempt: 1}
	writing := gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldWriting, Attempt: 1, BaseSHA: "base"}
	awaiting := gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldAwaiting, Attempt: 1, BaseSHA: "base", Run: run}
	written := gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "base", Files: &files}
	pr := gate.ScaffoldPR{Number: 4, URL: "https://gh/pull/4"}

	tests := []struct {
		name string
		got  gate.ScaffoldState
		want gate.ScaffoldState
	}{
		{name: "start", got: gate.OnScaffoldStart(idle, "base"), want: writing},
		{name: "started", got: gate.OnScaffoldStarted(writing, review.Pending{RunID: 5, Nonce: "n", Deadline: deadline}), want: awaiting},
		{name: "written", got: gate.OnScaffoldWritten(writing, files), want: written},
		{name: "written from the awaited run", got: gate.OnScaffoldWritten(awaiting, files), want: written},
		{
			name: "opened",
			got:  gate.OnScaffoldOpened(written, pr),
			want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldOpened, Attempt: 1, BaseSHA: "base", Files: &files, PRNumber: 4, PRURL: pr.URL},
		},
		{name: "failed while writing", got: gate.OnScaffoldFailed(writing), want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldIdle, Attempt: 2, Failures: 1, BaseSHA: "base"}},
		{name: "failed while awaiting", got: gate.OnScaffoldFailed(awaiting), want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldIdle, Attempt: 2, Failures: 1, BaseSHA: "base"}},
		{name: "failed after writing keeps files", got: gate.OnScaffoldFailed(written), want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldWritten, Attempt: 2, Failures: 1, BaseSHA: "base", Files: &files}},
		{
			name: "the third failure gives up",
			got:  gate.OnScaffoldFailed(gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldWriting, Attempt: 2, Failures: 2, BaseSHA: "base"}),
			want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldGaveUp, Attempt: 3, Failures: 3, BaseSHA: "base"},
		},
		{
			name: "the third failure after writing gives up and keeps the files",
			got:  gate.OnScaffoldFailed(gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldWritten, Attempt: 2, Failures: 2, BaseSHA: "base", Files: &files}),
			want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldGaveUp, Attempt: 3, Failures: 3, BaseSHA: "base", Files: &files},
		},
		{
			name: "a failure after repeated docs-present conclusions stays below the cap",
			got:  gate.OnScaffoldFailed(gate.OnScaffoldDocsPresent(gate.OnScaffoldDocsPresent(gate.OnScaffoldDocsPresent(gate.OnScaffoldDocsPresent(writing))))),
			want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldIdle, Attempt: 6, Failures: 1, BaseSHA: "base"},
		},
		{name: "docs present", got: gate.OnScaffoldDocsPresent(written), want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldIdle, Attempt: 2, BaseSHA: "base"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(tc.want, tc.got); diff != "" {
				t.Errorf("transition (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMatchesScaffoldRunAndOverdue(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	awaiting := gate.ScaffoldState{Phase: gate.ScaffoldAwaiting, Run: &gate.AwaitingRun{RunID: 5, Nonce: "n", Deadline: deadline}}
	idle := gate.ScaffoldState{Phase: gate.ScaffoldIdle}

	tests := []struct {
		name        string
		state       gate.ScaffoldState
		runID       int64
		now         time.Time
		wantMatch   bool
		wantOverdue bool
	}{
		{name: "the awaited run before its deadline", state: awaiting, runID: 5, now: deadline.Add(-time.Minute), wantMatch: true},
		{name: "the awaited run past its deadline", state: awaiting, runID: 5, now: deadline.Add(time.Minute), wantMatch: true, wantOverdue: true},
		{name: "another run", state: awaiting, runID: 6, now: deadline.Add(time.Minute), wantOverdue: true},
		{name: "run zero", state: awaiting, runID: 0, now: deadline},
		{name: "nothing awaited", state: idle, runID: 5, now: deadline.Add(time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := gate.MatchesScaffoldRun(tc.state, gate.RunCompleted{RunID: tc.runID}); got != tc.wantMatch {
				t.Errorf("MatchesScaffoldRun() = %v, want %v", got, tc.wantMatch)
			}
			if got := gate.ScaffoldOverdue(tc.state, tc.now); got != tc.wantOverdue {
				t.Errorf("ScaffoldOverdue() = %v, want %v", got, tc.wantOverdue)
			}
		})
	}
}

type fakeScaffoldQueue struct{ attempts []int }

func (q *fakeScaffoldQueue) EnqueueScaffold(_ context.Context, _ gate.RepoRef, attempt int) error {
	q.attempts = append(q.attempts, attempt)
	return nil
}

func TestHandlePullRequestWithoutDocs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		runners      gate.Runners
		wantSummary  string
		wantEnqueued int
	}{
		{name: "no runner points at the setup guide", wantSummary: "docs/guides/setup.md"},
		{name: "a runner requests the scaffold", runners: gate.Runners{Server: &fakeRunner{}}, wantSummary: "writing a starting docs/ folder", wantEnqueued: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &gatetest.GitHub{NoDocs: true, NextCheckRunID: 7}
			store := newStore(t)
			queue := &fakeScaffoldQueue{}
			svc := newService(gh, store, tc.runners, queue)

			if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
				t.Fatalf("HandlePullRequest() = %v, want nil", err)
			}

			got := theCheckRun(t, gh).Created
			if got.Conclusion != gate.ConclusionNeutral || got.Title != "No docs/ folder" || !strings.Contains(got.Summary, tc.wantSummary) {
				t.Errorf("check run = %+v, want neutral \"No docs/ folder\" mentioning %q", got, tc.wantSummary)
			}
			if n := gh.CallCount("ListChangedFiles"); n != 0 {
				t.Errorf("listed changed files %d times, want 0: no review analysis without docs/", n)
			}
			if len(queue.attempts) != tc.wantEnqueued {
				t.Errorf("enqueued scaffold jobs = %v, want %d", queue.attempts, tc.wantEnqueued)
			}
			if saved := loadPR(t, store, 7); saved.CheckRunID != 7 {
				t.Errorf("stored state = %+v, want the check run id 7", saved)
			}
		})
	}
}

// scaffoldRunner is an Actions runner whose scaffold start and collect are scripted.
type scaffoldRunner struct {
	*fakeRunner
	started    review.ScaffoldStarted
	files      review.Scaffold
	collectErr error
	collects   []review.Completion
}

func (r *scaffoldRunner) StartScaffold(context.Context, review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	return r.started, nil
}

func (r *scaffoldRunner) CollectScaffold(_ context.Context, c review.Completion) (review.Scaffold, error) {
	r.collects = append(r.collects, c)
	return r.files, r.collectErr
}

func awaitingScaffold(deadline time.Time) gate.ScaffoldState {
	return gate.ScaffoldState{
		Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldAwaiting, Attempt: 1, BaseSHA: "tip",
		Run: &gate.AwaitingRun{RunID: 5, Nonce: "n", Deadline: deadline},
	}
}

func TestHandleScaffold_StartsTheActionsRun(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	runner := &scaffoldRunner{fakeRunner: &fakeRunner{}, started: review.Pending{RunID: 5, Nonce: "n", Deadline: deadline}}
	gh := &gatetest.GitHub{NoDocs: true, Workflow: true}
	store := newScaffoldStore(t, gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1})
	svc := newService(gh, store, gate.Runners{Actions: runner}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if diff := cmp.Diff(awaitingScaffold(deadline), loadScaffold(t, store)); diff != "" {
		t.Errorf("state (-want +got):\n%s", diff)
	}
	if len(gh.Branches) != 0 || len(gh.PullRequests()) != 0 || len(gh.Committed()) != 0 {
		t.Errorf("branches %v, pull requests %v, commits %v, want none until the run completes", gh.Branches, gh.PullRequests(), gh.Committed())
	}

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() while awaiting = %v, want nil", err)
	}
	if diff := cmp.Diff(awaitingScaffold(deadline), loadScaffold(t, store)); diff != "" {
		t.Errorf("state after a job that finds the run awaited (-want +got):\n%s", diff)
	}
}

func TestHandleScaffoldRun(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	invalid := &review.InvalidResultError{Cause: errors.New("bad docs")}
	idleAfterFailure := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 2, Failures: 1, BaseSHA: "tip"}

	tests := []struct {
		name        string
		state       gate.ScaffoldState
		rc          gate.RunCompleted
		collectErr  error
		wantErr     bool
		wantState   *gate.ScaffoldState // nil: the state is left as it was
		wantCollect int
		wantPR      bool
	}{
		{
			name: "a successful run opens the pull request", state: awaitingScaffold(deadline),
			rc: gate.RunCompleted{RunID: 5, Conclusion: "success"}, wantCollect: 1, wantPR: true,
		},
		{
			name: "a failed run fails the attempt", state: awaitingScaffold(deadline),
			rc: gate.RunCompleted{RunID: 5, Conclusion: "failure"}, wantErr: true, wantState: &idleAfterFailure,
		},
		{
			name: "a cancelled run fails the attempt", state: awaitingScaffold(deadline),
			rc: gate.RunCompleted{RunID: 5, Conclusion: "cancelled"}, wantErr: true, wantState: &idleAfterFailure,
		},
		{
			name: "an invalid result fails the attempt", state: awaitingScaffold(deadline),
			rc: gate.RunCompleted{RunID: 5, Conclusion: "success"}, collectErr: invalid, wantErr: true, wantState: &idleAfterFailure, wantCollect: 1,
		},
		{
			name: "a result that cannot be read is retried", state: awaitingScaffold(deadline),
			rc: gate.RunCompleted{RunID: 5, Conclusion: "success"}, collectErr: errors.New("network"), wantErr: true, wantCollect: 3,
		},
		{name: "another run is ignored", state: awaitingScaffold(deadline), rc: gate.RunCompleted{RunID: 6, Conclusion: "success"}},
		{
			name:  "a repo awaiting nothing ignores the run",
			state: gate.ScaffoldState{Owner: "acme", Repo: "widgets", Phase: gate.ScaffoldIdle},
			rc:    gate.RunCompleted{RunID: 5, Conclusion: "success"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := &scaffoldRunner{fakeRunner: &fakeRunner{}, files: files, collectErr: tc.collectErr}
			gh := &gatetest.GitHub{NoDocs: true, Branches: map[string]gate.Commit{"pollux-agent/docs-scaffold": {SHA: "tip"}}}
			store := newScaffoldStore(t, tc.state, 11)
			svc := newService(gh, store, gate.Runners{Actions: runner}, nil).WithRetryBackoff(0)
			tc.rc.Owner, tc.rc.Repo = "acme", "widgets"

			err := svc.HandleScaffoldRun(t.Context(), tc.rc)

			if (err != nil) != tc.wantErr {
				t.Fatalf("HandleScaffoldRun() = %v, want error %v", err, tc.wantErr)
			}
			if len(runner.collects) != tc.wantCollect {
				t.Errorf("CollectScaffold calls = %d, want %d", len(runner.collects), tc.wantCollect)
			}
			if tc.wantCollect > 0 {
				want := review.Completion{InstallationID: 42, Owner: "acme", Repo: "widgets", HeadSHA: "tip", RunID: 5, Nonce: "n"}
				if diff := cmp.Diff(want, runner.collects[0]); diff != "" {
					t.Errorf("completion (-want +got):\n%s", diff)
				}
			}
			state := loadScaffold(t, store)
			switch {
			case tc.wantPR:
				if state.Phase != gate.ScaffoldOpened || state.PRURL != "https://gh/pull/9" || state.Run != nil || state.Files == nil || *state.Files != files {
					t.Errorf("state = %+v, want Opened with the PR, the files and no run", state)
				}
				if prs := gh.PullRequests(); len(prs) != 1 {
					t.Errorf("pull requests = %d, want 1", len(prs))
				}
				commits := gh.Committed()
				if len(commits) != 1 {
					t.Fatalf("commits = %+v, want one", commits)
				}
				if diff := cmp.Diff(scaffoldFiles(), commits[0].Files); diff != "" {
					t.Errorf("committed files (-want +got):\n%s", diff)
				}
				if cr := checkRunByID(gh, 11); len(cr.Updates) != 1 || !strings.Contains(cr.Latest().Summary, "https://gh/pull/9") {
					t.Errorf("check run 11 = %+v, want one update linking the pull request", cr)
				}
			case tc.wantState != nil:
				if diff := cmp.Diff(*tc.wantState, state); diff != "" {
					t.Errorf("state (-want +got):\n%s", diff)
				}
			default:
				if diff := cmp.Diff(tc.state, state); diff != "" {
					t.Errorf("state, want it left as it was (-want +got):\n%s", diff)
				}
			}
			if !tc.wantPR && (len(gh.PullRequests()) != 0 || len(gh.Committed()) != 0) {
				t.Errorf("pull requests %v, commits %v, want none", gh.PullRequests(), gh.Committed())
			}
		})
	}
}

func TestHandleScaffoldDeadline(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	failed := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 2, Failures: 1, BaseSHA: "tip"}

	tests := []struct {
		name      string
		state     gate.ScaffoldState
		nonce     string
		now       time.Time
		wantState *gate.ScaffoldState
	}{
		{name: "an overdue run fails the attempt", state: awaitingScaffold(deadline), nonce: "n", now: deadline.Add(time.Minute), wantState: &failed},
		{name: "a run before its deadline is left", state: awaitingScaffold(deadline), nonce: "n", now: deadline.Add(-time.Minute)},
		{name: "another nonce is left", state: awaitingScaffold(deadline), nonce: "old", now: deadline.Add(time.Minute)},
		{name: "a repo awaiting nothing is left", state: gate.ScaffoldState{Owner: "acme", Repo: "widgets", Phase: gate.ScaffoldWritten}, nonce: "n", now: deadline.Add(time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newScaffoldStore(t, tc.state)
			svc := newService(&gatetest.GitHub{}, store, gate.Runners{}, nil)

			if err := svc.HandleScaffoldDeadline(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}, tc.nonce, tc.now); err != nil {
				t.Fatalf("HandleScaffoldDeadline() = %v, want nil", err)
			}

			want := tc.state
			if tc.wantState != nil {
				want = *tc.wantState
			}
			if diff := cmp.Diff(want, loadScaffold(t, store)); diff != "" {
				t.Errorf("state (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleScaffold_DocsPresentTellsTheWaiters(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	store := newScaffoldStore(t, gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1}, 11, 12)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if runs := gh.CheckRuns(); len(runs) != 2 || updateCount(gh) != 2 {
		t.Fatalf("check runs = %+v, want 2 updated once each", runs)
	}
	for _, cr := range gh.CheckRuns() {
		run := cr.Latest()
		if run.Conclusion != gate.ConclusionNeutral || run.Title != "No docs/ folder" ||
			!strings.Contains(run.Summary, "already has a docs/ folder") || !strings.Contains(run.Summary, "Merge or rebase") || strings.Contains(run.Summary, "writing") {
			t.Errorf("check run %d = %+v, want neutral \"No docs/ folder\" saying the default branch has docs/ and to merge or rebase", cr.ID, run)
		}
	}
	if state := loadScaffold(t, store); state.Phase != gate.ScaffoldIdle || state.Attempt != 2 {
		t.Errorf("state = %+v, want Idle at attempt 2", state)
	}
}

func TestLinkWaiters_ContinuesPastAFailureAndTheNextRequestHeals(t *testing.T) {
	t.Parallel()

	opened := gate.ScaffoldState{
		Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldOpened, Attempt: 1,
		BaseSHA: "tip", PRNumber: 9, PRURL: "https://gh/pull/9",
	}
	gh := &gatetest.GitHub{NoDocs: true, NextCheckRunID: 13, Before: failUpdateOnce(11)}
	store := newScaffoldStore(t, opened, 11, 12)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err == nil {
		t.Fatal("HandleScaffold() = nil, want the failed link update")
	}
	if runs := gh.CheckRuns(); len(runs) != 1 || runs[0].ID != 12 || len(runs[0].Updates) != 1 {
		t.Fatalf("check runs = %+v, want check run 12 linked despite 11 failing", runs)
	}

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	for _, id := range []int64{11, 12, 13} {
		if cr := checkRunByID(gh, id); !strings.Contains(cr.Latest().Summary, "https://gh/pull/9") {
			t.Errorf("check run %d = %+v, want the scaffold PR linked", id, cr)
		}
	}
}

func TestRequestScaffold_SavesTheStateBeforeLinking(t *testing.T) {
	t.Parallel()

	opened := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldOpened, Attempt: 1, BaseSHA: "tip", PRNumber: 9, PRURL: "https://gh/pull/9"}
	gh := &gatetest.GitHub{NoDocs: true, NextCheckRunID: 13, Before: failUpdateOnce(11)}
	store := newScaffoldStore(t, opened, 11)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err == nil {
		t.Fatal("HandlePullRequest() = nil, want the failed link update")
	}

	if saved := loadPR(t, store, 7); saved.CheckRunID != 13 {
		t.Errorf("stored state = %+v, want the check run id 13 saved despite the failing waiter", saved)
	}
}

func TestHandlePullRequest_ClosedScaffoldPRIsNotReplaced(t *testing.T) {
	t.Parallel()

	opened := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldOpened, Attempt: 1, BaseSHA: "tip", PRNumber: 9, PRURL: "https://gh/pull/9"}
	gh := &gatetest.GitHub{NoDocs: true, NextCheckRunID: 13}
	queue := &fakeScaffoldQueue{}
	store := newScaffoldStore(t, opened)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, queue)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	cr := theCheckRun(t, gh)
	if !strings.Contains(cr.Created.Summary, "https://gh/pull/9") {
		t.Errorf("created check run = %+v, want one linking the original pull request", cr.Created)
	}
	if cr.ID != 13 || len(cr.Updates) != 1 || !strings.Contains(cr.Latest().Summary, "https://gh/pull/9") {
		t.Errorf("check run = %+v, want check run 13 updated to link the original pull request", cr)
	}
	if len(queue.attempts) != 0 || len(gh.PullRequests()) != 0 || len(gh.Branches) != 0 || len(gh.Committed()) != 0 {
		t.Errorf("enqueued %v, pull requests %v, branches %v, commits %v, want none: one scaffold PR per repo, ever", queue.attempts, gh.PullRequests(), gh.Branches, gh.Committed())
	}
}

func TestHandleScaffold_ResetsAForeignBranch(t *testing.T) {
	t.Parallel()

	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files}
	gh := &gatetest.GitHub{NoDocs: true, Branches: map[string]gate.Commit{"pollux-agent/docs-scaffold": {SHA: "stale"}}}
	store := newScaffoldStore(t, written)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if diff := cmp.Diff([]string{"pollux-agent/docs-scaffold@tip"}, gh.Resets()); diff != "" {
		t.Errorf("resets (-want +got):\n%s", diff)
	}
	commits := gh.Committed()
	if len(commits) != 1 {
		t.Fatalf("commits = %+v, want one", commits)
	}
	if diff := cmp.Diff(scaffoldFiles(), commits[0].Files); diff != "" {
		t.Errorf("committed files (-want +got):\n%s", diff)
	}
	if state := loadScaffold(t, store); len(gh.PullRequests()) != 1 || state.Phase != gate.ScaffoldOpened {
		t.Errorf("pull requests = %v, state = %+v, want one pull request and an Opened scaffold", gh.PullRequests(), state)
	}
}

func TestHandleScaffold_ForeignBranchWithAPullRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		existing  gate.ScaffoldPR
		wantErr   bool
		wantPhase gate.ScaffoldPhase
	}{
		{name: "a bot's pull request is adopted", existing: gate.ScaffoldPR{Number: 7, URL: "https://gh/pull/7", ByBot: true}, wantPhase: gate.ScaffoldOpened},
		{name: "an open human pull request fails the attempt", existing: gate.ScaffoldPR{Number: 8, URL: "https://gh/pull/8", Open: true}, wantErr: true, wantPhase: gate.ScaffoldWritten},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
			written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files}
			gh := &gatetest.GitHub{NoDocs: true, Branches: map[string]gate.Commit{"pollux-agent/docs-scaffold": {SHA: "stale"}}, ExistingPR: &tc.existing}
			store := newScaffoldStore(t, written)
			svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

			err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"})

			if (err != nil) != tc.wantErr {
				t.Fatalf("HandleScaffold() = %v, want error %v", err, tc.wantErr)
			}
			if len(gh.Resets()) != 0 || len(gh.Committed()) != 0 || len(gh.PullRequests()) != 0 {
				t.Errorf("resets = %v, commits = %v, pull requests = %v, want none", gh.Resets(), gh.Committed(), gh.PullRequests())
			}
			state := loadScaffold(t, store)
			if state.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", state.Phase, tc.wantPhase)
			}
			if !tc.wantErr && (state.PRNumber != tc.existing.Number || state.PRURL != tc.existing.URL) {
				t.Errorf("state = %+v, want the adopted pull request %+v", state, tc.existing)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "not opened by pollux") {
				t.Errorf("error = %v, want it to say the pull request was not opened by pollux", err)
			}
		})
	}
}

func TestHandleScaffold_AdoptsAClosedBotPullRequestWithoutABranch(t *testing.T) {
	t.Parallel()

	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files}
	closed := gate.ScaffoldPR{Number: 7, URL: "https://gh/pull/7", ByBot: true}
	gh := &gatetest.GitHub{NoDocs: true, ExistingPR: &closed}
	store := newScaffoldStore(t, written)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if len(gh.Branches) != 0 || len(gh.Resets()) != 0 || len(gh.Committed()) != 0 || len(gh.PullRequests()) != 0 {
		t.Errorf("branches = %v, resets = %v, commits = %v, pull requests = %v, want none", gh.Branches, gh.Resets(), gh.Committed(), gh.PullRequests())
	}
	if state := loadScaffold(t, store); state.Phase != gate.ScaffoldOpened || state.PRNumber != 7 || state.PRURL != closed.URL {
		t.Errorf("state = %+v, want Opened with the adopted pull request %+v", state, closed)
	}
}

func TestHandleScaffold_ClosedHumanPullRequestDoesNotBlock(t *testing.T) {
	t.Parallel()

	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files}
	gh := &gatetest.GitHub{NoDocs: true, ExistingPR: &gate.ScaffoldPR{Number: 8, URL: "https://gh/pull/8"}}
	store := newScaffoldStore(t, written)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if state := loadScaffold(t, store); len(gh.PullRequests()) != 1 || len(gh.Committed()) != 1 || state.Phase != gate.ScaffoldOpened || state.PRNumber != 9 {
		t.Errorf("pull requests = %v, commits = %v, state = %+v, want a new pull request 9 and an Opened scaffold", gh.PullRequests(), gh.Committed(), state)
	}
}

func TestHandleScaffold_KeepsItsOwnCommitOnRetry(t *testing.T) {
	t.Parallel()

	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files, CommitSHA: "mine"}
	gh := &gatetest.GitHub{NoDocs: true, Branches: map[string]gate.Commit{"pollux-agent/docs-scaffold": {SHA: "mine"}}}
	store := newScaffoldStore(t, written)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if len(gh.Resets()) != 0 || len(gh.Committed()) != 0 {
		t.Errorf("resets = %v, commits = %v, want neither", gh.Resets(), gh.Committed())
	}
	if state := loadScaffold(t, store); len(gh.PullRequests()) != 1 || state.Phase != gate.ScaffoldOpened {
		t.Errorf("pull requests = %v, state = %+v, want one pull request and an Opened scaffold", gh.PullRequests(), state)
	}
}

func TestHandleScaffold_DocsPresentKeepsItsStateWhenAWaiterFails(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Before: failUpdateOnce(11)}
	store := newScaffoldStore(t, gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1}, 11, 12)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err == nil {
		t.Fatal("HandleScaffold() = nil, want the failed waiter update")
	}

	if state := loadScaffold(t, store); state.Phase != gate.ScaffoldIdle || state.Attempt != 2 || state.Failures != 0 {
		t.Errorf("state = %+v, want Idle at attempt 2 and no failed attempt on top", state)
	}
	if diff := cmp.Diff([]int64{11}, unlinkedWaiters(t, store)); diff != "" {
		t.Errorf("unlinked check runs (-want +got):\n%s\nwant only check run 12 done and 11 left for the retry", diff)
	}
}

func TestScaffoldGivenUp(t *testing.T) {
	t.Parallel()

	gaveUp := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldGaveUp, Attempt: 3, Failures: 3, BaseSHA: "tip"}
	gh := &gatetest.GitHub{NoDocs: true, NextCheckRunID: 13}
	queue := &fakeScaffoldQueue{}
	store := newScaffoldStore(t, gaveUp, 11)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, queue)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if diff := cmp.Diff(gaveUp, loadScaffold(t, store)); len(queue.attempts) != 0 || diff != "" {
		t.Errorf("enqueued %v, state (-want +got):\n%s\nwant nothing after giving up", queue.attempts, diff)
	}
	created := checkRunByID(gh, 13).Created
	if !strings.Contains(created.Summary, "after 3 attempts") || !strings.Contains(created.Summary, "docs/guides/setup.md") {
		t.Errorf("created check run = %+v, want it saying the scaffold could not be written after 3 attempts and linking the setup guide", created)
	}
	if n := gh.CallCount("CreateCheckRun"); n != 1 {
		t.Errorf("created check runs = %d, want 1", n)
	}
	if slices.Contains(unlinkedWaiters(t, store), 11) || updateCount(gh) != 2 {
		t.Errorf("unlinked = %v, updates = %d, want the waiting check runs concluded", unlinkedWaiters(t, store), updateCount(gh))
	}
}

func TestScaffoldFailureTellsTheWaiters(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		attempt     int
		wantSummary string
	}{
		{name: "the first failure says the next event retries", attempt: 0, wantSummary: "tries again"},
		{name: "the last failure says to add docs by hand", attempt: 2, wantSummary: "after 3 attempts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := awaitingScaffold(deadline)
			state.Attempt, state.Failures = tc.attempt, tc.attempt
			gh := &gatetest.GitHub{Before: failUpdateOnce(11)}
			store := newScaffoldStore(t, state, 11, 12)
			svc := newService(gh, store, gate.Runners{}, nil)

			err := svc.HandleScaffoldDeadline(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}, "n", deadline.Add(time.Minute))

			if err == nil {
				t.Fatal("HandleScaffoldDeadline() = nil, want the failed waiter update")
			}
			if runs := gh.CheckRuns(); len(runs) != 1 || runs[0].ID != 12 || len(runs[0].Updates) != 1 ||
				!strings.Contains(runs[0].Latest().Summary, tc.wantSummary) || runs[0].Latest().Title != "No docs/ folder" {
				t.Errorf("check runs = %+v, want check run 12 titled \"No docs/ folder\" mentioning %q", runs, tc.wantSummary)
			}
			if diff := cmp.Diff([]int64{11, 12}, unlinkedWaiters(t, store)); diff != "" {
				t.Errorf("unlinked check runs (-want +got):\n%s\nwant no waiter marked done by a failure", diff)
			}
			if got := loadScaffold(t, store).Attempt; got != tc.attempt+1 {
				t.Errorf("attempt = %d, want %d", got, tc.attempt+1)
			}
		})
	}
}

func TestHandleScaffoldRun_WaiterFailureDoesNotMaskTheRunFailure(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	gh := &gatetest.GitHub{Before: failUpdateOnce(11)}
	store := newScaffoldStore(t, awaitingScaffold(deadline), 11)
	svc := newService(gh, store, gate.Runners{Actions: &scaffoldRunner{fakeRunner: &fakeRunner{}}}, nil)

	err := svc.HandleScaffoldRun(t.Context(), gate.RunCompleted{Owner: "acme", Repo: "widgets", RunID: 5, Conclusion: "failure"})

	if err == nil || !strings.Contains(err.Error(), `concluded "failure"`) || !strings.Contains(err.Error(), "github unavailable") {
		t.Errorf("HandleScaffoldRun() = %v, want the run failure joined with the waiter update failure", err)
	}
}

func TestHandleScaffoldRun_WaiterFailureEnqueuesAJobToHealThem(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		noDocs      bool
		wantAttempt int
	}{
		{name: "docs present at the new attempt", noDocs: false, wantAttempt: 2},
		{name: "an opened pull request at the same attempt", noDocs: true, wantAttempt: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &gatetest.GitHub{NoDocs: tc.noDocs, Before: failUpdateOnce(11)}
			runner := &scaffoldRunner{fakeRunner: &fakeRunner{}, files: review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}}
			queue := &fakeScaffoldQueue{}
			store := newScaffoldStore(t, awaitingScaffold(deadline), 11)
			svc := newService(gh, store, gate.Runners{Actions: runner}, queue)

			err := svc.HandleScaffoldRun(t.Context(), gate.RunCompleted{Owner: "acme", Repo: "widgets", RunID: 5, Conclusion: "success"})

			if err == nil || !strings.Contains(err.Error(), "github unavailable") {
				t.Fatalf("HandleScaffoldRun() = %v, want the waiter update failure", err)
			}
			if diff := cmp.Diff([]int{tc.wantAttempt}, queue.attempts); diff != "" {
				t.Errorf("enqueued attempts (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleScaffold_FailedSaveOfOpenedIsNotAFailedAttempt(t *testing.T) {
	t.Parallel()

	written := gate.ScaffoldState{
		Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip",
		Files: &review.Scaffold{Index: "i", Architecture: "a", Setup: "s"},
	}
	store := &hookStore{Store: newScaffoldStore(t, written, 11), saveScaffoldErr: func(s gate.ScaffoldState) error {
		if s.Phase == gate.ScaffoldOpened {
			return errors.New("disk full")
		}
		return nil
	}}
	gh := &gatetest.GitHub{NoDocs: true}
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{}}, nil)

	err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"})

	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("HandleScaffold() = %v, want the failed save", err)
	}
	if state := loadScaffold(t, store); state.Failures != 0 || state.Attempt != 1 {
		t.Errorf("state = %+v, want no failure counted once the pull request exists", state)
	}
	if runs := gh.CheckRuns(); len(runs) != 0 {
		t.Errorf("check runs = %+v, want none telling the waiters the attempt failed", runs)
	}
}

func TestOnScaffoldFailed_LeavesOpenedUnchanged(t *testing.T) {
	t.Parallel()

	opened := gate.ScaffoldState{Phase: gate.ScaffoldOpened, Attempt: 1, Failures: 2, PRNumber: 9, PRURL: "https://gh/pull/9"}

	if diff := cmp.Diff(opened, gate.OnScaffoldFailed(opened)); diff != "" {
		t.Errorf("OnScaffoldFailed(opened) (-want +got):\n%s", diff)
	}
}

func TestHandleScaffold_NoRunnerIsNotAFailedAttempt(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{NoDocs: true}
	store := newScaffoldStore(t, gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1}, 11)
	svc := newService(gh, store, gate.Runners{}, nil)

	for i := 1; i <= 3; i++ {
		if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
			t.Fatalf("HandleScaffold() #%d = %v, want nil", i, err)
		}
		if state := loadScaffold(t, store); state.Phase != gate.ScaffoldIdle || state.Failures != 0 || state.Attempt != 1+i {
			t.Fatalf("state after #%d = %+v, want Idle, no failures, attempt %d", i, state, 1+i)
		}
	}

	cr := checkRunByID(gh, 11)
	if len(cr.Updates) != 3 || !strings.Contains(cr.Latest().Summary, "docs/guides/setup.md") {
		t.Errorf("check run 11 = %+v, want the no-runner text each time", cr)
	}
	if diff := cmp.Diff([]int64{11}, unlinkedWaiters(t, store)); diff != "" {
		t.Errorf("unlinked check runs (-want +got):\n%s\nwant the waiter left unlinked", diff)
	}
}
