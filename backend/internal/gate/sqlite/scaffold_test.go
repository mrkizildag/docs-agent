package sqlite_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestLoadScaffold_Unseen(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	got, err := store.LoadScaffold(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("LoadScaffold() = %v, want nil error", err)
	}
	want := gate.ScaffoldState{Owner: "acme", Repo: "widgets", Phase: gate.ScaffoldIdle}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("LoadScaffold() (-want +got):\n%s", diff)
	}
}

func TestSaveScaffold_RoundTrips(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	want := gate.ScaffoldState{
		Owner: "acme", Repo: "widgets", InstallationID: 9, Phase: gate.ScaffoldWritten, Attempt: 3, Failures: 2,
		BaseSHA: "abc", Run: &gate.AwaitingRun{RunID: 5, Nonce: "n", Deadline: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)},
		Files:     &review.Scaffold{Runner: "r", Model: "m", Index: "i", Architecture: "a", Setup: "s"},
		CommitSHA: "c0ffee", PRNumber: 4, PRURL: "https://gh/pull/4",
	}
	if err := store.SaveScaffold(t.Context(), want); err != nil {
		t.Fatalf("SaveScaffold() = %v, want nil error", err)
	}
	got, err := store.LoadScaffold(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("LoadScaffold() = %v, want nil error", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("LoadScaffold() (-want +got):\n%s", diff)
	}
}

func TestRequestScaffold_CreatesOnceAndKeepsExistingState(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	first, err := store.RequestScaffold(t.Context(), 9, "acme", "widgets", gate.ScaffoldWaiter{CheckRunID: 11})
	if err != nil {
		t.Fatalf("first RequestScaffold() = %v, want nil error", err)
	}
	want := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 9, Phase: gate.ScaffoldIdle}
	if diff := cmp.Diff(want, first); diff != "" {
		t.Errorf("first RequestScaffold() (-want +got):\n%s", diff)
	}

	opened := first
	opened.Phase, opened.PRNumber, opened.PRURL = gate.ScaffoldWritten, 4, "u"
	if err := store.SaveScaffold(t.Context(), opened); err != nil {
		t.Fatalf("SaveScaffold() = %v, want nil error", err)
	}

	second, err := store.RequestScaffold(t.Context(), 9, "acme", "widgets", gate.ScaffoldWaiter{CheckRunID: 12})
	if err != nil {
		t.Fatalf("second RequestScaffold() = %v, want nil error", err)
	}
	if diff := cmp.Diff(opened, second); diff != "" {
		t.Errorf("second RequestScaffold() (-want +got):\n%s", diff)
	}
}

func TestRequestScaffold_RefreshesInstallationID(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	opened := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 9, Phase: gate.ScaffoldWritten, PRNumber: 4, PRURL: "u"}
	if err := store.SaveScaffold(t.Context(), opened); err != nil {
		t.Fatalf("SaveScaffold() = %v, want nil error", err)
	}

	got, err := store.RequestScaffold(t.Context(), 77, "acme", "widgets", gate.ScaffoldWaiter{CheckRunID: 12})
	if err != nil {
		t.Fatalf("RequestScaffold() = %v, want nil error", err)
	}
	opened.InstallationID = 77
	if diff := cmp.Diff(opened, got); diff != "" {
		t.Errorf("RequestScaffold() (-want +got):\n%s", diff)
	}
	loaded, err := store.LoadScaffold(t.Context(), "acme", "widgets")
	if err != nil || loaded.InstallationID != 77 {
		t.Errorf("LoadScaffold() = %+v, %v; want installation 77 persisted", loaded, err)
	}
}

func TestUnlinkedScaffoldWaiters_AppendOnlyAndDeduplicated(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	for _, w := range []gate.ScaffoldWaiter{{CheckRunID: 11}, {CheckRunID: 12}, {CheckRunID: 11}} {
		if _, err := store.RequestScaffold(t.Context(), 9, "acme", "widgets", w); err != nil {
			t.Fatalf("RequestScaffold(%+v) = %v, want nil error", w, err)
		}
	}
	if _, err := store.RequestScaffold(t.Context(), 9, "acme", "other", gate.ScaffoldWaiter{CheckRunID: 13}); err != nil {
		t.Fatalf("RequestScaffold(other repo) = %v, want nil error", err)
	}

	got, err := store.UnlinkedScaffoldWaiters(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("UnlinkedScaffoldWaiters() = %v, want nil error", err)
	}
	want := []gate.ScaffoldWaiter{{CheckRunID: 11}, {CheckRunID: 12}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("UnlinkedScaffoldWaiters() (-want +got):\n%s", diff)
	}
}

func TestMarkScaffoldWaiterLinked_HidesOnlyThatWaiter(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	for _, w := range []gate.ScaffoldWaiter{{CheckRunID: 11}, {CheckRunID: 12}} {
		if _, err := store.RequestScaffold(t.Context(), 9, "acme", "widgets", w); err != nil {
			t.Fatalf("RequestScaffold(%+v) = %v, want nil error", w, err)
		}
	}
	if err := store.MarkScaffoldWaiterLinked(t.Context(), "acme", "widgets", 11); err != nil {
		t.Fatalf("MarkScaffoldWaiterLinked() = %v, want nil error", err)
	}
	if _, err := store.RequestScaffold(t.Context(), 9, "acme", "widgets", gate.ScaffoldWaiter{CheckRunID: 11}); err != nil {
		t.Fatalf("repeated RequestScaffold() = %v, want nil error", err)
	}

	got, err := store.UnlinkedScaffoldWaiters(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("UnlinkedScaffoldWaiters() = %v, want nil error", err)
	}
	want := []gate.ScaffoldWaiter{{CheckRunID: 12}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("UnlinkedScaffoldWaiters() (-want +got):\n%s", diff)
	}
}

func TestScaffoldForRun(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()
	deadline := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	awaiting := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 9, Phase: gate.ScaffoldAwaiting, Run: &gate.AwaitingRun{RunID: 5, Nonce: "n", Deadline: deadline}}
	if err := store.SaveScaffold(ctx, awaiting); err != nil {
		t.Fatalf("SaveScaffold() = %v, want nil error", err)
	}

	tests := []struct {
		name  string
		owner string
		repo  string
		runID int64
		want  bool
	}{
		{name: "the awaited run", owner: "acme", repo: "widgets", runID: 5, want: true},
		{name: "another run", owner: "acme", repo: "widgets", runID: 6},
		{name: "another repo", owner: "acme", repo: "other", runID: 5},
		{name: "run zero", owner: "acme", repo: "widgets"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.ScaffoldForRun(ctx, tc.owner, tc.repo, tc.runID)
			if err != nil || got != tc.want {
				t.Errorf("ScaffoldForRun(%s/%s, %d) = %v, %v, want %v", tc.owner, tc.repo, tc.runID, got, err, tc.want)
			}
		})
	}

	if err := store.SaveScaffold(ctx, gate.OnScaffoldFailed(awaiting)); err != nil {
		t.Fatalf("SaveScaffold(failed) = %v, want nil error", err)
	}
	if got, err := store.ScaffoldForRun(ctx, "acme", "widgets", 5); err != nil || got {
		t.Errorf("ScaffoldForRun() after the attempt failed = %v, %v, want false", got, err)
	}
}

func TestOverdueRuns_IncludesAwaitedScaffolds(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	if err := store.SavePR(ctx, gate.PRState{Owner: "acme", Repo: "widgets", Number: 1, HeadSHA: "a", CheckRunID: 1, Run: &gate.AwaitingRun{RunID: 1, Nonce: "n1", Deadline: base}}); err != nil {
		t.Fatalf("SavePR() = %v, want nil error", err)
	}
	for _, st := range []gate.ScaffoldState{
		{Owner: "acme", Repo: "overdue", Phase: gate.ScaffoldAwaiting, Run: &gate.AwaitingRun{RunID: 2, Nonce: "n2", Deadline: base}},
		{Owner: "acme", Repo: "pending", Phase: gate.ScaffoldAwaiting, Run: &gate.AwaitingRun{RunID: 3, Nonce: "n3", Deadline: base.Add(time.Hour)}},
		{Owner: "acme", Repo: "idle", Phase: gate.ScaffoldIdle},
	} {
		if err := store.SaveScaffold(ctx, st); err != nil {
			t.Fatalf("SaveScaffold(%+v) = %v, want nil error", st, err)
		}
	}

	got, err := store.OverdueRuns(ctx, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("OverdueRuns() = %v, want nil error", err)
	}
	want := []gate.OverdueRun{
		{PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 1}, Nonce: "n1", Deadline: base},
		{PRRef: gate.PRRef{Owner: "acme", Repo: "overdue"}, Scaffold: true, Nonce: "n2", Deadline: base},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("OverdueRuns() (-want +got):\n%s", diff)
	}
}

func TestSaveScaffold_KeepsTheInstallationRequestScaffoldRecorded(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	if _, err := store.RequestScaffold(t.Context(), 9, "acme", "widgets", gate.ScaffoldWaiter{CheckRunID: 11}); err != nil {
		t.Fatalf("RequestScaffold() = %v, want nil error", err)
	}
	if _, err := store.RequestScaffold(t.Context(), 10, "acme", "widgets", gate.ScaffoldWaiter{CheckRunID: 12}); err != nil {
		t.Fatalf("second RequestScaffold() = %v, want nil error", err)
	}
	stale := gate.ScaffoldState{Owner: "acme", Repo: "widgets", InstallationID: 9, Phase: gate.ScaffoldWriting, Attempt: 1}
	if err := store.SaveScaffold(t.Context(), stale); err != nil {
		t.Fatalf("SaveScaffold() = %v, want nil error", err)
	}

	got, err := store.LoadScaffold(t.Context(), "acme", "widgets")
	if err != nil {
		t.Fatalf("LoadScaffold() = %v, want nil error", err)
	}
	if got.InstallationID != 10 || got.Phase != gate.ScaffoldWriting {
		t.Errorf("LoadScaffold() = %+v, want installation 10 kept and phase Writing saved", got)
	}
}
