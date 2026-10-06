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
	svc := gate.NewService(api, comments, store, gate.Runners{}, nil, nil)

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
	svc := gate.NewService(gh, comments, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil, nil)
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
	svc := gate.NewService(gh, comments, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil, nil)

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
	svc := gate.NewService(gh, &fakeCommentGitHub{}, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil, nil)
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
	svc := gate.NewService(apiWithComments(), comments, store, gate.Runners{}, nil, nil)
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

func TestHandleCommentRefusesARejectedCommitWithItsReason(t *testing.T) {
	t.Parallel()

	store := &fakeStore{stored: openState(), live: true}
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), commitErr: &gate.CommitRejectedError{Reason: "the branch is protected"}}
	svc := gate.NewService(apiWithComments(), comments, store, gate.Runners{}, nil, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	want := "GitHub rejected the commit: the branch is protected; nothing was committed."
	if len(comments.replies) != 1 || !strings.Contains(comments.replies[0].body, want) {
		t.Errorf("replies = %v, want one containing %q", comments.replies, want)
	}
}

func TestHandleCommentRetryDoesNotClaimTargetsTheAdoptedCommitMissed(t *testing.T) {
	t.Parallel()

	state := threeState()
	state.PendingApply = &gate.PendingApply{IDs: []string{"p1"}, Message: singleMsg, Parent: "head1"}
	store := &fakeStore{stored: state, live: true}
	api := apiWithComments()
	api.pullRequest = gate.PullRequest{HeadSHA: "botbot1234", Open: true}
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), byAdd: map[string]gate.Commit{"botbot1234": botCommit()}}
	svc := gate.NewService(api, comments, store, gate.Runners{}, nil, nil)
	ev := reviewTick(2)

	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if got := store.stored.Proposals; got[0].State != gate.ProposalApplied || got[1].State != gate.ProposalOpen {
		t.Errorf("proposal states = %s, %s, want applied, open", got[0].State, got[1].State)
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
		t.Errorf("reactions (-want +got):\n%s", diff)
	}
}

func TestHandlePullRequestAdoptsACommitUnderAUserPush(t *testing.T) {
	t.Parallel()

	store := &fakeStore{stored: pendingState(), live: true}
	gh := &fakeGitHub{}
	gh.addComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	comments := &fakeCommentGitHub{byAdd: map[string]gate.Commit{
		"user1":      {SHA: "user1", Parents: []string{"botbot1234"}, Message: "wip"},
		"botbot1234": botCommit(),
	}}
	svc := gate.NewService(gh, comments, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil, nil)
	pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "user1", HeadRef: "feature"}

	if err := svc.HandlePullRequest(t.Context(), pr); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if got := store.stored.Proposals[0]; got.State != gate.ProposalApplied || got.AppliedSHA != "botbot1234" {
		t.Errorf("proposal = %+v, want applied at botbot1234", got)
	}
}

func TestHandlePullRequestAnalyzesTheNewHeadWhenAdoptedCommentsFail(t *testing.T) {
	t.Parallel()

	store := &fakeStore{stored: pendingState(), live: true}
	gh := &fakeGitHub{}
	gh.addComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	comments := &fakeCommentGitHub{replyErr: errors.New("502"), byAdd: map[string]gate.Commit{"botbot1234": botCommit()}}
	svc := gate.NewService(gh, comments, store, gate.Runners{Server: &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}}, nil, nil)
	pr := gate.PullRequest{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, BaseSHA: "base1", HeadSHA: "botbot1234", HeadRef: "feature"}

	if err := svc.HandlePullRequest(t.Context(), pr); err == nil {
		t.Error("HandlePullRequest() = nil, want the reply error")
	}
	if len(gh.calls) != 1 {
		t.Errorf("check runs created = %d, want 1 for the new head", len(gh.calls))
	}
}

// ctxGitHub and ctxStore fail like the real adapters once their ctx is cancelled.
type ctxGitHub struct{ *fakeGitHub }

func (g ctxGitHub) CreateIssueComment(ctx context.Context, id int64, owner, repo string, number int, body string) (gate.Comment, error) {
	if err := ctx.Err(); err != nil {
		return gate.Comment{}, fmt.Errorf("create issue comment: %w", err)
	}
	return g.fakeGitHub.CreateIssueComment(ctx, id, owner, repo, number, body)
}

type ctxStore struct{ *fakeStore }

func (s ctxStore) LoadPR(ctx context.Context, owner, repo string, number int) (gate.PRState, error) {
	if err := ctx.Err(); err != nil {
		return gate.PRState{}, fmt.Errorf("load pr: %w", err)
	}
	return s.fakeStore.LoadPR(ctx, owner, repo, number)
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
	svc := gate.NewService(ctxGitHub{gh}, &fakeCommentGitHub{}, ctxStore{&fakeStore{stored: state, live: true}}, gate.Runners{Server: runner}, nil, nil)

	_ = svc.HandleRerun(ctx, gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}})

	if n := countComments(gh.comments, "was cancelled"); n != 1 {
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
			api := apiWithComments()
			comments := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
			svc := gate.NewService(api, comments, &fakeStore{stored: state, live: true}, gate.Runners{}, nil, nil)
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
	svc := gate.NewService(newHeadGitHub(), comments, &fakeStore{stored: failedSummaryState()}, gate.Runners{Server: runner}, nil, nil)
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
	svc := gate.NewService(api, comments, store, gate.Runners{}, nil, nil)

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
	svc := gate.NewService(apiWithComments(), comments, &fakeStore{stored: state, live: true}, gate.Runners{}, nil, nil)

	if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if want := "This PR is skipped, so its proposals are not refreshed; nothing was committed."; len(comments.replies) != 1 || !strings.Contains(comments.replies[0].body, want) {
		t.Errorf("replies = %v, want %q", comments.replies, want)
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
	store := &fakeStore{stored: state, live: true}
	api := apiWithComments()
	api.pullRequest = gate.PullRequest{HeadSHA: "botall1234", Open: true}
	landed := gate.Commit{SHA: "botall1234", Parents: []string{"head1"}, Message: msg, Mine: true}
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), api: api, byAdd: map[string]gate.Commit{"botall1234": landed}}
	svc := gate.NewService(api, comments, store, gate.Runners{}, nil, nil)

	for range 2 {
		if err := svc.HandleComment(t.Context(), issueComment("/pollux-agent apply")); err != nil {
			t.Fatalf("HandleComment() = %v", err)
		}
	}

	if len(comments.commits) != 0 {
		t.Errorf("commits = %d, want 0 (the landed commit is adopted)", len(comments.commits))
	}
	for _, p := range store.stored.Proposals {
		if p.State != gate.ProposalApplied || p.AppliedSHA != "botall1234" {
			t.Errorf("proposal %s = %v at %q, want applied at botall1234", p.ID, p.State, p.AppliedSHA)
		}
	}
	if n := countReplies(comments.replies, "Applied in botall1"); n != 3 {
		t.Errorf("applied replies = %d, want 3; replies = %+v", n, comments.replies)
	}
	if n := countReplies(comments.replies, "nothing was committed"); n != 0 {
		t.Errorf("refusals = %d, want 0", n)
	}
	for _, id := range []int64{1, 2, 3} {
		for _, c := range api.comments {
			if c.ID == id && !strings.Contains(c.Body, "- [x] Apply this change") {
				t.Errorf("proposal comment %d not ticked: %q", id, c.Body)
			}
		}
	}
}
