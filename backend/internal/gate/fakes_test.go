package gate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

type fakeRunner struct {
	calls      []review.Request
	started    review.Started
	err        error
	collected  []review.Completion
	result     review.Result
	collectErr error
	failFirst  int // Collect returns collectErr only for the first failFirst calls; 0 means always
	onStart    func()
}

func (f *fakeRunner) Collect(_ context.Context, c review.Completion) (review.Result, error) {
	f.collected = append(f.collected, c)
	if f.failFirst > 0 && len(f.collected) > f.failFirst {
		return f.result, nil
	}
	return f.result, f.collectErr
}

func (f *fakeRunner) Start(_ context.Context, req review.Request) (review.Started, error) {
	f.calls = append(f.calls, req)
	if f.onStart != nil {
		f.onStart()
	}
	return f.started, f.err
}

func (f *fakeRunner) StartScaffold(context.Context, review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	return nil, errors.New("fakeRunner does not scaffold")
}

func (f *fakeRunner) CollectScaffold(context.Context, review.Completion) (review.Scaffold, error) {
	return review.Scaffold{}, errors.New("fakeRunner does not scaffold")
}

// hookStore is a gate.Store that can fail saves and loads on demand and sees
// the context of each SavePR; everything else is the real store beneath it.
type hookStore struct {
	gate.Store
	loadErr         error
	saveErr         error
	saveScaffoldErr func(gate.ScaffoldState) error
	saveCtxErrs     []error
}

func (s *hookStore) LoadPR(ctx context.Context, owner, repo string, number int) (gate.PRState, error) {
	if s.loadErr != nil {
		return gate.PRState{}, s.loadErr
	}
	return s.Store.LoadPR(ctx, owner, repo, number) //nolint:wrapcheck // a pass-through to the real store
}

func (s *hookStore) SavePR(ctx context.Context, state gate.PRState) error {
	s.saveCtxErrs = append(s.saveCtxErrs, ctx.Err())
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Store.SavePR(ctx, state) //nolint:wrapcheck // a pass-through to the real store
}

func (s *hookStore) SaveScaffold(ctx context.Context, state gate.ScaffoldState) error {
	if s.saveScaffoldErr != nil {
		if err := s.saveScaffoldErr(state); err != nil {
			return err
		}
	}
	return s.Store.SaveScaffold(ctx, state) //nolint:wrapcheck // a pass-through to the real store
}

// newStore is a real store holding each of states.
func newStore(t *testing.T, states ...gate.PRState) *sqlite.Store {
	t.Helper()

	store := sqlitetest.Open(t)
	for _, state := range states {
		if err := store.SavePR(t.Context(), state); err != nil {
			t.Fatalf("SavePR(%+v) = %v, want nil", state, err)
		}
	}
	return store
}

// newScaffoldStore is a real store holding the scaffold state of acme/widgets
// and the check runs waiting for it.
func newScaffoldStore(t *testing.T, state gate.ScaffoldState, waiting ...int64) *sqlite.Store {
	t.Helper()

	store := newStore(t)
	for _, id := range waiting {
		if _, err := store.RequestScaffold(t.Context(), state.InstallationID, "acme", "widgets", gate.ScaffoldWaiter{CheckRunID: id}); err != nil {
			t.Fatalf("RequestScaffold(%d) = %v, want nil", id, err)
		}
	}
	if err := store.SaveScaffold(t.Context(), state); err != nil {
		t.Fatalf("SaveScaffold(%+v) = %v, want nil", state, err)
	}
	return store
}

// loadPR is the state of pull request number of acme/widgets as stored now.
func loadPR(t *testing.T, store gate.Store, number int) gate.PRState {
	t.Helper()

	state, err := store.LoadPR(t.Context(), "acme", "widgets", number)
	if err != nil {
		t.Fatalf("LoadPR(%d) = %v, want nil", number, err)
	}
	return state
}

// loadScaffold is the scaffold state of acme/widgets as stored now.
func loadScaffold(t *testing.T, store gate.Store) gate.ScaffoldState {
	t.Helper()

	state, err := store.LoadScaffold(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("LoadScaffold() = %v, want nil", err)
	}
	return state
}

// unlinkedWaiters are the check run IDs still waiting for the scaffold of acme/widgets.
func unlinkedWaiters(t *testing.T, store gate.Store) []int64 {
	t.Helper()

	waiters, err := store.UnlinkedScaffoldWaiters(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("UnlinkedScaffoldWaiters() = %v, want nil", err)
	}
	var ids []int64
	for _, w := range waiters {
		ids = append(ids, w.CheckRunID)
	}
	return ids
}

// theCheckRun is the only check run the fake has seen.
func theCheckRun(t *testing.T, gh *gatetest.GitHub) gatetest.CheckRun {
	t.Helper()

	runs := gh.CheckRuns()
	if len(runs) != 1 {
		t.Fatalf("check runs = %+v, want exactly one", runs)
	}
	return runs[0]
}

// summaryBody is the body of the pull request's only issue comment, the summary.
func summaryBody(t *testing.T, gh *gatetest.GitHub) string {
	t.Helper()

	var bodies []string
	for _, c := range gh.Comments() {
		if c.Kind == gate.CommentKindIssue {
			bodies = append(bodies, c.Body)
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("issue comments = %q, want exactly one", bodies)
	}
	return bodies[0]
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

// newService is gate.NewService over the fake; a nil queue records nothing.
func newService(gh *gatetest.GitHub, store gate.Store, runners gate.Runners, queue gate.ScaffoldQueue) *gate.Service {
	if queue == nil {
		queue = &fakeScaffoldQueue{}
	}
	return gate.NewService(gh, store, runners, queue)
}

// commentCalls counts the comment writes the fake received, failed ones included.
type commentCalls struct{ createReview, createIssue, editReview, editIssue int }

func callsOf(gh *gatetest.GitHub) commentCalls {
	return commentCalls{
		createReview: gh.CallCount("CreateReviewComment"),
		createIssue:  gh.CallCount("CreateIssueComment"),
		editReview:   gh.CallCount("EditReviewComment"),
		editIssue:    gh.CallCount("EditIssueComment"),
	}
}

// writeOrder is the comment writes the fake received, in order, by method name.
func writeOrder(gh *gatetest.GitHub) []string {
	var order []string
	for _, c := range gh.Calls() {
		switch c.Method {
		case "CreateReviewComment", "CreateIssueComment", "EditReviewComment", "EditIssueComment":
			order = append(order, c.Method)
		}
	}
	return order
}

// failUpdateOnce is a gatetest Before hook that fails the first UpdateCheckRun of check run id.
func failUpdateOnce(id int64) func(context.Context, gatetest.Call) error {
	failed := false
	return func(_ context.Context, c gatetest.Call) error {
		if c.Method == "UpdateCheckRun" && c.ID == id && !failed {
			failed = true
			return errors.New("github unavailable")
		}
		return nil
	}
}

// scaffoldFiles are the files of the scaffold the tests write, as the commit carries them.
func scaffoldFiles() []gate.FileChange {
	return []gate.FileChange{{Path: "docs/README.md", Content: "i"}, {Path: "docs/architecture.md", Content: "a"}, {Path: "docs/guides/setup.md", Content: "s"}}
}

// updateCount is how many check run updates the fake has received in all.
func updateCount(gh *gatetest.GitHub) int {
	n := 0
	for _, cr := range gh.CheckRuns() {
		n += len(cr.Updates)
	}
	return n
}

// mustCheckRun is the fake's check run id, failing the test when it has not seen one.
func mustCheckRun(t *testing.T, gh *gatetest.GitHub, id int64) gatetest.CheckRun {
	t.Helper()

	cr, ok := gh.CheckRun(id)
	if !ok {
		t.Fatalf("check run %d not seen; check runs = %+v", id, gh.CheckRuns())
	}
	return cr
}

// theCheckRunID is the ID of the only check run the fake has seen.
func theCheckRunID(t *testing.T, gh *gatetest.GitHub) int64 {
	t.Helper()

	return theCheckRun(t, gh).ID
}
