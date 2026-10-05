package gate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
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
			got:  gate.OnScaffoldOpened(written, "b", pr),
			want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldOpened, Attempt: 1, BaseSHA: "base", Files: &files, Branch: "b", PRNumber: 4, PRURL: pr.URL},
		},
		{name: "failed while writing", got: gate.OnScaffoldFailed(writing), want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldIdle, Attempt: 2, BaseSHA: "base"}},
		{name: "failed while awaiting", got: gate.OnScaffoldFailed(awaiting), want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldIdle, Attempt: 2, BaseSHA: "base"}},
		{name: "failed after writing keeps files", got: gate.OnScaffoldFailed(written), want: gate.ScaffoldState{Owner: "o", Repo: "r", Phase: gate.ScaffoldWritten, Attempt: 2, BaseSHA: "base", Files: &files}},
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

			gh := &fakeGitHub{noDocs: true, checkRunID: 7}
			store := &fakeStore{}
			queue := &fakeScaffoldQueue{}
			svc := gate.NewService(gh, nil, store, tc.runners, nil, queue)

			if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
				t.Fatalf("HandlePullRequest() = %v, want nil", err)
			}

			if len(gh.calls) != 1 {
				t.Fatalf("created %d check runs, want 1", len(gh.calls))
			}
			got := gh.calls[0].run
			if got.Conclusion != gate.ConclusionNeutral || got.Title != "No docs/ folder" || !strings.Contains(got.Summary, tc.wantSummary) {
				t.Errorf("check run = %+v, want neutral \"No docs/ folder\" mentioning %q", got, tc.wantSummary)
			}
			if gh.changedCalls != 0 {
				t.Errorf("listed changed files %d times, want 0: no review analysis without docs/", gh.changedCalls)
			}
			if len(queue.attempts) != tc.wantEnqueued {
				t.Errorf("enqueued scaffold jobs = %v, want %d", queue.attempts, tc.wantEnqueued)
			}
			if store.saved == nil || store.saved.CheckRunID != 7 {
				t.Errorf("saved state = %+v, want the check run id 7", store.saved)
			}
		})
	}
}

// scaffoldStore is a fakeStore that remembers the scaffold state and its waiters.
type scaffoldStore struct {
	*fakeStore
	state   gate.ScaffoldState
	waiters []gate.ScaffoldWaiter
	linked  map[int64]bool
	saves   []gate.ScaffoldState
}

func (s *scaffoldStore) LoadScaffold(context.Context, string, string) (gate.ScaffoldState, error) {
	return s.state, nil
}

func (s *scaffoldStore) SaveScaffold(_ context.Context, state gate.ScaffoldState) error {
	s.saves = append(s.saves, state)
	s.state = state
	return nil
}

func (s *scaffoldStore) RequestScaffold(_ context.Context, _ int64, _, _ string, waiter gate.ScaffoldWaiter) (gate.ScaffoldState, error) {
	s.waiters = append(s.waiters, waiter)
	return s.state, nil
}

func (s *scaffoldStore) UnlinkedScaffoldWaiters(context.Context, string, string) ([]gate.ScaffoldWaiter, error) {
	var unlinked []gate.ScaffoldWaiter
	for _, w := range s.waiters {
		if !s.linked[w.CheckRunID] {
			unlinked = append(unlinked, w)
		}
	}
	return unlinked, nil
}

func (s *scaffoldStore) MarkScaffoldWaiterLinked(_ context.Context, _, _ string, checkRunID int64) error {
	if s.linked == nil {
		s.linked = map[int64]bool{}
	}
	s.linked[checkRunID] = true
	return nil
}

// failingUpdatesGitHub fails the first update of each check run in failFirst.
type failingUpdatesGitHub struct {
	*fakeGitHub
	failFirst map[int64]bool
}

func (g *failingUpdatesGitHub) UpdateCheckRun(ctx context.Context, inst int64, owner, repo string, id int64, run gate.CheckRun) error {
	if g.failFirst[id] {
		delete(g.failFirst, id)
		return errors.New("github unavailable")
	}
	return g.fakeGitHub.UpdateCheckRun(ctx, inst, owner, repo, id, run)
}

// scaffoldGitHub is a ScaffoldGitHub for a repo whose default branch is at "tip".
type scaffoldGitHub struct {
	branches map[string]string
	prs      []gate.NewPullRequest
}

func (g *scaffoldGitHub) DefaultBranch(context.Context, int64, string, string) (string, string, error) {
	return "main", "tip", nil
}

func (g *scaffoldGitHub) CreateBranch(_ context.Context, _ int64, _, _, branch, sha string) error {
	if _, ok := g.branches[branch]; ok {
		return gate.ErrBranchExists
	}
	g.branches[branch] = sha
	return nil
}

func (g *scaffoldGitHub) BranchSHA(_ context.Context, _ int64, _, _, branch string) (string, error) {
	return g.branches[branch], nil
}

func (g *scaffoldGitHub) CreatePullRequest(_ context.Context, _ int64, _, _ string, pr gate.NewPullRequest) (gate.ScaffoldPR, error) {
	g.prs = append(g.prs, pr)
	return gate.ScaffoldPR{Number: 9, URL: "https://gh/pull/9"}, nil
}

func (g *scaffoldGitHub) FindPullRequest(context.Context, int64, string, string, string) (gate.ScaffoldPR, bool, error) {
	return gate.ScaffoldPR{}, false, nil
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
		Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldAwaiting, Attempt: 3, BaseSHA: "tip",
		Run: &gate.AwaitingRun{RunID: 5, Nonce: "n", Deadline: deadline},
	}
}

func TestHandleScaffold_StartsTheActionsRun(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	runner := &scaffoldRunner{fakeRunner: &fakeRunner{}, started: review.Pending{RunID: 5, Nonce: "n", Deadline: deadline}}
	gh := &fakeGitHub{noDocs: true, workflowExists: true}
	sgh := &scaffoldGitHub{branches: map[string]string{}}
	comments := &fakeCommentGitHub{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 3}}
	svc := gate.NewService(gh, comments, store, gate.Runners{Actions: runner}, sgh, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if diff := cmp.Diff(awaitingScaffold(deadline), store.state); diff != "" {
		t.Errorf("state (-want +got):\n%s", diff)
	}
	if len(sgh.branches) != 0 || len(sgh.prs) != 0 || len(comments.commits) != 0 {
		t.Errorf("branches %v, pull requests %v, commits %v, want none until the run completes", sgh.branches, sgh.prs, comments.commits)
	}

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() while awaiting = %v, want nil", err)
	}
	if len(store.saves) != 2 {
		t.Errorf("saves = %d, want 2 (writing, awaiting) and none for a job that finds the run awaited", len(store.saves))
	}
}

func TestHandleScaffoldRun(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	invalid := &review.InvalidResultError{Cause: errors.New("bad docs")}
	idleAfterFailure := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 4, BaseSHA: "tip"}

	tests := []struct {
		name        string
		state       gate.ScaffoldState
		rc          gate.RunCompleted
		collectErr  error
		wantErr     bool
		wantState   *gate.ScaffoldState // nil: the state is not saved
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
			gh := &fakeGitHub{noDocs: true}
			sgh := &scaffoldGitHub{branches: map[string]string{"pollux-agent/docs-scaffold": "tip"}}
			comments := &fakeCommentGitHub{}
			store := &scaffoldStore{fakeStore: &fakeStore{}, state: tc.state, waiters: []gate.ScaffoldWaiter{{CheckRunID: 11, PRNumber: 1}}}
			svc := gate.NewService(gh, comments, store, gate.Runners{Actions: runner}, sgh, &fakeScaffoldQueue{}).WithCollectBackoff(0)
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
			switch {
			case tc.wantPR:
				if store.state.Phase != gate.ScaffoldOpened || store.state.PRURL != "https://gh/pull/9" || store.state.Run != nil || *store.state.Files != files {
					t.Errorf("state = %+v, want Opened with the PR, the files and no run", store.state)
				}
				if len(sgh.prs) != 1 {
					t.Errorf("pull requests = %d, want 1", len(sgh.prs))
				}
				wantCommit := []gate.FileChange{{Path: "docs/README.md", Content: "i"}, {Path: "docs/architecture.md", Content: "a"}, {Path: "docs/guides/setup.md", Content: "s"}}
				if diff := cmp.Diff([][]gate.FileChange{wantCommit}, comments.commits); diff != "" {
					t.Errorf("commits (-want +got):\n%s", diff)
				}
				if len(gh.updates) != 1 || gh.updates[0].id != 11 || !strings.Contains(gh.updates[0].run.Summary, "https://gh/pull/9") {
					t.Errorf("check run updates = %+v, want check run 11 linking the pull request", gh.updates)
				}
			case tc.wantState != nil:
				if diff := cmp.Diff(*tc.wantState, store.state); diff != "" {
					t.Errorf("state (-want +got):\n%s", diff)
				}
			default:
				if len(store.saves) != 0 {
					t.Errorf("saved %d states, want none", len(store.saves))
				}
			}
			if !tc.wantPR && (len(sgh.prs) != 0 || len(comments.commits) != 0) {
				t.Errorf("pull requests %v, commits %v, want none", sgh.prs, comments.commits)
			}
		})
	}
}

func TestHandleScaffoldDeadline(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	failed := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 4, BaseSHA: "tip"}

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

			store := &scaffoldStore{fakeStore: &fakeStore{}, state: tc.state}
			svc := gate.NewService(&fakeGitHub{}, nil, store, gate.Runners{}, nil, &fakeScaffoldQueue{})

			if err := svc.HandleScaffoldDeadline(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}, tc.nonce, tc.now); err != nil {
				t.Fatalf("HandleScaffoldDeadline() = %v, want nil", err)
			}

			if tc.wantState == nil {
				if len(store.saves) != 0 {
					t.Errorf("saved %d states, want none", len(store.saves))
				}
				return
			}
			if diff := cmp.Diff(*tc.wantState, store.state); diff != "" {
				t.Errorf("state (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleScaffold_DocsPresentTellsTheWaiters(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &scaffoldStore{
		fakeStore: &fakeStore{},
		state:     gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1},
		waiters:   []gate.ScaffoldWaiter{{CheckRunID: 11, PRNumber: 1}, {CheckRunID: 12, PRNumber: 2}},
	}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: &fakeRunner{}}, &scaffoldGitHub{branches: map[string]string{}}, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if len(gh.updates) != 2 {
		t.Fatalf("check run updates = %d, want 2", len(gh.updates))
	}
	for _, u := range gh.updates {
		run := u.run
		if run.Conclusion != gate.ConclusionNeutral || run.Title != "No docs/ folder" ||
			!strings.Contains(run.Summary, "already has a docs/ folder") || !strings.Contains(run.Summary, "Merge or rebase") || strings.Contains(run.Summary, "writing") {
			t.Errorf("check run %d = %+v, want neutral \"No docs/ folder\" saying the default branch has docs/ and to merge or rebase", u.id, run)
		}
	}
	if store.state.Phase != gate.ScaffoldIdle || store.state.Attempt != 2 {
		t.Errorf("state = %+v, want Idle at attempt 2", store.state)
	}
}

func TestLinkWaiters_ContinuesPastAFailureAndTheNextRequestHeals(t *testing.T) {
	t.Parallel()

	opened := gate.ScaffoldState{
		Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldOpened, Attempt: 1,
		BaseSHA: "tip", Branch: "pollux-agent/docs-scaffold", PRNumber: 9, PRURL: "https://gh/pull/9",
	}
	gh := &failingUpdatesGitHub{fakeGitHub: &fakeGitHub{noDocs: true, checkRunID: 13}, failFirst: map[int64]bool{11: true}}
	store := &scaffoldStore{
		fakeStore: &fakeStore{},
		state:     opened,
		waiters:   []gate.ScaffoldWaiter{{CheckRunID: 11, PRNumber: 1}, {CheckRunID: 12, PRNumber: 2}},
	}
	svc := gate.NewService(gh, &fakeCommentGitHub{}, store, gate.Runners{Server: &fakeRunner{}}, &scaffoldGitHub{branches: map[string]string{}}, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err == nil {
		t.Fatal("HandleScaffold() = nil, want the failed link update")
	}
	if len(gh.updates) != 1 || gh.updates[0].id != 12 {
		t.Fatalf("check run updates = %+v, want check run 12 linked despite 11 failing", gh.updates)
	}

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	var linked []int64
	for _, u := range gh.updates {
		if !strings.Contains(u.run.Summary, "https://gh/pull/9") {
			t.Errorf("check run %d = %+v, want the scaffold PR linked", u.id, u.run)
		}
		linked = append(linked, u.id)
	}
	if diff := cmp.Diff([]int64{12, 11, 13}, linked); diff != "" {
		t.Errorf("linked check runs (-want +got):\n%s", diff)
	}
}
