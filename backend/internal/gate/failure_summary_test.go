package gate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
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
	gh := &gatetest.GitHub{NextCheckRunID: 555}
	store := newStore(t)
	svc := newService(gh, store, gate.Runners{Server: runner}, nil)

	errc := make(chan error, 1)
	go func() { errc <- svc.HandlePullRequest(ctx, testPR()) }()
	<-runner.started
	cancel(errors.New("superseded"))

	if err := <-errc; err == nil {
		t.Fatal("HandlePullRequest() = nil, want the interruption")
	}
	if cr := theCheckRun(t, gh); len(cr.Updates) != 0 || len(gh.Comments()) != 0 {
		t.Errorf("check run = %+v, comments = %+v, want no conclusion and no comment", cr, gh.Comments())
	}
	if saved := loadPR(t, store, 7); saved.Run == nil || saved.CheckRunID != 555 {
		t.Errorf("stored state = %+v, want the check run armed", saved)
	}
}

// Detail from an artifact or the model never reaches the check run or the summary.
func TestHandleRunCompletedInvalidResultHidesDetail(t *testing.T) {
	t.Parallel()

	const injected = "@someone [link](http://x)"
	gh := &gatetest.GitHub{}
	runner := &fakeRunner{collectErr: &review.InvalidResultError{Cause: errors.New(injected)}}
	svc := newService(gh, newStore(t, awaitingState()), gate.Runners{Actions: runner}, nil)

	err := svc.HandleRunCompleted(t.Context(), completedRun("success"))
	if err == nil || !strings.Contains(err.Error(), injected) {
		t.Fatalf("HandleRunCompleted() = %v, want an error carrying the detail for the job log", err)
	}
	cr := theCheckRun(t, gh)
	if len(cr.Updates) != 1 {
		t.Fatalf("check run = %+v, want one update", cr)
	}
	if body := summaryBody(t, gh); strings.Contains(cr.Latest().Summary, injected) || strings.Contains(body, injected) {
		t.Errorf("check = %q, summary = %q, want neither to contain the detail", cr.Latest().Summary, body)
	}
}

func TestHandleRunCompletedUnusableResultWritesFailureSummary(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{}}}
	svc := newService(gh, newStore(t, awaitingState()), gate.Runners{Actions: runner}, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}
	cr := theCheckRun(t, gh)
	if len(cr.Updates) != 1 || cr.Latest().Title != "Analysis failed" {
		t.Fatalf("check run = %+v, want one Analysis failed update", cr)
	}
	cause := cr.Latest().Summary
	if body := summaryBody(t, gh); !strings.Contains(body, cause) || !strings.Contains(body, "- [ ] Re-run analysis\n") {
		t.Errorf("summary = %q, want it stating %q with an unticked Re-run box", body, cause)
	}
}

// An Actions run that fails writes a summary that states the cause and holds an
// unticked Re-run box.
func TestHandleRunCompletedFailureWritesFailureSummary(t *testing.T) {
	t.Parallel()

	for _, conclusion := range []string{"failure", "cancelled", "timed_out"} {
		t.Run(conclusion, func(t *testing.T) {
			t.Parallel()

			gh := &gatetest.GitHub{}
			store := newStore(t, awaitingState())
			svc := newService(gh, store, gate.Runners{Actions: &fakeRunner{}}, nil)

			if err := svc.HandleRunCompleted(t.Context(), completedRun(conclusion)); err != nil {
				t.Fatalf("HandleRunCompleted() = %v, want nil", err)
			}
			cr := theCheckRun(t, gh)
			if len(cr.Updates) != 1 || cr.Latest().Conclusion != gate.ConclusionNeutral {
				t.Fatalf("check run = %+v, want one neutral conclusion", cr)
			}
			cause := cr.Latest().Summary
			if body := summaryBody(t, gh); !strings.Contains(body, cause) || !strings.Contains(body, "- [ ] Re-run analysis\n") {
				t.Errorf("summary = %q, want it stating %q with an unticked Re-run box", body, cause)
			}
		})
	}
}

// Edge: a re-run ticked on an Actions repo whose new run fails rewrites the
// existing summary, keeps earlier proposals listed and unticks the box.
func TestHandleRunCompletedFailureUnticksSummaryAndKeepsProposals(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	review := gh.AddComment(gate.CommentKindReview, "proposal for docs/a.md")
	summary := gh.AddComment(gate.CommentKindIssue, "old summary\n- [x] Re-run analysis\n")
	state := awaitingState()
	state.SummaryCommentID = summary.ID
	state.Proposals = []gate.ProposalState{{
		ID: gate.ProposalID("docs/a.md", "A"), DocPath: "docs/a.md", Section: "A",
		CommentID: review.ID, State: gate.ProposalOpen,
	}}
	store := newStore(t, state)
	svc := newService(gh, store, gate.Runners{Actions: &fakeRunner{}}, nil)

	if err := svc.HandleRunCompleted(t.Context(), completedRun("failure")); err != nil {
		t.Fatalf("HandleRunCompleted() = %v, want nil", err)
	}
	if c := callsOf(gh); c.editIssue != 1 || c.createIssue != 0 {
		t.Fatalf("summary writes = edits %d creates %d, want 1 edit", c.editIssue, c.createIssue)
	}
	body := gh.Comments()[1].Body
	if !strings.Contains(body, "docs/a.md") || !strings.Contains(body, "- [ ] Re-run analysis\n") || strings.Contains(body, "[x]") {
		t.Errorf("summary body = %q, want docs/a.md still listed and an unticked Re-run box", body)
	}
}
