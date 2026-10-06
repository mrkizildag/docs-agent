package gate_test

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestIsRerunTick(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ticked string
		want   bool
	}{
		{"- [x] Re-run analysis", true},
		{"  - [x] Re-run analysis\r", true},
		{"- [x] Apply all", false},
		{"- [ ] Re-run analysis", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := gate.IsRerunTick(tc.ticked); got != tc.want {
			t.Errorf("IsRerunTick(%q) = %v, want %v", tc.ticked, got, tc.want)
		}
	}
}

func failedSummaryState() gate.PRState {
	state := skipBase()
	state.Run = nil
	state.HeadSHA = "old111"
	state.FailureCause = "The analysis timed out."
	return state
}

func rerunEvent() gate.CommentEvent {
	return skipEvent(gate.CommentKindIssue, "- [x] Re-run analysis", "")
}

func newHeadGitHub() *gatetest.GitHub {
	return &gatetest.GitHub{PullRequest: gate.PullRequest{BaseSHA: "base1", HeadSHA: "new222", Open: true}, CanWrite: true}
}

// The write budget of the final reaction starts after the action, so a re-run
// that takes longer than the budget still settles.
func TestHandleCommentRerunSettlesAfterASlowAnalysis(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		runner := &fakeRunner{
			started: review.Result{Verdict: review.NoImpact{Reason: "ok"}},
			onStart: func() { time.Sleep(40 * time.Second) },
		}
		gh := newHeadGitHub()
		svc := newService(gh, newStore(t, failedSummaryState()), gate.Runners{Server: runner}, nil)
		ev := rerunEvent()

		if err := svc.HandleComment(t.Context(), ev); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if diff := cmp.Diff([]gate.Reaction{gate.ReactionDone}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
			t.Errorf("reactions (-want +got):\n%s", diff)
		}
	})
}

func TestHandleCommentRerunWhoseAnalysisFailedIsDone(t *testing.T) {
	t.Parallel()

	failure := &review.FailedError{Cause: review.CauseLimit, Err: errors.New("limit")}
	tests := []struct {
		name         string
		updateErr    error
		wantErr      bool
		wantReaction gate.Reaction
	}{
		{name: "failure reported on the check run", wantReaction: gate.ReactionDone},
		{name: "failure could not be reported", updateErr: errors.New("boom"), wantErr: true, wantReaction: gate.ReactionSeen},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := newHeadGitHub()
			if tc.updateErr != nil {
				gh.Fail = map[string]error{"UpdateCheckRun": tc.updateErr}
			}
			svc := newService(gh, newStore(t, failedSummaryState()), gate.Runners{Server: &fakeRunner{err: failure}}, nil)
			ev := rerunEvent()

			err := svc.HandleComment(t.Context(), ev)

			if (err != nil) != tc.wantErr {
				t.Errorf("HandleComment() = %v, wantErr %v", err, tc.wantErr)
			}
			if diff := cmp.Diff([]gate.Reaction{tc.wantReaction}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleCommentOnAPullRequestNeverAnalyzed(t *testing.T) {
	t.Parallel()

	for _, body := range []string{"/pollux-agent apply", "/pollux-agent skip typo"} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()

			gh := &gatetest.GitHub{CanWrite: true, Files: baseFiles()}
			store := newStore(t)
			svc := newService(gh, store, gate.Runners{}, nil)
			ev := issueComment(body)

			if err := svc.HandleComment(t.Context(), ev); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			for _, c := range gh.Calls() {
				if c.Method == "React" && c.InstallationID != ev.InstallationID {
					t.Errorf("reacted as installation %d, want the event's %d", c.InstallationID, ev.InstallationID)
				}
			}
			if c := gh.Comments(); len(c) != 1 || !strings.Contains(c[0].Body, "hasn't analyzed") {
				t.Errorf("comments = %+v, want one saying the PR was not analyzed yet", c)
			}
			if runs := gh.CheckRuns(); len(gh.Committed()) != 0 || len(runs) != 0 {
				t.Errorf("commits %d, check runs %d, want none", len(gh.Committed()), len(runs))
			}
			if got := loadPR(t, store, ev.Number); got.HeadSHA != "" {
				t.Errorf("stored state = %+v, want none saved", got)
			}
			if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleCommentFromAReaderGetsNoSeenReaction(t *testing.T) {
	t.Parallel()

	gh := apiWithComments()
	gh.CanWrite = false
	svc := newService(gh, newStore(t, threeState()), gate.Runners{}, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, gh.ReactionLog()); diff != "" {
		t.Errorf("reactions added (-want +got):\n%s", diff)
	}
	if replies := gh.Replies(); len(replies) != 1 || !strings.Contains(replies[0].Body, "write access") {
		t.Errorf("replies = %v, want one write-access refusal", replies)
	}
}

func TestHandleCommentSavesPendingApplyBeforeCommitting(t *testing.T) {
	t.Parallel()

	t.Run("a failed commit leaves it", func(t *testing.T) {
		t.Parallel()

		store := newStore(t, openState())
		gh := apiWithComments()
		gh.Fail = map[string]error{"CommitFiles": errors.New("boom")}
		svc := newService(gh, store, gate.Runners{}, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err == nil {
			t.Fatal("HandleComment() = nil, want the commit error")
		}
		want := &gate.PendingApply{IDs: []string{"p1"}, Message: singleMsg, Parent: "head1"}
		if diff := cmp.Diff(want, loadPR(t, store, 3).PendingApply); diff != "" {
			t.Errorf("PendingApply (-want +got):\n%s", diff)
		}
	})

	t.Run("a saved commit clears it", func(t *testing.T) {
		t.Parallel()

		store := newStore(t, openState())
		svc := newService(apiWithComments(), store, gate.Runners{}, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if pending := loadPR(t, store, 3).PendingApply; pending != nil {
			t.Errorf("PendingApply = %+v, want nil", pending)
		}
	})
}

// The bot's own push of an Apply commit can be handled before the Apply that made
// it is retried; it must not outdate the proposals it applied.
func TestHandlePullRequestAdoptsTheCommitOfACrashedApply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		commit      gate.Commit
		wantState   gate.ProposalStatus
		wantReplies int
	}{
		{name: "ours", commit: gate.Commit{SHA: "botbot1234", Parents: []string{"head1"}, Message: singleMsg, Mine: true}, wantState: gate.ProposalApplied, wantReplies: 1},
		{name: "not authored by the bot", commit: gate.Commit{SHA: "botbot1234", Parents: []string{"head1"}, Message: singleMsg}, wantState: gate.ProposalOutdated},
		{name: "other message", commit: gate.Commit{SHA: "botbot1234", Parents: []string{"head1"}, Message: "docs: other", Mine: true}, wantState: gate.ProposalOutdated},
		{name: "other parent", commit: gate.Commit{SHA: "botbot1234", Parents: []string{"elsewhere"}, Message: singleMsg, Mine: true}, wantState: gate.ProposalOutdated},
		{name: "a later commit is the branch tip", commit: gate.Commit{SHA: "laterlater", Parents: []string{"botbot1234"}, Message: "wip", Mine: true}, wantState: gate.ProposalOutdated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := openState()
			state.SummaryCommentID = 0
			state.PendingApply = &gate.PendingApply{IDs: []string{"p1"}, Message: singleMsg, Parent: "head1"}
			store := newStore(t, state)
			gh := &gatetest.GitHub{Branches: map[string]gate.Commit{"feature": tc.commit}}
			gh.AddComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			svc := newService(gh, store, gate.Runners{Server: runner}, nil)
			pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature"}

			if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
				t.Fatalf("HandlePullRequest() = %v, want nil", err)
			}

			stored := loadPR(t, store, 3)
			got := stored.Proposals[0]
			if got.State != tc.wantState {
				t.Errorf("proposal state = %s, want %s", got.State, tc.wantState)
			}
			if tc.wantState == gate.ProposalApplied && got.AppliedSHA != "botbot1234" {
				t.Errorf("AppliedSHA = %q, want botbot1234", got.AppliedSHA)
			}
			if replies := gh.Replies(); len(replies) != tc.wantReplies {
				t.Errorf("replies = %d, want %d", len(replies), tc.wantReplies)
			}
			if stored.PendingApply != nil {
				t.Errorf("PendingApply = %+v, want it cleared", stored.PendingApply)
			}
		})
	}
}

func TestHandleCommentAppliedReply(t *testing.T) {
	t.Parallel()

	t.Run("carries a marker", func(t *testing.T) {
		t.Parallel()

		gh := apiWithComments()
		svc := newService(gh, newStore(t, threeState()), gate.Runners{}, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if replies := gh.Replies(); len(replies) != 1 || !strings.Contains(replies[0].Body, "<!-- pollux-agent:applied:p1:abcdef1234567 -->") {
			t.Errorf("replies = %v, want one carrying the applied marker", replies)
		}
	})

	t.Run("posted before a crash is adopted, not posted again", func(t *testing.T) {
		t.Parallel()

		state := openState()
		state.Proposals[0].State, state.Proposals[0].AppliedSHA = gate.ProposalApplied, "abcdef1234567"
		store := newStore(t, state)
		gh := apiWithComments()
		prior := gh.AddComment(gate.CommentKindReview, "✅ Applied in abcdef1\n\n<!-- pollux-agent:applied:p1:abcdef1234567 -->")
		svc := newService(gh, store, gate.Runners{}, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if replies := gh.Replies(); len(replies) != 0 {
			t.Errorf("replies = %v, want none", replies)
		}
		if got := loadPR(t, store, 3).Proposals[0].ReplyID; got != prior.ID {
			t.Errorf("ReplyID = %d, want the existing reply %d", got, prior.ID)
		}
	})
}
