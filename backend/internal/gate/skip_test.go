package gate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

type skipCommentGitHub struct{ canWrite bool }

func (f skipCommentGitHub) Permission(context.Context, int64, string, string, string) (bool, error) {
	return f.canWrite, nil
}

func (skipCommentGitHub) FileAtRef(context.Context, int64, string, string, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (skipCommentGitHub) CommitFiles(context.Context, int64, string, string, string, string, []gate.FileChange, string) (string, error) {
	return "", nil
}

func (skipCommentGitHub) BranchCommit(context.Context, int64, string, string, string) (gate.Commit, error) {
	return gate.Commit{}, nil
}

func (skipCommentGitHub) ReplyToReviewComment(context.Context, int64, string, string, int, int64, string) (gate.Comment, error) {
	return gate.Comment{}, nil
}

func skipBase() gate.PRState {
	return gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7,
		HeadSHA: "head1", CheckRunID: 555, SummaryCommentID: 9,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1"},
	}
}

func skipService(state gate.PRState) (*gate.Service, *fakeGitHub, *fakeStore) {
	gh := &fakeGitHub{}
	store := &fakeStore{stored: state}
	svc := gate.NewService(gh, store, gate.Runners{}).WithComments(skipCommentGitHub{canWrite: true})
	return svc, gh, store
}

func skipEvent(kind gate.CommentKind, ticked, body string) gate.CommentEvent {
	return gate.CommentEvent{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, Sender: "dev", CommentID: 9, Kind: kind, Ticked: ticked, Body: body}
}

func TestOnSkip(t *testing.T) {
	t.Parallel()

	prev := skipBase()
	prev.PendingSkip = &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}

	got, run := gate.OnSkip(prev, gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated code"})

	wantSkip := &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated code", HeadSHA: "head1"}
	if diff := cmp.Diff(wantSkip, got.Skip); diff != "" {
		t.Errorf("Skip (-want +got):\n%s", diff)
	}
	if got.PendingSkip != nil || got.Run != nil {
		t.Errorf("OnSkip() PendingSkip = %v, Run = %v, want both nil", got.PendingSkip, got.Run)
	}
	if run.Status != gate.StatusCompleted || run.Conclusion != gate.ConclusionSuccess || run.HeadSHA != "head1" {
		t.Errorf("check run = %+v, want completed success on head1", run)
	}
	for _, want := range []string{"@dev", "PR", "generated code"} {
		if !strings.Contains(run.Title+run.Summary, want) {
			t.Errorf("check run text %q %q lacks %q", run.Title, run.Summary, want)
		}
	}
	if prev.Skip != nil || prev.Run == nil {
		t.Error("OnSkip mutated its input")
	}
}

func TestHandleCommentSkipAsk(t *testing.T) {
	t.Parallel()

	svc, gh, store := skipService(skipBase())
	ev := skipEvent(gate.CommentKindIssue, "- [x] Skip this commit", "")

	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}

	if store.saved == nil || store.saved.PendingSkip == nil || *store.saved.PendingSkip != (gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}) {
		t.Fatalf("saved state = %+v, want PendingSkip for dev at commit scope", store.saved)
	}
	if len(gh.comments) != 1 || gh.comments[0].Body != "@dev, reply with the reason for skipping this commit; your next comment on this PR becomes the reason." {
		t.Errorf("comments = %+v, want one ask for dev", gh.comments)
	}
	if gh.editIssue != 1 {
		t.Errorf("summary edits = %d, want 1", gh.editIssue)
	}
	if len(gh.updates) != 0 || store.saved.Skip != nil {
		t.Errorf("check run updates = %d, Skip = %v, want a pending ask only", len(gh.updates), store.saved.Skip)
	}

	store.stored = *store.saved
	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() redelivery = %v, want nil", err)
	}
	if gh.createIssue != 1 || len(store.saveCalls) != 1 {
		t.Errorf("after redelivery: asks = %d, saves = %d, want 1 and 1", gh.createIssue, len(store.saveCalls))
	}
}

func TestHandleCommentSkipAskForPR(t *testing.T) {
	t.Parallel()

	svc, gh, _ := skipService(skipBase())

	if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "- [x] Skip this PR", "")); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if len(gh.comments) != 1 || !strings.Contains(gh.comments[0].Body, "skipping this PR;") {
		t.Errorf("comments = %+v, want an ask naming this PR", gh.comments)
	}
}

func TestHandleCommentSkipWithReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state func() gate.PRState
		body  string
		scope gate.SkipScope
		// wantCreate is whether the check run is created rather than updated.
		wantCreate bool
	}{
		{name: "commit command", state: skipBase, body: "/pollux-agent skip typo fix", scope: gate.SkipCommit},
		{name: "pr command", state: skipBase, body: "/pollux-agent skip-pr typo fix", scope: gate.SkipPR},
		{name: "no check run yet", state: func() gate.PRState { s := skipBase(); s.CheckRunID = 0; return s }, body: "/pollux-agent skip typo fix", scope: gate.SkipCommit, wantCreate: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc, gh, store := skipService(tc.state())
			gh.checkRunID = 777

			if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", tc.body)); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}

			got := store.saved
			if got == nil || got.Skip == nil {
				t.Fatalf("saved state = %+v, want an active skip", got)
			}
			wantSkip := gate.Skip{User: "dev", Scope: tc.scope, Reason: "typo fix", HeadSHA: "head1"}
			if *got.Skip != wantSkip {
				t.Errorf("Skip = %+v, want %+v", *got.Skip, wantSkip)
			}
			if got.Run != nil {
				t.Errorf("Run = %+v, want nil: the skip wins over the awaited run", got.Run)
			}
			if tc.wantCreate {
				if len(gh.calls) != 1 || gh.calls[0].run.Conclusion != gate.ConclusionSuccess || got.CheckRunID != 777 {
					t.Errorf("created check runs = %+v, CheckRunID = %d, want one success and 777", gh.calls, got.CheckRunID)
				}
			} else if len(gh.updates) != 1 || gh.updates[0].id != 555 || gh.updates[0].run.Conclusion != gate.ConclusionSuccess || len(gh.calls) != 0 {
				t.Errorf("check run updates = %+v, creates = %d, want one success update of 555", gh.updates, len(gh.calls))
			}
			if gh.editIssue != 1 {
				t.Errorf("summary edits = %d, want 1", gh.editIssue)
			}

			store.stored = *got
			if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", tc.body)); err != nil {
				t.Fatalf("HandleComment() redelivery = %v, want nil", err)
			}
			if len(store.saveCalls) != 1 {
				t.Errorf("saves after redelivery = %d, want 1", len(store.saveCalls))
			}
		})
	}
}

func TestHandleCommentSkipReason(t *testing.T) {
	t.Parallel()

	t.Run("pending ask becomes the skip", func(t *testing.T) {
		t.Parallel()

		state := skipBase()
		state.PendingSkip = &gate.SkipAsk{User: "dev", Scope: gate.SkipPR}
		svc, gh, store := skipService(state)

		if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "generated code")); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}

		want := &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated code", HeadSHA: "head1"}
		if diff := cmp.Diff(want, store.saved.Skip); diff != "" {
			t.Errorf("Skip (-want +got):\n%s", diff)
		}
		if store.saved.PendingSkip != nil {
			t.Errorf("PendingSkip = %+v, want nil", store.saved.PendingSkip)
		}
		if len(gh.updates) != 1 || !strings.Contains(gh.updates[0].run.Summary, "generated code") {
			t.Errorf("check run updates = %+v, want one naming the reason", gh.updates)
		}
	})

	t.Run("no pending ask does nothing", func(t *testing.T) {
		t.Parallel()

		svc, gh, store := skipService(skipBase())

		if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "just a comment")); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if len(store.saveCalls) != 0 || len(gh.updates) != 0 || len(gh.comments) != 0 {
			t.Errorf("saves = %d, updates = %d, comments = %d, want none", len(store.saveCalls), len(gh.updates), len(gh.comments))
		}
	})
}

func TestHandleCommentSkipUpdatesCheckRunFromPush(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{checkRunID: 321}
	store := &fakeStore{}
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	svc := gate.NewService(gh, store, gate.Runners{Server: runner}).WithComments(skipCommentGitHub{canWrite: true})

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if store.saved == nil || store.saved.CheckRunID != 321 {
		t.Fatalf("saved state = %+v, want CheckRunID 321", store.saved)
	}

	if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "/pollux-agent skip typo fix")); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}

	if len(gh.calls) != 1 {
		t.Errorf("created check runs = %d, want 1: the skip must reuse the push's check run", len(gh.calls))
	}
	if len(gh.updates) != 2 || gh.updates[1].id != 321 || gh.updates[1].run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("check run updates = %+v, want the push's conclusion then a success update of 321", gh.updates)
	}
}

func (skipCommentGitHub) React(context.Context, int64, string, string, gate.CommentKind, int64, gate.Reaction) (int64, error) {
	return 0, nil
}

func (skipCommentGitHub) Unreact(context.Context, int64, string, string, gate.CommentKind, int64, int64) error {
	return nil
}

func TestHandleCommentSkipReactsDone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event gate.CommentEvent
	}{
		{name: "skip command", event: skipEvent(gate.CommentKindIssue, "", "/pollux-agent skip typo fix")},
		{name: "skip tick asks for the reason", event: skipEvent(gate.CommentKindIssue, "- [x] Skip this commit", "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			comments := &fakeCommentGitHub{canWrite: true}
			svc := gate.NewService(&fakeGitHub{}, &fakeStore{stored: skipBase()}, gate.Runners{}).WithComments(comments)

			if err := svc.HandleComment(t.Context(), tc.event); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			if diff := cmp.Diff([]gate.Reaction{gate.ReactionDone}, comments.reactionsOn(tc.event.Kind, tc.event.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleCommentRerun(t *testing.T) {
	t.Parallel()

	failed := skipBase()
	failed.Run = nil
	failed.HeadSHA = "old111"
	failed.FailureCause = "The analysis timed out."
	tests := []struct {
		name         string
		canWrite     bool
		wantStarts   int
		wantReaction gate.Reaction
	}{
		{name: "writer", canWrite: true, wantStarts: 1, wantReaction: gate.ReactionDone},
		{name: "no write access", wantReaction: gate.ReactionRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{pullRequest: gate.PullRequest{BaseSHA: "base1", HeadSHA: "new222", Open: true}}
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			comments := &fakeCommentGitHub{canWrite: tc.canWrite}
			svc := gate.NewService(gh, &fakeStore{stored: failed}, gate.Runners{Server: runner}).WithComments(comments)
			ev := skipEvent(gate.CommentKindIssue, "- [x] Re-run analysis", "")

			if err := svc.HandleComment(t.Context(), ev); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			if len(runner.calls) != tc.wantStarts {
				t.Errorf("analyses started = %d, want %d", len(runner.calls), tc.wantStarts)
			}
			if diff := cmp.Diff([]gate.Reaction{tc.wantReaction}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleCommentRerunInfraErrorKeepsSeen(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{pullRequestErr: errors.New("boom")}
	comments := &fakeCommentGitHub{canWrite: true}
	svc := gate.NewService(gh, &fakeStore{stored: skipBase()}, gate.Runners{Server: &fakeRunner{}}).WithComments(comments)
	ev := skipEvent(gate.CommentKindIssue, "- [x] Re-run analysis", "")

	if err := svc.HandleComment(t.Context(), ev); err == nil {
		t.Fatal("HandleComment() = nil, want the pull request lookup error")
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionSeen}, comments.reactionsOn(ev.Kind, ev.CommentID)); diff != "" {
		t.Errorf("reactions (-want +got):\n%s", diff)
	}
}

func TestHandleCommentSkipKeepsFailureOnTheSummary(t *testing.T) {
	t.Parallel()

	state := skipBase()
	state.Run = nil
	state.FailureCause = "The analysis timed out."
	gh := &fakeGitHub{}
	gh.addComment(gate.CommentKindIssue, "old summary")
	for len(gh.comments) < int(state.SummaryCommentID) {
		gh.addComment(gate.CommentKindIssue, "filler")
	}
	svc := gate.NewService(gh, &fakeStore{stored: state}, gate.Runners{}).WithComments(skipCommentGitHub{canWrite: true})

	if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "/pollux-agent skip typo fix")); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	body := gh.comments[state.SummaryCommentID-1].Body
	for _, want := range []string{"**Analysis failed:** The analysis timed out.", "- [ ] Re-run analysis", "Skipped by @dev"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary after skip lacks %q:\n%s", want, body)
		}
	}
}
