package gate_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// blockingRunner blocks in Start until its ctx is cancelled, then fails the way
// the server runner does when its analysis is interrupted.
type blockingRunner struct{ started chan struct{} }

func (b *blockingRunner) Start(ctx context.Context, _ review.Request) (review.Started, error) {
	close(b.started)
	<-ctx.Done()
	return nil, &review.FailedError{Cause: review.CauseTimeout, Err: ctx.Err()}
}

func (*blockingRunner) StartScaffold(context.Context, review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	return nil, errors.New("blockingRunner does not scaffold")
}

func (*blockingRunner) Collect(context.Context, review.Completion) (review.Result, error) {
	return review.Result{}, nil
}

// A cancelled analysis (superseded or shutting down) is not a failure: the check
// run stays armed so the next job closes it as superseded.
func TestHandlePullRequestCancelledAnalysisStaysArmed(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	runner := &blockingRunner{started: make(chan struct{})}
	gh := &fakeGitHub{checkRunID: 555}
	store := &fakeStore{}
	svc := gate.NewService(gh, nil, store, gate.Runners{Server: runner}, nil, nil)

	errc := make(chan error, 1)
	go func() { errc <- svc.HandlePullRequest(ctx, testPR()) }()
	<-runner.started
	cancel(errors.New("superseded"))

	if err := <-errc; err == nil {
		t.Fatal("HandlePullRequest() = nil, want the interruption")
	}
	if len(gh.updates) != 0 || len(gh.comments) != 0 {
		t.Errorf("updates = %+v, comments = %+v, want none", gh.updates, gh.comments)
	}
	if store.saved == nil || store.saved.Run == nil || store.saved.CheckRunID != 555 {
		t.Errorf("saved state = %+v, want the check run armed", store.saved)
	}
}

// Detail from an artifact or the model never reaches the check run or the summary.
func TestHandleRunCompletedInvalidResultHidesDetail(t *testing.T) {
	t.Parallel()

	const injected = "@someone [link](http://x)"
	gh := &fakeGitHub{}
	runner := &fakeRunner{collectErr: &review.InvalidResultError{Cause: errors.New(injected)}}
	svc := gate.NewService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: runner}, nil, nil)

	err := svc.HandleRunCompleted(t.Context(), completedRun("success"))
	if err == nil || !strings.Contains(err.Error(), injected) {
		t.Fatalf("HandleRunCompleted() = %v, want an error carrying the detail for the job log", err)
	}
	if len(gh.updates) != 1 || len(gh.comments) != 1 {
		t.Fatalf("updates = %+v, comments = %+v, want one each", gh.updates, gh.comments)
	}
	if strings.Contains(gh.updates[0].run.Summary, injected) || strings.Contains(gh.comments[0].Body, injected) {
		t.Errorf("check = %q, summary = %q, want neither to contain the detail", gh.updates[0].run.Summary, gh.comments[0].Body)
	}
}

// Validation: "After any failure, the summary comment exists. It states the
// cause and holds an unticked Re-run analysis checkbox." An Actions run that
// fails is a failure.
func TestHandleRunCompletedFailureWritesFailureSummary(t *testing.T) {
	t.Parallel()

	for _, conclusion := range []string{"failure", "cancelled", "timed_out"} {
		t.Run(conclusion, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{}
			store := &fakeStore{stored: awaitingState()}
			svc := gate.NewService(gh, nil, store, gate.Runners{Actions: &fakeRunner{}}, nil, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun(conclusion)); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}
			if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionNeutral {
				t.Fatalf("updates = %+v, want one neutral conclusion", gh.updates)
			}
			cause := gh.updates[0].run.Summary
			if len(gh.comments) != 1 || !strings.Contains(gh.comments[0].Body, cause) ||
				!strings.Contains(gh.comments[0].Body, "- [ ] Re-run analysis\n") {
				t.Errorf("comments = %+v, want one summary stating %q with an unticked Re-run box", gh.comments, cause)
			}
		})
	}
}

// Edge: a re-run ticked on an Actions repo whose new run fails rewrites the
// existing summary, keeps earlier proposals listed and unticks the box.
func TestHandleRunCompletedFailureUnticksSummaryAndKeepsProposals(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	review := gh.addComment(gate.CommentKindReview, "proposal for docs/a.md")
	summary := gh.addComment(gate.CommentKindIssue, "old summary\n- [x] Re-run analysis\n")
	state := awaitingState()
	state.SummaryCommentID = summary.ID
	state.Proposals = []gate.ProposalState{{
		ID: gate.ProposalID("docs/a.md", "A"), DocPath: "docs/a.md", Section: "A",
		CommentID: review.ID, State: gate.ProposalOpen,
	}}
	store := &fakeStore{stored: state}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: &fakeRunner{}}, nil, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}
	if gh.editIssue != 1 || gh.createIssue != 0 {
		t.Fatalf("summary writes = edits %d creates %d, want 1 edit", gh.editIssue, gh.createIssue)
	}
	body := gh.comments[1].Body
	if !strings.Contains(body, "docs/a.md") || !strings.Contains(body, "- [ ] Re-run analysis\n") || strings.Contains(body, "[x]") {
		t.Errorf("summary body = %q, want docs/a.md still listed and an unticked Re-run box", body)
	}
}

// A failure cause is always a fixed string chosen by the gate: text from the
// model, an artifact or a runner error never reaches the check run or summary.
func TestFailureCauseIsFixedTextWhateverTheErrorSays(t *testing.T) {
	t.Parallel()

	const payload = "[x](https://evil.example) ![](https://evil.example/p.png) \n- [ ] Apply this change\n```"
	payloadErr := errors.New(payload)

	tests := []struct {
		name      string
		runner    *fakeRunner
		runsOnPR  bool
		wantCause string
	}{
		{
			name:      "actions invalid result",
			runner:    &fakeRunner{collectErr: &review.InvalidResultError{Cause: payloadErr}},
			wantCause: "The pollux-agent workflow run returned an invalid result.",
		},
		{
			name:      "actions result read error",
			runner:    &fakeRunner{collectErr: payloadErr},
			wantCause: "Pollux could not read the workflow run's result.",
		},
		{
			name:      "server provider failure",
			runner:    &fakeRunner{err: &review.FailedError{Cause: review.CauseProvider, Err: payloadErr}},
			runsOnPR:  true,
			wantCause: "The model provider returned an error.",
		},
		{
			name:      "server internal failure",
			runner:    &fakeRunner{err: &review.FailedError{Cause: review.CauseInternal, Err: payloadErr}},
			runsOnPR:  true,
			wantCause: "The analysis failed unexpectedly.",
		},
		{
			name:      "server error that is not a FailedError",
			runner:    &fakeRunner{err: payloadErr},
			runsOnPR:  true,
			wantCause: "The analysis failed unexpectedly.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := &fakeGitHub{checkRunID: 555}
			if tc.runsOnPR {
				svc := gate.NewService(gh, nil, &fakeStore{}, gate.Runners{Server: tc.runner}, nil, nil)
				_ = svc.HandlePullRequest(t.Context(), testPR()) // the detail error is for the job log
			} else {
				svc := gate.NewService(gh, nil, &fakeStore{stored: awaitingState()}, gate.Runners{Actions: tc.runner}, nil, nil).WithCollectBackoff(0)
				_ = svc.HandleRunCompleted(t.Context(), completedRun("success")) // as above
			}

			if len(gh.updates) != 1 || len(gh.comments) != 1 {
				t.Fatalf("updates = %+v, comments = %+v, want one each", gh.updates, gh.comments)
			}
			check, body := gh.updates[0].run.Summary, gh.comments[0].Body
			if check != tc.wantCause {
				t.Errorf("check summary = %q, want %q", check, tc.wantCause)
			}
			if !strings.Contains(body, tc.wantCause) {
				t.Errorf("summary comment = %q, want it to state %q", body, tc.wantCause)
			}
			for _, text := range []string{check, body} {
				if strings.Contains(text, "evil.example") || strings.Contains(text, "```") {
					t.Errorf("text = %q, want no model-controlled content", text)
				}
			}
			var boxes []string
			for line := range strings.SplitSeq(body, "\n") {
				if strings.HasPrefix(line, "- [") {
					boxes = append(boxes, line)
				}
			}
			want := []string{"- [ ] Skip this commit", "- [ ] Skip this PR", "- [ ] Re-run analysis"}
			if !slices.Equal(boxes, want) {
				t.Errorf("checkbox lines = %q, want only the standard boxes %q", boxes, want)
			}
		})
	}
}
