package gate_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func TestOnPush(t *testing.T) {
	t.Parallel()

	want := gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}

	tests := []struct {
		name  string
		state gate.PRState
	}{
		{name: "fresh state", state: gate.PRState{Owner: "acme", Repo: "widgets", Number: 7}},
		{name: "state with older head", state: gate.PRState{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "old111"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if diff := cmp.Diff(want, gate.OnPush(tt.state, testPR())); diff != "" {
				t.Errorf("OnPush() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOnPushDropsAwaitedRun(t *testing.T) {
	t.Parallel()

	got := gate.OnPush(awaitingState(), testPR())
	if got.Run != nil || got.CheckRunID != 0 {
		t.Errorf("OnPush() = %+v, want no awaited run and no check run", got)
	}
}

func TestMatchesRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state gate.PRState
		runID int64
		want  bool
	}{
		{name: "same run", state: awaitingState(), runID: 99, want: true},
		{name: "other run", state: awaitingState(), runID: 100},
		{name: "zero run id", state: awaitingState(), runID: 0},
		{name: "not awaiting", state: gate.PRState{}, runID: 99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := gate.MatchesRun(tc.state, gate.RunCompleted{RunID: tc.runID}); got != tc.want {
				t.Errorf("MatchesRun(%+v, run %d) = %v, want %v", tc.state, tc.runID, got, tc.want)
			}
		})
	}
}

func TestOnPushSkips(t *testing.T) {
	t.Parallel()

	commit := &gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "r", HeadSHA: "old111"}
	pr := &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "r", HeadSHA: "old111"}
	tests := []struct {
		name        string
		prev        gate.PRState
		wantSkip    *gate.Skip
		wantPending *gate.SkipAsk
	}{
		{name: "commit skip cleared", prev: gate.PRState{Skip: commit, PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}}},
		{name: "PR skip kept, its pending ask cancelled by the new head", prev: gate.PRState{HeadSHA: "old111", Skip: pr, PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}}, wantSkip: pr},
		{name: "pending PR ask kept for the same head", prev: gate.PRState{HeadSHA: "abc123", PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}}, wantPending: &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}},
		{name: "pending commit ask kept for the same head", prev: gate.PRState{HeadSHA: "abc123", PendingSkip: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}}, wantPending: &gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}},
		{name: "commit skip kept for the same head", prev: gate.PRState{Skip: &gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "r", HeadSHA: "abc123"}}, wantSkip: &gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "r", HeadSHA: "abc123"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := gate.OnPush(tt.prev, testPR())
			if diff := cmp.Diff(tt.wantSkip, got.Skip); diff != "" {
				t.Errorf("Skip (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantPending, got.PendingSkip); diff != "" {
				t.Errorf("PendingSkip (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOnPushDropsPendingApply(t *testing.T) {
	t.Parallel()

	prev := gate.PRState{HeadSHA: "old111", PendingApply: &gate.PendingApply{IDs: []string{"p1"}, Message: "m", Parent: "old111"}}
	if got := gate.OnPush(prev, testPR()); got.PendingApply != nil {
		t.Errorf("OnPush().PendingApply = %+v, want nil", got.PendingApply)
	}
}

func TestOnPushCopiesFork(t *testing.T) {
	t.Parallel()

	pr := testPR()
	pr.Fork = true
	if got := gate.OnPush(gate.PRState{}, pr); !got.Fork {
		t.Error("OnPush().Fork = false, want true")
	}
}
