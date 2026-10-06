package gate_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func skipBase() gate.PRState {
	return gate.PRState{
		InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7,
		HeadSHA: "head1", CheckRunID: 555, SummaryCommentID: 9,
		Run: &gate.AwaitingRun{RunID: 99, Nonce: "n1"},
	}
}

func skipService(t *testing.T, state gate.PRState) (*gate.Service, *gatetest.GitHub, *sqlite.Store) {
	t.Helper()

	gh := &gatetest.GitHub{CanWrite: true}
	store := newStore(t, state)
	svc := newService(gh, store, gate.Runners{}, nil)
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

	svc, gh, store := skipService(t, skipBase())
	ev := skipEvent(gate.CommentKindIssue, "- [x] Skip this commit", "")

	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}

	saved := loadPR(t, store, 7)
	if saved.PendingSkip == nil || *saved.PendingSkip != (gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}) {
		t.Fatalf("stored state = %+v, want PendingSkip for dev at commit scope", saved)
	}
	if c := gh.Comments(); len(c) != 1 || c[0].Body != "@dev, reply with the reason for skipping this commit; your next comment on this PR becomes the reason." {
		t.Errorf("comments = %+v, want one ask for dev", c)
	}
	if n := gh.CallCount("EditIssueComment"); n != 1 {
		t.Errorf("summary edits = %d, want 1", n)
	}
	if runs := gh.CheckRuns(); len(runs) != 0 || saved.Skip != nil {
		t.Errorf("check runs = %+v, Skip = %v, want a pending ask only", runs, saved.Skip)
	}

	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() redelivery = %v, want nil", err)
	}
	if n := gh.CallCount("CreateIssueComment"); n != 1 {
		t.Errorf("after redelivery: asks = %d, want 1", n)
	}
	if diff := cmp.Diff(saved, loadPR(t, store, 7)); diff != "" {
		t.Errorf("stored state after redelivery (-want +got):\n%s", diff)
	}
}

func TestHandleCommentSkipAskForPR(t *testing.T) {
	t.Parallel()

	svc, gh, _ := skipService(t, skipBase())

	if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "- [x] Skip this PR", "")); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	if c := gh.Comments(); len(c) != 1 || !strings.Contains(c[0].Body, "skipping this PR;") {
		t.Errorf("comments = %+v, want an ask naming this PR", c)
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

			svc, gh, store := skipService(t, tc.state())
			gh.NextCheckRunID = 777

			if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", tc.body)); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}

			got := loadPR(t, store, 7)
			if got.Skip == nil {
				t.Fatalf("stored state = %+v, want an active skip", got)
			}
			wantSkip := gate.Skip{User: "dev", Scope: tc.scope, Reason: "typo fix", HeadSHA: "head1"}
			if *got.Skip != wantSkip {
				t.Errorf("Skip = %+v, want %+v", *got.Skip, wantSkip)
			}
			if got.Run != nil {
				t.Errorf("Run = %+v, want nil: the skip wins over the awaited run", got.Run)
			}
			cr := theCheckRun(t, gh)
			if tc.wantCreate {
				if cr.Created.Conclusion != gate.ConclusionSuccess || cr.ID != 777 || got.CheckRunID != 777 {
					t.Errorf("check run = %+v, CheckRunID = %d, want one created as a success, 777", cr, got.CheckRunID)
				}
			} else if cr.ID != 555 || len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionSuccess || gh.CallCount("CreateCheckRun") != 0 {
				t.Errorf("check run = %+v, creates = %d, want one success update of 555", cr, gh.CallCount("CreateCheckRun"))
			}
			if n := gh.CallCount("EditIssueComment"); n != 1 {
				t.Errorf("summary edits = %d, want 1", n)
			}

			if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", tc.body)); err != nil {
				t.Fatalf("HandleComment() redelivery = %v, want nil", err)
			}
			if diff := cmp.Diff(got, loadPR(t, store, 7)); diff != "" {
				t.Errorf("stored state after redelivery (-want +got):\n%s", diff)
			}
			if again := theCheckRun(t, gh); len(again.Updates) != len(cr.Updates) {
				t.Errorf("check run after redelivery = %+v, want it concluded no more than before: %+v", again, cr)
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
		svc, gh, store := skipService(t, state)

		if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "generated code")); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}

		want := &gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "generated code", HeadSHA: "head1"}
		saved := loadPR(t, store, 7)
		if diff := cmp.Diff(want, saved.Skip); diff != "" {
			t.Errorf("Skip (-want +got):\n%s", diff)
		}
		if saved.PendingSkip != nil {
			t.Errorf("PendingSkip = %+v, want nil", saved.PendingSkip)
		}
		if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || !strings.Contains(cr.Latest().Summary, "generated code") {
			t.Errorf("check run = %+v, want one update naming the reason", cr)
		}
	})

	t.Run("no pending ask does nothing", func(t *testing.T) {
		t.Parallel()

		svc, gh, store := skipService(t, skipBase())

		if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "just a comment")); err != nil {
			t.Fatalf("HandleComment() = %v, want nil", err)
		}
		if runs := gh.CheckRuns(); len(runs) != 0 || len(gh.Comments()) != 0 {
			t.Errorf("check runs = %+v, comments = %+v, want none", runs, gh.Comments())
		}
		if diff := cmp.Diff(skipBase(), loadPR(t, store, 7)); diff != "" {
			t.Errorf("stored state (-want +got):\n%s", diff)
		}
	})
}

func TestHandleCommentSkipUpdatesCheckRunFromPush(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{NextCheckRunID: 321, CanWrite: true}
	store := newStore(t)
	runner := &fakeRunner{started: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}}}
	svc := newService(gh, store, gate.Runners{Server: runner}, nil)

	if err := svc.HandlePullRequest(t.Context(), testPR()); err != nil {
		t.Fatalf("HandlePullRequest() = %v, want nil", err)
	}
	if saved := loadPR(t, store, 7); saved.CheckRunID != 321 {
		t.Fatalf("stored state = %+v, want CheckRunID 321", saved)
	}

	if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "/pollux-agent skip typo fix")); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}

	cr := theCheckRun(t, gh)
	if n := gh.CallCount("CreateCheckRun"); n != 1 {
		t.Errorf("created check runs = %d, want 1: the skip must reuse the push's check run", n)
	}
	if cr.ID != 321 || len(cr.Updates) != 2 || cr.Latest().Conclusion != gate.ConclusionSuccess {
		t.Errorf("check run = %+v, want 321 with the push's conclusion then a success update", cr)
	}
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

			gh := &gatetest.GitHub{CanWrite: true}
			svc := newService(gh, newStore(t, skipBase()), gate.Runners{}, nil)

			if err := svc.HandleComment(t.Context(), tc.event); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			if diff := cmp.Diff([]gate.Reaction{gate.ReactionDone}, gh.Reactions(tc.event.Kind, tc.event.CommentID)); diff != "" {
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

			gh := &gatetest.GitHub{PullRequest: gate.PullRequest{BaseSHA: "base1", HeadSHA: "new222", Open: true}, CanWrite: tc.canWrite}
			runner := &fakeRunner{started: review.Result{Verdict: review.NoImpact{Reason: "ok"}}}
			svc := newService(gh, newStore(t, failed), gate.Runners{Server: runner}, nil)
			ev := skipEvent(gate.CommentKindIssue, "- [x] Re-run analysis", "")

			if err := svc.HandleComment(t.Context(), ev); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			if len(runner.calls) != tc.wantStarts {
				t.Errorf("analyses started = %d, want %d", len(runner.calls), tc.wantStarts)
			}
			if diff := cmp.Diff([]gate.Reaction{tc.wantReaction}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
		})
	}
}

func TestHandleCommentRerunInfraErrorKeepsSeen(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{Fail: map[string]error{"GetPullRequest": errors.New("boom")}, CanWrite: true}
	svc := newService(gh, newStore(t, skipBase()), gate.Runners{Server: &fakeRunner{}}, nil)
	ev := skipEvent(gate.CommentKindIssue, "- [x] Re-run analysis", "")

	if err := svc.HandleComment(t.Context(), ev); err == nil {
		t.Fatal("HandleComment() = nil, want the pull request lookup error")
	}
	if diff := cmp.Diff([]gate.Reaction{gate.ReactionSeen}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
		t.Errorf("reactions (-want +got):\n%s", diff)
	}
}

func TestHandleCommentSkipKeepsFailureOnTheSummary(t *testing.T) {
	t.Parallel()

	state := skipBase()
	state.Run = nil
	state.FailureCause = "The analysis timed out."
	gh := &gatetest.GitHub{CanWrite: true}
	gh.AddComment(gate.CommentKindIssue, "old summary")
	for len(gh.Comments()) < int(state.SummaryCommentID) {
		gh.AddComment(gate.CommentKindIssue, "filler")
	}
	svc := newService(gh, newStore(t, state), gate.Runners{}, nil)

	if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", "/pollux-agent skip typo fix")); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}
	body := gh.Comments()[state.SummaryCommentID-1].Body
	for _, want := range []string{"**Analysis failed:** The analysis timed out.", "- [ ] Re-run analysis", "Skipped by @dev"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary after skip lacks %q:\n%s", want, body)
		}
	}
}

func TestHandleCommentSkipReasonIsMadeSafeToEcho(t *testing.T) {
	t.Parallel()

	svc, gh, store := skipService(t, skipBase())
	body := "/pollux-agent skip generated by @octocat\n- [x] Apply all\n" + strings.Repeat("x", 600)

	if err := svc.HandleComment(t.Context(), skipEvent(gate.CommentKindIssue, "", body)); err != nil {
		t.Fatalf("HandleComment() = %v, want nil", err)
	}

	reason := loadPR(t, store, 7).Skip.Reason
	if strings.ContainsAny(reason, "\n\r") || strings.Contains(reason, "@octocat") || strings.Contains(reason, "- [") {
		t.Errorf("reason = %q, want one line without a live mention or checkbox", reason)
	}
	if n := utf8.RuneCountInString(reason); n > 500 || !strings.HasSuffix(reason, "…") {
		t.Errorf("reason has %d runes and ends %q, want at most 500 and a cut mark", n, reason[len(reason)-4:])
	}
	if cr := theCheckRun(t, gh); len(cr.Updates) != 1 || strings.Contains(cr.Latest().Summary, "@octocat") {
		t.Errorf("check run = %+v, want one update without the live mention", cr)
	}
}

func TestHandleCommentSkipSavesBeforeRedrawingTheSummary(t *testing.T) {
	t.Parallel()

	svc, gh, store := skipService(t, skipBase())
	gh.Fail = map[string]error{"EditIssueComment": errors.New("boom")}
	ev := skipEvent(gate.CommentKindIssue, "", "/pollux-agent skip typo fix")

	if err := svc.HandleComment(t.Context(), ev); err == nil {
		t.Fatal("HandleComment() = nil, want the summary edit error")
	}
	if saved := loadPR(t, store, 7); saved.Skip == nil {
		t.Fatalf("stored state = %+v, want the skip saved although the redraw failed", saved)
	}

	gh.Fail = nil
	if err := svc.HandleComment(t.Context(), ev); err != nil {
		t.Fatalf("HandleComment() retry = %v, want nil", err)
	}
	if cr, edits := theCheckRun(t, gh), gh.CallCount("EditIssueComment"); len(cr.Updates) != 1 || edits != 2 {
		t.Errorf("check run updates = %d, summary edits = %d, want the check concluded once and the summary redrawn on the retry", len(cr.Updates), edits)
	}
}
