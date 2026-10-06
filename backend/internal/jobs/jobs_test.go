package jobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobs"
)

const (
	pullRequestJobKind  = "pull_request"
	rerunJobKind        = "rerun"
	commentRerunJobKind = "comment_rerun"
	workflowRunJobKind  = "workflow_run"
	commentJobKind      = "comment"
)

type fakeEnqueuer struct {
	jobs   []jobqueue.NewJob
	result bool
	err    error
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, job jobqueue.NewJob) (bool, error) {
	f.jobs = append(f.jobs, job)
	if f.err != nil {
		return false, f.err
	}
	return f.result, nil
}

func newFakeEnqueuer() *fakeEnqueuer {
	return &fakeEnqueuer{result: true}
}

type fakePullRequestHandler struct {
	calls    []gate.PullRequest
	comments []gate.CommentEvent
	reruns   []gate.RerunRequest
	runCalls []gate.RunCompleted
	err      error
}

func (f *fakePullRequestHandler) HandleDeadline(context.Context, gate.PRRef, string, time.Time) error {
	return f.err
}

func (f *fakePullRequestHandler) HandleScaffoldRun(context.Context, gate.RunCompleted) error {
	return nil
}

func (f *fakePullRequestHandler) HandleScaffoldDeadline(context.Context, gate.RepoRef, string, time.Time) error {
	return nil
}

func (f *fakePullRequestHandler) HandleScaffold(context.Context, gate.RepoRef) error {
	return f.err
}

func (f *fakePullRequestHandler) HandleComment(_ context.Context, ev gate.CommentEvent) error {
	f.comments = append(f.comments, ev)
	return f.err
}

func (f *fakePullRequestHandler) HandleRerun(_ context.Context, r gate.RerunRequest) error {
	f.reruns = append(f.reruns, r)
	return f.err
}

func (f *fakePullRequestHandler) HandleRunCompleted(_ context.Context, rc gate.RunCompleted) error {
	f.runCalls = append(f.runCalls, rc)
	return f.err
}

func (f *fakePullRequestHandler) HandlePullRequest(_ context.Context, pr gate.PullRequest) error {
	f.calls = append(f.calls, pr)
	return f.err
}

func TestHandleJob(t *testing.T) {
	t.Parallel()

	pr := gate.PullRequest{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}
	payload, err := json.Marshal(pr)
	if err != nil {
		t.Fatalf("marshal pull request: %v", err)
	}

	t.Run("dispatches by kind", func(t *testing.T) {
		t.Parallel()

		handler := &fakePullRequestHandler{}
		job := jobqueue.Job{ID: 1, Key: "acme/widgets#7", Kind: pullRequestJobKind, Payload: payload}

		if err := jobs.HandleJob(handler)(t.Context(), job); err != nil {
			t.Fatalf("HandleJob() error = %v", err)
		}

		if diff := cmp.Diff([]gate.PullRequest{pr}, handler.calls); diff != "" {
			t.Errorf("HandlePullRequest calls (-want +got):\n%s", diff)
		}
	})

	t.Run("dispatches workflow runs", func(t *testing.T) {
		t.Parallel()

		rc := gate.RunCompleted{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, RunID: 99, Conclusion: "success"}
		rcPayload, err := json.Marshal(rc)
		if err != nil {
			t.Fatalf("marshal run completed: %v", err)
		}
		handler := &fakePullRequestHandler{}
		job := jobqueue.Job{ID: 4, Key: "acme/widgets#7", Kind: workflowRunJobKind, Payload: rcPayload}

		if err := jobs.HandleJob(handler)(t.Context(), job); err != nil {
			t.Fatalf("HandleJob() error = %v", err)
		}

		if diff := cmp.Diff([]gate.RunCompleted{rc}, handler.runCalls); diff != "" {
			t.Errorf("HandleRunCompleted calls (-want +got):\n%s", diff)
		}
	})

	t.Run("dispatches comments", func(t *testing.T) {
		t.Parallel()

		ev := gate.CommentEvent{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, Sender: "dev", CommentID: 5, Kind: gate.CommentKindReview, Ticked: "- [x] Apply this change"}
		evPayload, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal comment event: %v", err)
		}
		handler := &fakePullRequestHandler{}
		job := jobqueue.Job{ID: 5, Key: "acme/widgets#7", Kind: commentJobKind, Payload: evPayload}

		if err := jobs.HandleJob(handler)(t.Context(), job); err != nil {
			t.Fatalf("HandleJob() error = %v", err)
		}

		if diff := cmp.Diff([]gate.CommentEvent{ev}, handler.comments); diff != "" {
			t.Errorf("HandleComment calls (-want +got):\n%s", diff)
		}
	})

	t.Run("dispatches rerun ticks as comments", func(t *testing.T) {
		t.Parallel()

		ev := gate.CommentEvent{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, Sender: "dev", CommentID: 5, Kind: gate.CommentKindIssue, Ticked: "- [x] Re-run analysis"}
		evPayload, err := json.Marshal(map[string]any{"Comment": ev})
		if err != nil {
			t.Fatalf("marshal rerun comment job: %v", err)
		}
		handler := &fakePullRequestHandler{}
		job := jobqueue.Job{ID: 6, Key: "acme/widgets#7", Kind: pullRequestJobKind, Payload: evPayload}

		if err := jobs.HandleJob(handler)(t.Context(), job); err != nil {
			t.Fatalf("HandleJob() error = %v", err)
		}

		if diff := cmp.Diff([]gate.CommentEvent{ev}, handler.comments); diff != "" {
			t.Errorf("HandleComment calls (-want +got):\n%s", diff)
		}
		if len(handler.calls) != 0 {
			t.Errorf("HandlePullRequest calls = %v, want none", handler.calls)
		}
	})

	t.Run("unknown kind errors", func(t *testing.T) {
		t.Parallel()

		handler := &fakePullRequestHandler{}
		job := jobqueue.Job{ID: 2, Kind: "unknown", Payload: payload}

		if err := jobs.HandleJob(handler)(t.Context(), job); err == nil {
			t.Fatal("HandleJob() error = nil, want error")
		}
		if len(handler.calls) != 0 {
			t.Errorf("HandlePullRequest calls = %v, want none", handler.calls)
		}
	})

	t.Run("handler error is returned", func(t *testing.T) {
		t.Parallel()

		wantErr := errors.New("boom")
		handler := &fakePullRequestHandler{err: wantErr}
		job := jobqueue.Job{ID: 3, Kind: pullRequestJobKind, Payload: payload}

		err := jobs.HandleJob(handler)(t.Context(), job)
		if !errors.Is(err, wantErr) {
			t.Errorf("HandleJob() error = %v, want wrapping %v", err, wantErr)
		}
	})
}

func TestEnqueueDeadlineJobsKeysAScaffoldByRepo(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	run := gate.OverdueRun{PRRef: gate.PRRef{Owner: "acme", Repo: "widgets"}, Scaffold: true, Nonce: "n1", Deadline: deadline}
	enqueuer := newFakeEnqueuer()
	if err := jobs.EnqueueDeadlineJobs(t.Context(), fakeOverdueSource{run}, enqueuer, slog.New(slog.DiscardHandler), deadline.Add(time.Second)); err != nil {
		t.Fatalf("EnqueueDeadlineJobs() = %v, want nil", err)
	}

	wantPayload, err := json.Marshal(run)
	if err != nil {
		t.Fatalf("marshal want payload: %v", err)
	}
	want := []jobqueue.NewJob{{DeliveryID: "deadline:n1:0", Key: "acme/widgets#scaffold", Kind: "scaffold_deadline", Payload: wantPayload}}
	if diff := cmp.Diff(want, enqueuer.jobs); diff != "" {
		t.Errorf("Enqueue calls (-want +got):\n%s", diff)
	}
}

type fakeOverdueSource []gate.OverdueRun

func (f fakeOverdueSource) OverdueRuns(context.Context, time.Time) ([]gate.OverdueRun, error) {
	return f, nil
}

func TestEnqueueDeadlineJobsBacksOffExponentiallyAndStopsAfterADay(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	src := fakeOverdueSource{{PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}, Nonce: "n1", Deadline: deadline}}
	enqueuer := newFakeEnqueuer()
	enqueuer.result = true

	var retriedAt []int
	seen := map[string]bool{}
	for minute := range 24*60 + 10 {
		before := len(enqueuer.jobs)
		now := deadline.Add(time.Duration(minute)*time.Minute + time.Second)
		if err := jobs.EnqueueDeadlineJobs(t.Context(), src, enqueuer, slog.New(slog.DiscardHandler), now); err != nil {
			t.Fatalf("EnqueueDeadlineJobs(+%dm) = %v", minute, err)
		}
		if minute > 24*60 && len(enqueuer.jobs) != before {
			t.Errorf("job enqueued %dm past the deadline, want none after 24h", minute)
		}
		for _, job := range enqueuer.jobs[before:] {
			if !seen[job.DeliveryID] {
				seen[job.DeliveryID] = true
				retriedAt = append(retriedAt, minute)
			}
		}
	}

	want := []int{0, 1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 1264}
	if diff := cmp.Diff(want, retriedAt); diff != "" {
		t.Errorf("minutes overdue at which a new job is enqueued (-want +got):\n%s", diff)
	}
}

func TestEnqueueDeadlineJobsLogsGivingUpOnce(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	src := fakeOverdueSource{{PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}, Nonce: "n1", Deadline: deadline}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	for now := deadline.Add(24*time.Hour - time.Minute + 10*time.Second); now.Before(deadline.Add(24*time.Hour + 3*time.Minute)); now = now.Add(30 * time.Second) {
		if err := jobs.EnqueueDeadlineJobs(t.Context(), src, newFakeEnqueuer(), logger, now); err != nil {
			t.Fatalf("EnqueueDeadlineJobs(%v) = %v", now, err)
		}
	}
	if got := strings.Count(logs.String(), "giving up on overdue run"); got != 1 {
		t.Errorf("give-up warnings = %d, want 1 across sweeps every 30s:\n%s", got, logs.String())
	}
}

func TestEnqueueDeadlineJobsNamesScaffoldRunsByScaffoldKey(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	src := fakeOverdueSource{{PRRef: gate.PRRef{Owner: "acme", Repo: "widgets"}, Scaffold: true, Nonce: "n1", Deadline: deadline}}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	if err := jobs.EnqueueDeadlineJobs(t.Context(), src, newFakeEnqueuer(), logger, deadline.Add(24*time.Hour+time.Second)); err != nil {
		t.Fatalf("EnqueueDeadlineJobs() = %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "acme/widgets#scaffold") || strings.Contains(got, "#0") {
		t.Errorf("give-up log = %q, want the scaffold key acme/widgets#scaffold and no #0", got)
	}
}

func TestHandleJobDispatchesRerunKinds(t *testing.T) {
	t.Parallel()

	req := gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}}
	ev := gate.CommentEvent{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, Sender: "dev", CommentID: 5, Kind: gate.CommentKindIssue, Ticked: "- [x] Re-run analysis"}

	t.Run("rerun", func(t *testing.T) {
		t.Parallel()

		job, err := jobs.Rerun("d1", req)
		if err != nil {
			t.Fatalf("Rerun() = %v", err)
		}
		if job.Kind != rerunJobKind || job.Group != "pull_request" || job.Supersedes {
			t.Errorf("Rerun() = %+v, want kind rerun, group pull_request, no supersede", job)
		}
		handler := &fakePullRequestHandler{}
		if err := jobs.HandleJob(handler)(t.Context(), jobqueue.Job{ID: 1, Key: job.Key, Kind: job.Kind, Payload: job.Payload}); err != nil {
			t.Fatalf("HandleJob() = %v", err)
		}
		if diff := cmp.Diff([]gate.RerunRequest{req}, handler.reruns); diff != "" {
			t.Errorf("HandleRerun calls (-want +got):\n%s", diff)
		}
	})

	t.Run("comment rerun", func(t *testing.T) {
		t.Parallel()

		job, err := jobs.Comment("d1", ev)
		if err != nil {
			t.Fatalf("Comment() = %v", err)
		}
		if job.Kind != commentRerunJobKind || job.Group != "pull_request" || job.Supersedes {
			t.Errorf("Comment() = %+v, want kind comment_rerun, group pull_request, no supersede", job)
		}
		handler := &fakePullRequestHandler{}
		if err := jobs.HandleJob(handler)(t.Context(), jobqueue.Job{ID: 1, Key: job.Key, Kind: job.Kind, Payload: job.Payload}); err != nil {
			t.Fatalf("HandleJob() = %v", err)
		}
		if diff := cmp.Diff([]gate.CommentEvent{ev}, handler.comments); diff != "" {
			t.Errorf("HandleComment calls (-want +got):\n%s", diff)
		}
	})
}

func TestHandleJobDecodesPullRequestJobsQueuedBeforeTheKindSplit(t *testing.T) {
	t.Parallel()

	req := gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 7}}
	ev := gate.CommentEvent{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, Sender: "dev", CommentID: 5, Kind: gate.CommentKindIssue, Ticked: "- [x] Re-run analysis"}
	pr := gate.PullRequest{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"}
	encode := func(v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %T: %v", v, err)
		}
		return b
	}

	handler := &fakePullRequestHandler{}
	for i, payload := range [][]byte{
		encode(map[string]any{"Rerun": req}),
		encode(map[string]any{"Comment": ev}),
		encode(pr),
	} {
		job := jobqueue.Job{ID: int64(i + 1), Key: "acme/widgets#7", Kind: pullRequestJobKind, Payload: payload}
		if err := jobs.HandleJob(handler)(t.Context(), job); err != nil {
			t.Fatalf("HandleJob(%s) = %v", payload, err)
		}
	}
	if diff := cmp.Diff([]gate.RerunRequest{req}, handler.reruns); diff != "" {
		t.Errorf("HandleRerun calls (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]gate.CommentEvent{ev}, handler.comments); diff != "" {
		t.Errorf("HandleComment calls (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]gate.PullRequest{pr}, handler.calls); diff != "" {
		t.Errorf("HandlePullRequest calls (-want +got):\n%s", diff)
	}
}

func TestScaffoldQueueEnqueuesTheScaffoldJobOnTheRepoKey(t *testing.T) {
	t.Parallel()

	enqueuer := newFakeEnqueuer()
	ref := gate.RepoRef{Owner: "acme", Repo: "widgets"}
	if err := jobs.NewScaffoldQueue(enqueuer).EnqueueScaffold(t.Context(), ref, 2); err != nil {
		t.Fatalf("EnqueueScaffold() = %v", err)
	}

	wantPayload, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("marshal want payload: %v", err)
	}
	want := []jobqueue.NewJob{{DeliveryID: "scaffold:acme/widgets:2", Key: "acme/widgets#scaffold", Kind: "scaffold", Payload: wantPayload}}
	if diff := cmp.Diff(want, enqueuer.jobs); diff != "" {
		t.Errorf("Enqueue calls (-want +got):\n%s", diff)
	}
}
