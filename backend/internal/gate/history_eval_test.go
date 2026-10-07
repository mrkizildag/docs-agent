package gate_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// savedAnalyses is the rows the store holds: one per nonce, last save wins, as sqlite upserts.
func savedAnalyses(store *fakeStore) []gate.Analysis {
	var out []gate.Analysis
	idx := map[string]int{}
	for _, h := range store.histories {
		if h.Analysis == nil {
			continue
		}
		if i, ok := idx[h.Analysis.Nonce]; ok {
			out[i] = *h.Analysis
			continue
		}
		idx[h.Analysis.Nonce] = len(out)
		out = append(out, *h.Analysis)
	}
	return out
}

func TestHistoryActionsCompletionRecordedOnceWithUsage(t *testing.T) {
	t.Parallel()

	cost := 0.42
	usage := &review.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 4, CostUSD: &cost, CostBasis: "list"}
	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := awaitingState()
	state.Run.StartedAt, state.Run.Runner = started, gate.RunnerKindActions
	runner := &fakeRunner{result: review.Result{Model: "claude-x", Verdict: review.NoImpact{Reason: "fine"}, Usage: usage}}
	store := &fakeStore{stored: state, live: true}
	svc := gate.NewService(&fakeGitHub{}, nil, store, gate.Runners{Actions: runner}, nil, nil)

	for range 2 { // a redelivered workflow_run event
		if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
			t.Fatalf("HandleRunCompleted() = %v", err)
		}
	}

	got := savedAnalyses(store)
	want := []gate.Analysis{{
		Nonce: "n1", HeadSHA: "abc123", Runner: gate.RunnerKindActions, Model: "claude-x", Verdict: gate.VerdictNoImpact,
		Reason: "fine", StartedAt: started, RunID: 99, Usage: usage,
	}}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(gate.Analysis{}, "FinishedAt")); diff != "" {
		t.Errorf("analyses (-want +got):\n%s", diff)
	}
	if len(got) == 1 && got[0].FinishedAt.IsZero() {
		t.Error("FinishedAt is zero")
	}
}

func TestHistoryFailedRunRecordsCause(t *testing.T) {
	t.Parallel()

	state := awaitingState()
	state.Run.StartedAt, state.Run.Runner = time.Now(), gate.RunnerKindActions
	store := &fakeStore{stored: state, live: true}
	svc := gate.NewService(&fakeGitHub{}, nil, store, gate.Runners{Actions: &fakeRunner{}}, nil, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("cancelled")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v", err)
	}
	got := savedAnalyses(store)
	if len(got) != 1 || got[0].Verdict != gate.VerdictFailed || got[0].Reason == "" || got[0].Usage != nil || got[0].Runner != gate.RunnerKindActions {
		t.Errorf("analyses = %+v, want one failed row with a cause, runner actions, no usage", got)
	}
}

func TestHistoryServerStartErrorRecordedFailed(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{checkRunID: 700}
	runner := &fakeRunner{err: errors.New("clone failed")}
	store := &fakeStore{live: true}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)

	_ = svc.HandlePullRequest(t.Context(), testPR())

	got := savedAnalyses(store)
	if len(got) == 0 {
		t.Fatal("no analysis saved for a run that failed to start")
	}
	last := got[len(got)-1]
	if last.Nonce != "check-700" || last.Verdict != gate.VerdictFailed || last.Runner != gate.RunnerKindServer || last.StartedAt.IsZero() || last.Reason == "" {
		t.Errorf("last analysis = %+v, want check-700 failed by server with start time and cause", last)
	}
}

func TestHistoryDeliberateRerunOfSameHeadWritesNewRow(t *testing.T) {
	t.Parallel()

	ref := gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}
	stored := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "new222", SummaryCommentID: 3}
	gh := &fakeGitHub{pullRequest: gate.PullRequest{BaseSHA: "tip1", HeadSHA: "new222", Open: true}, mergeBase: "base1", checkRunID: 801}
	runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
	store := &fakeStore{stored: stored, live: true}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)

	for _, id := range []int64{801, 802} {
		gh.checkRunID = id
		if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 42, PRRef: ref}); err != nil {
			t.Fatalf("HandleRerun() = %v", err)
		}
	}

	nonces := map[string]gate.AnalysisVerdict{}
	for _, a := range savedAnalyses(store) {
		nonces[a.Nonce] = a.Verdict
	}
	want := map[string]gate.AnalysisVerdict{"check-801": gate.VerdictNoImpact, "check-802": gate.VerdictNoImpact}
	if diff := cmp.Diff(want, nonces); diff != "" {
		t.Errorf("analysis rows by nonce (-want +got):\n%s", diff)
	}
}
