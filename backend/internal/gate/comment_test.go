package gate_test

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
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

func TestParseIntentIgnoresTickOnCommentWithoutProposal(t *testing.T) {
	t.Parallel()

	state := gate.PRState{Proposals: []gate.ProposalState{{ID: "p1", CommentID: 2, State: gate.ProposalOpen}}}
	ev := gate.CommentEvent{Kind: gate.CommentKindReview, CommentID: 1, Ticked: "- [x] Apply this change"}

	if got := gate.ParseIntent(ev, state); got.Kind != gate.IntentNone {
		t.Errorf("ParseIntent() = %+v, want no intent for a superseded comment no proposal points at", got)
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

func newHeadGitHub() *fakeGitHub {
	return &fakeGitHub{pullRequest: gate.PullRequest{BaseSHA: "base1", HeadSHA: "new222", Open: true}}
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
		comments := &fakeCommentGitHub{canWrite: true}
		svc := gate.NewService(newHeadGitHub(), comments, &fakeStore{stored: failedSummaryState()}, gate.Runners{Server: runner}, nil, nil)
		ev := rerunEvent()

		if err := svc.HandleComment(t.Context(), ev); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if diff := cmp.Diff([]gate.Reaction{gate.ReactionDone}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
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
			gh.updateErr = tc.updateErr
			comments := &fakeCommentGitHub{canWrite: true}
			svc := gate.NewService(gh, comments, &fakeStore{stored: failedSummaryState()}, gate.Runners{Server: &fakeRunner{err: failure}}, nil, nil)
			ev := rerunEvent()

			err := svc.HandleComment(t.Context(), ev)

			if (err != nil) != tc.wantErr {
				t.Errorf("HandleComment() = %v, wantErr %v", err, tc.wantErr)
			}
			if diff := cmp.Diff([]gate.Reaction{tc.wantReaction}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
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

			gh := &fakeGitHub{}
			store := &fakeStore{}
			comments := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
			svc := gate.NewService(gh, comments, store, gate.Runners{}, nil, nil)
			ev := issueComment(body)

			if err := svc.HandleComment(t.Context(), ev); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			if comments.reactedAs != ev.InstallationID {
				t.Errorf("reacted as installation %d, want the event's %d", comments.reactedAs, ev.InstallationID)
			}
			if len(gh.comments) != 1 || !strings.Contains(gh.comments[0].Body, "hasn't analyzed") {
				t.Errorf("comments = %+v, want one saying the PR was not analyzed yet", gh.comments)
			}
			if len(comments.commits) != 0 || len(gh.calls) != 0 || len(store.saveCalls) != 0 {
				t.Errorf("commits %d, check runs %d, saves %d, want none", len(comments.commits), len(gh.calls), len(store.saveCalls))
			}
			if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleCommentFromAReaderGetsNoSeenReaction(t *testing.T) {
	t.Parallel()

	comments := &fakeCommentGitHub{files: baseFiles()}
	api := apiWithComments()
	svc := gate.NewService(api, comments, &fakeStore{stored: threeState()}, gate.Runners{}, nil, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, comments.reactionLog); diff != "" {
		t.Errorf("reactions added (-want +got):\n%s", diff)
	}
	if len(comments.replies) != 1 || !strings.Contains(comments.replies[0].body, "write access") {
		t.Errorf("replies = %v, want one write-access refusal", comments.replies)
	}
}

func TestHandleCommentSavesPendingApplyBeforeCommitting(t *testing.T) {
	t.Parallel()

	t.Run("a failed commit leaves it", func(t *testing.T) {
		t.Parallel()

		store := &fakeStore{stored: openState(), live: true}
		comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), commitErr: errors.New("boom")}
		svc := gate.NewService(apiWithComments(), comments, store, gate.Runners{}, nil, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err == nil {
			t.Fatal("HandleComment() = nil, want the commit error")
		}
		want := &gate.PendingApply{IDs: []string{"p1"}, Message: singleMsg, Parent: "head1", By: "dev"}
		if diff := cmp.Diff(want, store.stored.PendingApply); diff != "" {
			t.Errorf("PendingApply (-want +got):\n%s", diff)
		}
	})

	t.Run("a saved commit clears it", func(t *testing.T) {
		t.Parallel()

		store := &fakeStore{stored: openState(), live: true}
		comments := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
		svc := gate.NewService(apiWithComments(), comments, store, gate.Runners{}, nil, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if store.stored.PendingApply != nil {
			t.Errorf("PendingApply = %+v, want nil", store.stored.PendingApply)
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
			store := &fakeStore{stored: state, live: true}
			gh := &fakeGitHub{}
			gh.addComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
			comments := &fakeCommentGitHub{branch: tc.commit}
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			svc := gate.NewService(gh, comments, store, gate.Runners{Server: runner}, nil, nil)
			pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature"}

			if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
				t.Fatalf("HandlePullRequest() = %v, want nil", err)
			}

			got := store.stored.Proposals[0]
			if got.State != tc.wantState {
				t.Errorf("proposal state = %s, want %s", got.State, tc.wantState)
			}
			if tc.wantState == gate.ProposalApplied && got.AppliedSHA != "botbot1234" {
				t.Errorf("AppliedSHA = %q, want botbot1234", got.AppliedSHA)
			}
			if len(comments.replies) != tc.wantReplies {
				t.Errorf("replies = %d, want %d", len(comments.replies), tc.wantReplies)
			}
			if store.stored.PendingApply != nil {
				t.Errorf("PendingApply = %+v, want it cleared", store.stored.PendingApply)
			}
		})
	}
}

func TestHandleCommentAppliedReply(t *testing.T) {
	t.Parallel()

	t.Run("carries a marker", func(t *testing.T) {
		t.Parallel()

		comments := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
		svc := gate.NewService(apiWithComments(), comments, &fakeStore{stored: threeState(), live: true}, gate.Runners{}, nil, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if len(comments.replies) != 1 || !strings.Contains(comments.replies[0].body, "<!-- pollux-agent:applied:p1:abcdef1234567 -->") {
			t.Errorf("replies = %v, want one carrying the applied marker", comments.replies)
		}
	})

	t.Run("posted before a crash is adopted, not posted again", func(t *testing.T) {
		t.Parallel()

		state := openState()
		state.Proposals[0].State, state.Proposals[0].AppliedSHA = gate.ProposalApplied, "abcdef1234567"
		store := &fakeStore{stored: state, live: true}
		api := apiWithComments()
		prior := api.addComment(gate.CommentKindReview, "✅ Applied in abcdef1\n\n<!-- pollux-agent:applied:p1:abcdef1234567 -->")
		comments := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
		svc := gate.NewService(api, comments, store, gate.Runners{}, nil, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if len(comments.replies) != 0 {
			t.Errorf("replies = %v, want none", comments.replies)
		}
		if got := store.stored.Proposals[0].ReplyID; got != prior.ID {
			t.Errorf("ReplyID = %d, want the existing reply %d", got, prior.ID)
		}
	})
}
