package gate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
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

func countReplies(replies []gatetest.Reply, substr string) int {
	n := 0
	for _, r := range replies {
		if strings.Contains(r.Body, substr) {
			n++
		}
	}
	return n
}

func TestHandleCommentRetryAfterTheCommitLanded(t *testing.T) {
	t.Parallel()

	store := newStore(t, pendingState())
	gh := apiWithComments()
	gh.PullRequest = gate.PullRequest{HeadSHA: "botbot1234", Open: true}
	gh.KnownCommits = map[string]gate.Commit{"botbot1234": botCommit()}
	svc := newService(gh, store, gate.Runners{}, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if n := countReplies(gh.Replies(), "nothing was committed"); n != 0 {
		t.Errorf("refusals = %d, want 0", n)
	}
	if n := countReplies(gh.Replies(), "✅ Applied in botbot1"); n != 1 || len(gh.Committed()) != 0 {
		t.Errorf("applied replies = %d, commits = %d, want 1 and 0", n, len(gh.Committed()))
	}
	stored := loadPR(t, store, 3)
	if got := stored.Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" || stored.PendingApply != nil {
		t.Errorf("state = %+v, pending = %+v, want applied at botbot1234 and no pending apply", got, stored.PendingApply)
	}
}

func TestHandlePullRequestAdoptsTheCommitEvenWhenTheBranchMovedOn(t *testing.T) {
	t.Parallel()

	store := newStore(t, pendingState())
	gh := &gatetest.GitHub{
		Branches:     map[string]gate.Commit{"feature": {SHA: "laterlater", Parents: []string{"botbot1234"}, Message: "wip"}},
		KnownCommits: map[string]gate.Commit{"botbot1234": botCommit()},
	}
	gh.AddComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil)
	pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature"}

	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if got := loadPR(t, store, 3).Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" {
		t.Errorf("proposal = %+v, want applied at botbot1234", got)
	}
}

func TestHandleRerunAdoptsThePendingApply(t *testing.T) {
	t.Parallel()

	state := pendingState()
	state.SummaryCommentID = 0
	store := newStore(t, state)
	gh := &gatetest.GitHub{
		PullRequest:  gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature", Open: true},
		KnownCommits: map[string]gate.Commit{"botbot1234": botCommit()},
	}
	gh.AddComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil)

	if err := svc.HandleRerun(t.Context(), gate.RerunRequest{InstallationID: 1, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 3}}); err != nil {
		t.Fatalf("HandleRerun() = %v, want nil", err)
	}
	if got := loadPR(t, store, 3).Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" {
		t.Errorf("proposal = %+v, want applied at botbot1234", got)
	}
}

func TestHandleRerunOnANewHeadCancelsAPendingSkipOnce(t *testing.T) {
	t.Parallel()

	state := skipBase()
	state.Run = nil
	state.PendingSkip = &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}
	gh := newHeadGitHub()
	store := newStore(t, state)
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil)
	req := gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}}

	for range 2 {
		if err := svc.HandleRerun(t.Context(), req); err != nil {
			t.Fatalf("HandleRerun() = %v, want nil", err)
		}
	}
	if notes := countComments(gh.Comments(), "was cancelled"); notes != 1 {
		t.Errorf("cancellation notes = %d, want 1", notes)
	}
}

func TestHandleCommentRefusesARejectedCommit(t *testing.T) {
	t.Parallel()

	store := newStore(t, openState())
	gh := apiWithComments()
	gh.Fail = map[string]error{"CommitFiles": fmt.Errorf("%w: protected branch", gate.ErrCommitRejected)}
	svc := newService(gh, store, gate.Runners{}, nil)
	ev := reviewTick(1)

	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if replies := gh.Replies(); len(replies) != 1 || !strings.Contains(replies[0].Body, "GitHub rejected the commit (a protected branch, or a symlink or submodule at a doc path); nothing was committed.") {
		t.Errorf("replies = %v, want one rejection", replies)
	}
	if pending := loadPR(t, store, 3).PendingApply; pending != nil {
		t.Errorf("PendingApply = %+v, want it cleared", pending)
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
		t.Errorf("reactions (-want +got):\n%s", diff)
	}
}

func TestHandleCommentRefusesARejectedCommitWithItsReason(t *testing.T) {
	t.Parallel()

	gh := apiWithComments()
	gh.Fail = map[string]error{"CommitFiles": &gate.CommitRejectedError{Reason: "the branch is protected"}}
	svc := newService(gh, newStore(t, openState()), gate.Runners{}, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	want := "GitHub rejected the commit: the branch is protected; nothing was committed."
	if replies := gh.Replies(); len(replies) != 1 || !strings.Contains(replies[0].Body, want) {
		t.Errorf("replies = %v, want one containing %q", replies, want)
	}
}

func TestHandleCommentRetryDoesNotClaimTargetsTheAdoptedCommitMissed(t *testing.T) {
	t.Parallel()

	state := threeState()
	state.PendingApply = &gate.PendingApply{IDs: []string{"p1"}, Message: singleMsg, Parent: "head1"}
	store := newStore(t, state)
	gh := apiWithComments()
	gh.PullRequest = gate.PullRequest{HeadSHA: "botbot1234", Open: true}
	gh.KnownCommits = map[string]gate.Commit{"botbot1234": botCommit()}
	svc := newService(gh, store, gate.Runners{}, nil)
	ev := reviewTick(2)

	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if got := loadPR(t, store, 3).Proposals; got[0].State != gate.ProposalApplied || got[1].State != gate.ProposalOpen {
		t.Errorf("proposal states = %s, %s, want applied, open", got[0].State, got[1].State)
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
		t.Errorf("reactions (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestAdoptsACommitUnderAUserPush(t *testing.T) {
	t.Parallel()

	store := newStore(t, pendingState())
	gh := &gatetest.GitHub{KnownCommits: map[string]gate.Commit{
		"user1":      {SHA: "user1", Parents: []string{"botbot1234"}, Message: "wip"},
		"botbot1234": botCommit(),
	}}
	gh.AddComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil)
	pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "user1", HeadRef: "feature"}

	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if got := loadPR(t, store, 3).Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" {
		t.Errorf("proposal = %+v, want applied at botbot1234", got)
	}
}

func TestHandlePullRequestAnalyzesTheNewHeadWhenAdoptedCommentsFail(t *testing.T) {
	t.Parallel()

	store := newStore(t, pendingState())
	gh := &gatetest.GitHub{
		Fail:         map[string]error{"ReplyToReviewComment": errors.New("502")},
		KnownCommits: map[string]gate.Commit{"botbot1234": botCommit()},
	}
	gh.AddComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	svc := newService(gh, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil)
	pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature"}

	if err := svc.HandlePullRequest(t.Context(), pr); err == nil {
		t.Error("HandlePullRequest() = nil, want the reply error")
	}
	if n := gh.CallCount("CreateCheckRun"); n != 1 {
		t.Errorf("check runs created = %d, want 1 for the new head", n)
	}
}

func TestHandleRerunSupersededStillPostsTheSkipCancellationNote(t *testing.T) {
	t.Parallel()

	state := skipBase()
	state.Run = nil
	state.PendingSkip = &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}
	gh := newHeadGitHub()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	runner := &fakeRunner{err: errors.New("interrupted"), onStart: func() { cancel(errors.New("superseded")) }}
	svc := newService(gh, newStore(t, state), gate.Runners{Server: runner}, nil)

	_ = svc.HandleRerun(ctx, gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}})

	if n := countComments(gh.Comments(), "was cancelled"); n != 1 {
		t.Errorf("cancellation notes = %d, want 1", n)
	}
}

func countComments(comments []gate.Comment, substr string) int {
	n := 0
	for _, c := range comments {
		if strings.Contains(c.Body, substr) {
			n++
		}
	}
	return n
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
			gh := apiWithComments()
			svc := newService(gh, newStore(t, state), gate.Runners{}, nil)
			ev := summaryTick("Apply all")

			if err := svc.HandleComment(t.Context(), ev); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			if n := gh.CallCount("CreateIssueComment"); n != tc.wantSaid {
				t.Errorf("issue comments = %d, want %d", n, tc.wantSaid)
			}
			if diff := cmp.Diff([]gate.Reaction{tc.wantEvent}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
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
	gh := newHeadGitHub()
	svc := newService(gh, newStore(t, failedSummaryState()), gate.Runners{Server: runner}, nil)
	ev := rerunEvent()

	if err := svc.HandleComment(ctx, ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if got := gh.Reactions(ev.Kind, ev.CommentID); len(got) != 0 {
		t.Errorf("reactions = %v, want none", got)
	}
}

func TestHandleCommentReappliedProposalGetsANewReply(t *testing.T) {
	t.Parallel()

	store := newStore(t, openState())
	gh := apiWithComments()
	svc := newService(gh, store, gate.Runners{}, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	reopened := loadPR(t, store, 3)
	reopened.Proposals[0].State, reopened.Proposals[0].AppliedSHA, reopened.Proposals[0].ReplyID = gate.ProposalOpen, "", 0
	if err := store.SavePR(t.Context(), reopened); err != nil {
		t.Fatalf("SavePR() = %v, want nil", err)
	}
	gh.CommitSHA = "second99999"
	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() second = %v, want nil", err)
	}

	if replies := gh.Replies(); len(replies) != 2 || !strings.Contains(replies[1].Body, "✅ Applied in second9") {
		t.Errorf("replies = %v, want a second one naming the new commit", replies)
	}
}

func TestHandleCommentStaleApplyUnderAPRSkip(t *testing.T) {
	t.Parallel()

	state := openState()
	state.ProposalsSHA = "older"
	state.Skip = &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated", HeadSHA: "older"}
	gh := apiWithComments()
	svc := newService(gh, newStore(t, state), gate.Runners{}, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if want, replies := "This PR is skipped, so its proposals are not refreshed; nothing was committed.", gh.Replies(); len(replies) != 1 || !strings.Contains(replies[0].Body, want) {
		t.Errorf("replies = %v, want %q", replies, want)
	}
}

// An Apply all redelivered after its commit landed but
// before the state was saved commits nothing new, and still marks, ticks, and
// replies to every proposal exactly once.
func TestHandleCommentApplyAllRedeliveredAfterCrashCommitsOnce(t *testing.T) {
	t.Parallel()

	const msg = "docs: apply 3 pollux-agent proposals"
	state := threeState()
	state.PendingApply = &gate.PendingApply{IDs: []string{"p1", "p2", "p3"}, Message: msg, Parent: "head1"}
	store := newStore(t, state)
	gh := apiWithComments()
	gh.PullRequest = gate.PullRequest{HeadSHA: "botall1234", Open: true}
	gh.KnownCommits = map[string]gate.Commit{"botall1234": {SHA: "botall1234", Parents: []string{"head1"}, Message: msg, Mine: true}}
	svc := newService(gh, store, gate.Runners{}, nil)

	for range 2 {
		if err := svc.HandleComment(t.Context(), issueComment("/pollux-agent apply")); err != nil {
			t.Fatalf("HandleComment() = %v", err)
		}
	}

	if commits := gh.Committed(); len(commits) != 0 {
		t.Errorf("commits = %d, want 0 (the landed commit is adopted)", len(commits))
	}
	for _, p := range loadPR(t, store, 3).Proposals {
		if p.State != gate.ProposalApplied || p.AppliedSHA != "botall1234" {
			t.Errorf("proposal %s = %v at %q, want applied at botall1234", p.ID, p.State, p.AppliedSHA)
		}
	}
	if n := countReplies(gh.Replies(), "Applied in botall1"); n != 3 {
		t.Errorf("applied replies = %d, want 3; replies = %+v", n, gh.Replies())
	}
	if n := countReplies(gh.Replies(), "nothing was committed"); n != 0 {
		t.Errorf("refusals = %d, want 0", n)
	}
	for _, id := range []int64{1, 2, 3} {
		for _, c := range gh.Comments() {
			if c.ID == id && !strings.Contains(c.Body, "- [x] Apply this change") {
				t.Errorf("proposal comment %d not ticked: %q", id, c.Body)
			}
		}
	}
}
