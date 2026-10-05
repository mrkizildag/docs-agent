package gate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func botCommit() gate.Commit {
	return gate.Commit{SHA: "botbot1234", Parents: []string{"head1"}, Message: singleMsg, Mine: true}
}

func pendingState() gate.PRState {
	state := openState()
	state.PendingApply = &gate.PendingApply{IDs: []string{"p1"}, Message: singleMsg, Parent: "head1"}
	return state
}

func countReplies(replies []reply, substr string) int {
	n := 0
	for _, r := range replies {
		if strings.Contains(r.body, substr) {
			n++
		}
	}
	return n
}

func TestHandleCommentRetryAfterTheCommitLanded(t *testing.T) {
	t.Parallel()

	store := &fakeStore{stored: pendingState(), live: true}
	api := apiWithComments()
	api.pullRequest = gate.PullRequest{HeadSHA: "botbot1234", Open: true}
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), byAdd: map[string]gate.Commit{"botbot1234": botCommit()}}
	svc := gate.NewService(api, comments, store, gate.Runners{})

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if n := countReplies(comments.replies, "nothing was committed"); n != 0 {
		t.Errorf("refusals = %d, want 0", n)
	}
	if n := countReplies(comments.replies, "✅ Applied in botbot1"); n != 1 || len(comments.commits) != 0 {
		t.Errorf("applied replies = %d, commits = %d, want 1 and 0", n, len(comments.commits))
	}
	if got := store.stored.Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" || store.stored.PendingApply != nil {
		t.Errorf("state = %+v, pending = %+v, want applied at botbot1234 and no pending apply", got, store.stored.PendingApply)
	}
}

func TestHandlePullRequestAdoptsTheCommitEvenWhenTheBranchMovedOn(t *testing.T) {
	t.Parallel()

	store := &fakeStore{stored: pendingState(), live: true}
	gh := &fakeGitHub{}
	gh.addComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	comments := &fakeCommentGitHub{
		branch: gate.Commit{SHA: "laterlater", Parents: []string{"botbot1234"}, Message: "wip"},
		byAdd:  map[string]gate.Commit{"botbot1234": botCommit()},
	}
	svc := gate.NewService(gh, comments, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}})
	pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature"}

	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if got := store.stored.Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" {
		t.Errorf("proposal = %+v, want applied at botbot1234", got)
	}
}

func TestHandleRerunAdoptsThePendingApply(t *testing.T) {
	t.Parallel()

	state := pendingState()
	state.SummaryCommentID = 0
	store := &fakeStore{stored: state, live: true}
	gh := &fakeGitHub{pullRequest: gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature", Open: true}}
	gh.addComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	comments := &fakeCommentGitHub{byAdd: map[string]gate.Commit{"botbot1234": botCommit()}}
	svc := gate.NewService(gh, comments, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}})

	if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 1, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 3}}); err != nil {
		t.Fatalf("HandleRerun() = %v, want nil", err)
	}
	if got := store.stored.Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" {
		t.Errorf("proposal = %+v, want applied at botbot1234", got)
	}
}

func TestHandleRerunOnANewHeadCancelsAPendingSkipOnce(t *testing.T) {
	t.Parallel()

	state := skipBase()
	state.Run = nil
	state.PendingSkip = &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}
	gh := newHeadGitHub()
	store := &fakeStore{stored: state, live: true}
	svc := gate.NewService(gh, &fakeCommentGitHub{}, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}})
	req := gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}}

	for range 2 {
		if err := svc.HandleRerun(t.Context(), req); err != nil {
			t.Fatalf("HandleRerun() = %v, want nil", err)
		}
	}
	notes := 0
	for _, c := range gh.comments {
		if strings.Contains(c.Body, "was cancelled") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("cancellation notes = %d, want 1", notes)
	}
}

func TestHandleCommentRefusesARejectedCommit(t *testing.T) {
	t.Parallel()

	store := &fakeStore{stored: openState(), live: true}
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), commitErr: fmt.Errorf("%w: protected branch", gate.ErrCommitRejected)}
	svc := gate.NewService(apiWithComments(), comments, store, gate.Runners{})
	ev := reviewTick(1)

	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if len(comments.replies) != 1 || !strings.Contains(comments.replies[0].body, "GitHub rejected the commit (a protected branch, or a symlink or submodule at a doc path); nothing was committed.") {
		t.Errorf("replies = %v, want one rejection", comments.replies)
	}
	if store.stored.PendingApply != nil {
		t.Errorf("PendingApply = %+v, want it cleared", store.stored.PendingApply)
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
		t.Errorf("reactions (-want +got):\n%s", diff)
	}
}

func TestHandleCommentApplyAllWithAppliedProposalsIsNotNothingLeft(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		state     func(*gate.PRState)
		wantSaid  int
		wantEvent gate.Reaction
	}{
		{name: "one is applied and replied", state: func(s *gate.PRState) {
			s.Proposals[0].State, s.Proposals[0].AppliedSHA, s.Proposals[0].ReplyID = gate.ProposalApplied, "abcdef1234567", 5
		}, wantEvent: gate.ReactionDone},
		{name: "none is applied", state: func(s *gate.PRState) { s.Proposals[0].State = gate.ProposalOutdated }, wantSaid: 1, wantEvent: gate.ReactionDone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := openState()
			tc.state(&state)
			api := apiWithComments()
			comments := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
			svc := gate.NewService(api, comments, &fakeStore{stored: state, live: true}, gate.Runners{})
			ev := summaryTick("Apply all")

			if err := svc.HandleComment(t.Context(), ev); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			if api.createIssue != tc.wantSaid {
				t.Errorf("issue comments = %d, want %d", api.createIssue, tc.wantSaid)
			}
			if diff := cmp.Diff([]gate.Reaction{tc.wantEvent}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleCommentSupersededRerunDropsTheSeenReaction(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	runner := &fakeRunner{err: errors.New("interrupted"), onStart: func() { cancel(errors.New("superseded")) }}
	comments := &fakeCommentGitHub{canWrite: true}
	svc := gate.NewService(newHeadGitHub(), comments, &fakeStore{stored: failedSummaryState()}, gate.Runners{Server: runner})
	ev := rerunEvent()

	if err := svc.HandleComment(ctx, ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if got := comments.reactionsOn(ev.Kind, ev.CommentID); len(got) != 0 {
		t.Errorf("reactions = %v, want none", got)
	}
}

func TestHandleCommentReappliedProposalGetsANewReply(t *testing.T) {
	t.Parallel()

	store := &fakeStore{stored: openState(), live: true}
	api := apiWithComments()
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), api: api}
	svc := gate.NewService(api, comments, store, gate.Runners{})

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	reopened := store.stored
	reopened.Proposals = append([]gate.ProposalState(nil), reopened.Proposals...)
	reopened.Proposals[0].State, reopened.Proposals[0].AppliedSHA, reopened.Proposals[0].ReplyID = gate.ProposalOpen, "", 0
	store.stored = reopened
	comments.sha = "second99999"
	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() second = %v, want nil", err)
	}

	if len(comments.replies) != 2 || !strings.Contains(comments.replies[1].body, "✅ Applied in second9") {
		t.Errorf("replies = %v, want a second one naming the new commit", comments.replies)
	}
}

func TestHandleCommentStaleApplyUnderAPRSkip(t *testing.T) {
	t.Parallel()

	state := openState()
	state.ProposalsSHA = "older"
	state.Skip = &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated", HeadSHA: "older"}
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
	svc := gate.NewService(apiWithComments(), comments, &fakeStore{stored: state, live: true}, gate.Runners{})

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if want := "This PR is skipped, so its proposals are not refreshed; nothing was committed."; len(comments.replies) != 1 || !strings.Contains(comments.replies[0].body, want) {
		t.Errorf("replies = %v, want %q", comments.replies, want)
	}
}
