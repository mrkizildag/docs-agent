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

			gh := &fakeGitHub{noDocs: true, checkRunID: 7}
			store := &fakeStore{}
			queue := &fakeScaffoldQueue{}
			svc := newService(gh, nil, store, tc.runners, nil, queue)

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
	resets   []string // "branch@sha" of every ResetBranch
	prs      []gate.NewPullRequest
	existing *gate.ScaffoldPR // the pull request FindPullRequest reports, if any
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

func (g *scaffoldGitHub) ResetBranch(_ context.Context, _ int64, _, _, branch, sha string) error {
	g.resets = append(g.resets, branch+"@"+sha)
	g.branches[branch] = sha
	return nil
}

func (g *scaffoldGitHub) BranchCommit(_ context.Context, _ int64, _, _, branch string) (gate.Commit, error) {
	return gate.Commit{SHA: g.branches[branch]}, nil
}

func (g *scaffoldGitHub) CreatePullRequest(_ context.Context, _ int64, _, _ string, pr gate.NewPullRequest) (gate.ScaffoldPR, error) {
	g.prs = append(g.prs, pr)
	return gate.ScaffoldPR{Number: 9, URL: "https://gh/pull/9"}, nil
}

func (g *scaffoldGitHub) FindPullRequest(context.Context, int64, string, string, string) (gate.ScaffoldPR, bool, error) {
	if g.existing == nil {
		return gate.ScaffoldPR{}, false, nil
	}
	return *g.existing, true, nil
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
	gh := &fakeGitHub{noDocs: true, workflowExists: true}
	sgh := &scaffoldGitHub{branches: map[string]string{}}
	comments := &fakeCommentGitHub{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1}}
	svc := newService(gh, comments, store, gate.Runners{Actions: runner}, sgh, &fakeScaffoldQueue{})

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
	idleAfterFailure := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 2, Failures: 1, BaseSHA: "tip"}

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
			store := &scaffoldStore{fakeStore: &fakeStore{}, state: tc.state, waiters: []gate.ScaffoldWaiter{{CheckRunID: 11}}}
			svc := newService(gh, comments, store, gate.Runners{Actions: runner}, sgh, &fakeScaffoldQueue{}).WithRetryBackoff(0)
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

			store := &scaffoldStore{fakeStore: &fakeStore{}, state: tc.state}
			svc := newService(&fakeGitHub{}, nil, store, gate.Runners{}, nil, &fakeScaffoldQueue{})

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
		waiters:   []gate.ScaffoldWaiter{{CheckRunID: 11}, {CheckRunID: 12}},
	}
	svc := newService(gh, nil, store, gate.Runners{Server: &fakeRunner{}}, &scaffoldGitHub{branches: map[string]string{}}, &fakeScaffoldQueue{})

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
		BaseSHA: "tip", PRNumber: 9, PRURL: "https://gh/pull/9",
	}
	gh := &failingUpdatesGitHub{fakeGitHub: &fakeGitHub{noDocs: true, checkRunID: 13}, failFirst: map[int64]bool{11: true}}
	store := &scaffoldStore{
		fakeStore: &fakeStore{},
		state:     opened,
		waiters:   []gate.ScaffoldWaiter{{CheckRunID: 11}, {CheckRunID: 12}},
	}
	svc := newService(gh, &fakeCommentGitHub{}, store, gate.Runners{Server: &fakeRunner{}}, &scaffoldGitHub{branches: map[string]string{}}, &fakeScaffoldQueue{})

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

func TestRequestScaffold_SavesTheStateBeforeLinking(t *testing.T) {
	t.Parallel()

	opened := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldOpened, Attempt: 1, BaseSHA: "tip", PRNumber: 9, PRURL: "https://gh/pull/9"}
	gh := &failingUpdatesGitHub{fakeGitHub: &fakeGitHub{noDocs: true, checkRunID: 13}, failFirst: map[int64]bool{11: true}}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: opened, waiters: []gate.ScaffoldWaiter{{CheckRunID: 11}}}
	svc := newService(gh, nil, store, gate.Runners{Server: &fakeRunner{}}, nil, &fakeScaffoldQueue{})

	if err := svc.HandlePullRequest(t.Context(), testPR()); err == nil {
		t.Fatal("HandlePullRequest() = nil, want the failed link update")
	}

	if store.saved == nil || store.saved.CheckRunID != 13 {
		t.Errorf("saved state = %+v, want the check run id 13 saved despite the failing waiter", store.saved)
	}
}

func TestHandlePullRequest_ClosedScaffoldPRIsNotReplaced(t *testing.T) {
	t.Parallel()

	opened := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldOpened, Attempt: 1, BaseSHA: "tip", PRNumber: 9, PRURL: "https://gh/pull/9"}
	gh := &fakeGitHub{noDocs: true, checkRunID: 13}
	sgh := &scaffoldGitHub{branches: map[string]string{}}
	comments := &fakeCommentGitHub{}
	queue := &fakeScaffoldQueue{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: opened}
	svc := newService(gh, comments, store, gate.Runners{Server: &fakeRunner{}}, sgh, queue)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}

	if len(gh.calls) != 1 || !strings.Contains(gh.calls[0].run.Summary, "https://gh/pull/9") {
		t.Errorf("created check runs = %+v, want one linking the original pull request", gh.calls)
	}
	if len(gh.updates) != 1 || gh.updates[0].id != 13 || !strings.Contains(gh.updates[0].run.Summary, "https://gh/pull/9") {
		t.Errorf("check run updates = %+v, want check run 13 linking the original pull request", gh.updates)
	}
	if len(queue.attempts) != 0 || len(sgh.prs) != 0 || len(sgh.branches) != 0 || len(comments.commits) != 0 {
		t.Errorf("enqueued %v, pull requests %v, branches %v, commits %v, want none: one scaffold PR per repo, ever", queue.attempts, sgh.prs, sgh.branches, comments.commits)
	}
}

func TestHandleScaffold_ResetsAForeignBranch(t *testing.T) {
	t.Parallel()

	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files}
	gh := &fakeGitHub{noDocs: true}
	sgh := &scaffoldGitHub{branches: map[string]string{"pollux-agent/docs-scaffold": "stale"}}
	comments := &fakeCommentGitHub{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: written}
	svc := newService(gh, comments, store, gate.Runners{Server: &fakeRunner{}}, sgh, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if diff := cmp.Diff([]string{"pollux-agent/docs-scaffold@tip"}, sgh.resets); diff != "" {
		t.Errorf("resets (-want +got):\n%s", diff)
	}
	wantCommit := []gate.FileChange{{Path: "docs/README.md", Content: "i"}, {Path: "docs/architecture.md", Content: "a"}, {Path: "docs/guides/setup.md", Content: "s"}}
	if diff := cmp.Diff([][]gate.FileChange{wantCommit}, comments.commits); diff != "" {
		t.Errorf("commits (-want +got):\n%s", diff)
	}
	if len(sgh.prs) != 1 || store.state.Phase != gate.ScaffoldOpened {
		t.Errorf("pull requests = %v, state = %+v, want one pull request and an Opened scaffold", sgh.prs, store.state)
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
			sgh := &scaffoldGitHub{branches: map[string]string{"pollux-agent/docs-scaffold": "stale"}, existing: &tc.existing}
			comments := &fakeCommentGitHub{}
			store := &scaffoldStore{fakeStore: &fakeStore{}, state: written}
			svc := newService(&fakeGitHub{noDocs: true}, comments, store, gate.Runners{Server: &fakeRunner{}}, sgh, &fakeScaffoldQueue{})

			err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"})

			if (err != nil) != tc.wantErr {
				t.Fatalf("HandleScaffold() = %v, want error %v", err, tc.wantErr)
			}
			if len(sgh.resets) != 0 || len(comments.commits) != 0 || len(sgh.prs) != 0 {
				t.Errorf("resets = %v, commits = %v, pull requests = %v, want none", sgh.resets, comments.commits, sgh.prs)
			}
			if store.state.Phase != tc.wantPhase {
				t.Errorf("phase = %q, want %q", store.state.Phase, tc.wantPhase)
			}
			if !tc.wantErr && (store.state.PRNumber != tc.existing.Number || store.state.PRURL != tc.existing.URL) {
				t.Errorf("state = %+v, want the adopted pull request %+v", store.state, tc.existing)
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
	sgh := &scaffoldGitHub{branches: map[string]string{}, existing: &closed}
	comments := &fakeCommentGitHub{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: written}
	svc := newService(&fakeGitHub{noDocs: true}, comments, store, gate.Runners{Server: &fakeRunner{}}, sgh, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if len(sgh.branches) != 0 || len(sgh.resets) != 0 || len(comments.commits) != 0 || len(sgh.prs) != 0 {
		t.Errorf("branches = %v, resets = %v, commits = %v, pull requests = %v, want none", sgh.branches, sgh.resets, comments.commits, sgh.prs)
	}
	if store.state.Phase != gate.ScaffoldOpened || store.state.PRNumber != 7 || store.state.PRURL != closed.URL {
		t.Errorf("state = %+v, want Opened with the adopted pull request %+v", store.state, closed)
	}
}

func TestHandleScaffold_ClosedHumanPullRequestDoesNotBlock(t *testing.T) {
	t.Parallel()

	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files}
	sgh := &scaffoldGitHub{branches: map[string]string{}, existing: &gate.ScaffoldPR{Number: 8, URL: "https://gh/pull/8"}}
	comments := &fakeCommentGitHub{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: written}
	svc := newService(&fakeGitHub{noDocs: true}, comments, store, gate.Runners{Server: &fakeRunner{}}, sgh, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if len(sgh.prs) != 1 || len(comments.commits) != 1 || store.state.Phase != gate.ScaffoldOpened || store.state.PRNumber != 9 {
		t.Errorf("pull requests = %v, commits = %v, state = %+v, want a new pull request 9 and an Opened scaffold", sgh.prs, comments.commits, store.state)
	}
}

func TestHandleScaffold_KeepsItsOwnCommitOnRetry(t *testing.T) {
	t.Parallel()

	files := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}
	written := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip", Files: &files, CommitSHA: "mine"}
	gh := &fakeGitHub{noDocs: true}
	sgh := &scaffoldGitHub{branches: map[string]string{"pollux-agent/docs-scaffold": "mine"}}
	comments := &fakeCommentGitHub{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: written}
	svc := newService(gh, comments, store, gate.Runners{Server: &fakeRunner{}}, sgh, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if len(sgh.resets) != 0 || len(comments.commits) != 0 {
		t.Errorf("resets = %v, commits = %v, want neither", sgh.resets, comments.commits)
	}
	if len(sgh.prs) != 1 || store.state.Phase != gate.ScaffoldOpened {
		t.Errorf("pull requests = %v, state = %+v, want one pull request and an Opened scaffold", sgh.prs, store.state)
	}
}

func TestHandleScaffold_DocsPresentKeepsItsStateWhenAWaiterFails(t *testing.T) {
	t.Parallel()

	gh := &failingUpdatesGitHub{fakeGitHub: &fakeGitHub{}, failFirst: map[int64]bool{11: true}}
	store := &scaffoldStore{
		fakeStore: &fakeStore{},
		state:     gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1},
		waiters:   []gate.ScaffoldWaiter{{CheckRunID: 11}, {CheckRunID: 12}},
	}
	svc := newService(gh, nil, store, gate.Runners{Server: &fakeRunner{}}, &scaffoldGitHub{branches: map[string]string{}}, &fakeScaffoldQueue{})

	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err == nil {
		t.Fatal("HandleScaffold() = nil, want the failed waiter update")
	}

	if len(store.saves) != 1 || store.state.Phase != gate.ScaffoldIdle || store.state.Attempt != 2 {
		t.Errorf("saves = %+v, want one save of Idle at attempt 2 and no failed attempt on top", store.saves)
	}
	if store.linked[11] || !store.linked[12] {
		t.Errorf("linked = %v, want only check run 12 done and 11 left for the retry", store.linked)
	}
}

func TestScaffoldGivenUp(t *testing.T) {
	t.Parallel()

	gaveUp := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldGaveUp, Attempt: 3, Failures: 3, BaseSHA: "tip"}
	gh := &fakeGitHub{noDocs: true, checkRunID: 13}
	queue := &fakeScaffoldQueue{}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: gaveUp, waiters: []gate.ScaffoldWaiter{{CheckRunID: 11}}}
	svc := newService(gh, &fakeCommentGitHub{}, store, gate.Runners{Server: &fakeRunner{}}, &scaffoldGitHub{branches: map[string]string{}}, queue)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
		t.Fatalf("HandleScaffold() = %v, want nil", err)
	}

	if len(queue.attempts) != 0 || len(store.saves) != 0 {
		t.Errorf("enqueued %v, saved %d states, want none after giving up", queue.attempts, len(store.saves))
	}
	if len(gh.calls) != 1 || !strings.Contains(gh.calls[0].run.Summary, "after 3 attempts") || !strings.Contains(gh.calls[0].run.Summary, "docs/guides/setup.md") {
		t.Errorf("created check runs = %+v, want one saying the scaffold could not be written after 3 attempts and linking the setup guide", gh.calls)
	}
	if len(gh.updates) != 2 || !store.linked[11] {
		t.Errorf("check run updates = %+v, linked = %v, want the waiting check runs concluded", gh.updates, store.linked)
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
			gh := &failingUpdatesGitHub{fakeGitHub: &fakeGitHub{}, failFirst: map[int64]bool{11: true}}
			store := &scaffoldStore{fakeStore: &fakeStore{}, state: state, waiters: []gate.ScaffoldWaiter{{CheckRunID: 11}, {CheckRunID: 12}}}
			svc := newService(gh, nil, store, gate.Runners{}, nil, &fakeScaffoldQueue{})

			err := svc.HandleScaffoldDeadline(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}, "n", deadline.Add(time.Minute))

			if err == nil {
				t.Fatal("HandleScaffoldDeadline() = nil, want the failed waiter update")
			}
			if len(gh.updates) != 1 || gh.updates[0].id != 12 || !strings.Contains(gh.updates[0].run.Summary, tc.wantSummary) || gh.updates[0].run.Title != "No docs/ folder" {
				t.Errorf("check run updates = %+v, want check run 12 titled \"No docs/ folder\" mentioning %q", gh.updates, tc.wantSummary)
			}
			if len(store.linked) != 0 {
				t.Errorf("linked = %v, want no waiter marked done by a failure", store.linked)
			}
			if store.state.Attempt != tc.attempt+1 {
				t.Errorf("attempt = %d, want %d", store.state.Attempt, tc.attempt+1)
			}
		})
	}
}

func TestHandleScaffoldRun_WaiterFailureDoesNotMaskTheRunFailure(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	gh := &failingUpdatesGitHub{fakeGitHub: &fakeGitHub{}, failFirst: map[int64]bool{11: true}}
	store := &scaffoldStore{fakeStore: &fakeStore{}, state: awaitingScaffold(deadline), waiters: []gate.ScaffoldWaiter{{CheckRunID: 11}}}
	svc := newService(gh, nil, store, gate.Runners{Actions: &scaffoldRunner{fakeRunner: &fakeRunner{}}}, nil, &fakeScaffoldQueue{})

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

			gh := &failingUpdatesGitHub{fakeGitHub: &fakeGitHub{noDocs: tc.noDocs}, failFirst: map[int64]bool{11: true}}
			sgh := &scaffoldGitHub{branches: map[string]string{}}
			runner := &scaffoldRunner{fakeRunner: &fakeRunner{}, files: review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}}
			queue := &fakeScaffoldQueue{}
			store := &scaffoldStore{fakeStore: &fakeStore{}, state: awaitingScaffold(deadline), waiters: []gate.ScaffoldWaiter{{CheckRunID: 11}}}
			svc := newService(gh, &fakeCommentGitHub{}, store, gate.Runners{Actions: runner}, sgh, queue)

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

// openedSaveFailsStore fails the save of an Opened state.
type openedSaveFailsStore struct{ *scaffoldStore }

func (s *openedSaveFailsStore) SaveScaffold(ctx context.Context, state gate.ScaffoldState) error {
	if state.Phase == gate.ScaffoldOpened {
		return errors.New("disk full")
	}
	return s.scaffoldStore.SaveScaffold(ctx, state)
}

func TestHandleScaffold_FailedSaveOfOpenedIsNotAFailedAttempt(t *testing.T) {
	t.Parallel()

	written := gate.ScaffoldState{
		Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldWritten, Attempt: 1, BaseSHA: "tip",
		Files: &review.Scaffold{Index: "i", Architecture: "a", Setup: "s"},
	}
	store := &openedSaveFailsStore{&scaffoldStore{fakeStore: &fakeStore{}, state: written, waiters: []gate.ScaffoldWaiter{{CheckRunID: 11}}}}
	gh := &fakeGitHub{noDocs: true}
	sgh := &scaffoldGitHub{branches: map[string]string{}}
	svc := newService(gh, &fakeCommentGitHub{}, store, gate.Runners{Server: &fakeRunner{}}, sgh, &fakeScaffoldQueue{})

	err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"})

	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("HandleScaffold() = %v, want the failed save", err)
	}
	if store.state.Failures != 0 || store.state.Attempt != 1 {
		t.Errorf("state = %+v, want no failure counted once the pull request exists", store.state)
	}
	if len(gh.updates) != 0 {
		t.Errorf("check run updates = %+v, want none telling the waiters the attempt failed", gh.updates)
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

	gh := &fakeGitHub{noDocs: true}
	store := &scaffoldStore{
		fakeStore: &fakeStore{},
		state:     gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 42, Phase: gate.ScaffoldIdle, Attempt: 1},
		waiters:   []gate.ScaffoldWaiter{{CheckRunID: 11}},
	}
	svc := newService(gh, &fakeCommentGitHub{}, store, gate.Runners{}, &scaffoldGitHub{branches: map[string]string{}}, &fakeScaffoldQueue{})

	for i := 1; i <= 3; i++ {
		if err := svc.HandleScaffold(t.Context(), gate.RepoRef{Owner: "acme", Repo: "widgets"}); err != nil {
			t.Fatalf("HandleScaffold() #%d = %v, want nil", i, err)
		}
		if store.state.Phase != gate.ScaffoldIdle || store.state.Failures != 0 || store.state.Attempt != 1+i {
			t.Fatalf("state after #%d = %+v, want Idle, no failures, attempt %d", i, store.state, 1+i)
		}
	}

	if len(gh.updates) != 3 || !strings.Contains(gh.updates[2].run.Summary, "docs/guides/setup.md") {
		t.Errorf("check run updates = %+v, want the no-runner text each time", gh.updates)
	}
	if len(store.linked) != 0 {
		t.Errorf("linked = %v, want the waiter left unlinked", store.linked)
	}
}
