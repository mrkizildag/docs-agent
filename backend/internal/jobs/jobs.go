// Package jobs is the durable job plumbing between the webhook transport, the
// job queue, and the gate: job kinds and keys, the payload codecs, the handler
// that decodes a claimed job into a gate call, the scaffold queue adapter, and
// the deadline sweep.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
)

// Enqueuer accepts a durable job for later processing by a worker.
type Enqueuer interface {
	Enqueue(ctx context.Context, job jobqueue.NewJob) (bool, error)
}

// jobKind names the payload a durable job carries; it is stored as jobqueue.Job.Kind.
type jobKind string

const (
	// pullRequestJobKind: a gate.PullRequest. Rows queued before rerun and
	// comment-rerun had their own kinds may instead carry a legacyPullRequestPayload.
	pullRequestJobKind jobKind = "pull_request"
	// rerunJobKind: a gate.RerunRequest.
	rerunJobKind jobKind = "rerun"
	// commentRerunJobKind: a gate.CommentEvent whose tick is a summary Re-run.
	commentRerunJobKind jobKind = "comment_rerun"
	// commentJobKind: a gate.CommentEvent.
	commentJobKind jobKind = "comment"
	// scaffoldJobKind: a gate.RepoRef.
	scaffoldJobKind jobKind = "scaffold"
	// workflowRunJobKind: a gate.RunCompleted.
	workflowRunJobKind jobKind = "workflow_run"
	// scaffoldRunJobKind: the gate.RunCompleted of a scaffold's workflow run, on
	// the repo's scaffold key.
	scaffoldRunJobKind jobKind = "scaffold_run"
	// scaffoldDeadlineJobKind: a gate.OverdueRun of a scaffold, on the repo's
	// scaffold key.
	scaffoldDeadlineJobKind jobKind = "scaffold_deadline"
	// runDeadlineJobKind: a gate.OverdueRun.
	runDeadlineJobKind jobKind = "run_deadline"
)

// pullRequestGroup is the supersede group of every job that analyzes a pull
// request's head: a push supersedes a queued or running re-run too.
const pullRequestGroup = "pull_request"

// legacyPullRequestPayload is the payload shape of pull_request jobs queued
// before rerun and comment-rerun had their own kinds: a gate.PullRequest, or
// when Rerun is set a re-run request, or when Comment is set a Re-run tick.
type legacyPullRequestPayload struct {
	gate.PullRequest
	Rerun   *gate.RerunRequest `json:",omitempty"`
	Comment *gate.CommentEvent `json:",omitempty"`
}

// prJobKey is the per-PR queue key that serializes a pull request's jobs.
func prJobKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", owner, repo, number)
}

// scaffoldJobKey is the per-repo queue key that serializes a repo's scaffold jobs.
func scaffoldJobKey(ref gate.RepoRef) string {
	return fmt.Sprintf("%s/%s#scaffold", ref.Owner, ref.Repo)
}

func newJob(deliveryID, key string, kind jobKind, payload any) (jobqueue.NewJob, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return jobqueue.NewJob{}, fmt.Errorf("encode %s job payload for %s: %w", kind, key, err)
	}
	return jobqueue.NewJob{DeliveryID: deliveryID, Key: key, Kind: string(kind), Payload: encoded}, nil
}

// PullRequest returns the job that analyzes pr. It supersedes older analysis
// and re-run jobs of the same pull request.
func PullRequest(deliveryID string, pr gate.PullRequest) (jobqueue.NewJob, error) {
	job, err := newJob(deliveryID, prJobKey(pr.Owner, pr.Repo, pr.Number), pullRequestJobKind, pr)
	job.Group, job.Supersedes = pullRequestGroup, true
	return job, err
}

// Rerun returns the job that re-analyzes req's pull request. It queues behind
// a running analysis instead of superseding it: HandleRerun skips a head whose
// analysis is still armed, so cancelling that analysis would leave no analysis.
// A later push supersedes it.
func Rerun(deliveryID string, req gate.RerunRequest) (jobqueue.NewJob, error) {
	job, err := newJob(deliveryID, prJobKey(req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number), rerunJobKind, req)
	job.Group = pullRequestGroup
	return job, err
}

// Comment returns the job for a comment event. A Re-run tick is queued like
// Rerun, so a push supersedes it; any other comment is not superseded.
func Comment(deliveryID string, ev gate.CommentEvent) (jobqueue.NewJob, error) {
	key := prJobKey(ev.Owner, ev.Repo, ev.Number)
	if ev.Ticked != "" && gate.IsRerunTick(ev.Ticked) {
		job, err := newJob(deliveryID, key, commentRerunJobKind, ev)
		job.Group = pullRequestGroup
		return job, err
	}
	return newJob(deliveryID, key, commentJobKind, ev)
}

// WorkflowRun returns the job for the completed analysis workflow run of a pull request.
func WorkflowRun(deliveryID string, rc gate.RunCompleted) (jobqueue.NewJob, error) {
	return newJob(deliveryID, prJobKey(rc.Owner, rc.Repo, rc.Number), workflowRunJobKind, rc)
}

// ScaffoldRun returns the job for the completed workflow run of a repo's
// scaffold; rc.Number is ignored.
func ScaffoldRun(deliveryID string, rc gate.RunCompleted) (jobqueue.NewJob, error) {
	rc.Number = 0
	return newJob(deliveryID, scaffoldJobKey(gate.RepoRef{Owner: rc.Owner, Repo: rc.Repo}), scaffoldRunJobKind, rc)
}

// ScaffoldQueue implements gate.ScaffoldQueue over an Enqueuer. Its jobs never
// supersede.
type ScaffoldQueue struct {
	jobs Enqueuer
}

var _ gate.ScaffoldQueue = (*ScaffoldQueue)(nil)

// NewScaffoldQueue returns a ScaffoldQueue that enqueues into jobs.
func NewScaffoldQueue(jobs Enqueuer) *ScaffoldQueue {
	return &ScaffoldQueue{jobs: jobs}
}

// EnqueueScaffold enqueues the scaffold job of ref's repo for attempt. A job
// for an attempt already enqueued is a no-op.
func (q *ScaffoldQueue) EnqueueScaffold(ctx context.Context, ref gate.RepoRef, attempt int) error {
	job, err := newJob(fmt.Sprintf("scaffold:%s/%s:%d", ref.Owner, ref.Repo, attempt), scaffoldJobKey(ref), scaffoldJobKind, ref)
	if err != nil {
		return err
	}
	if _, err := q.jobs.Enqueue(ctx, job); err != nil {
		return fmt.Errorf("enqueue scaffold job for %s/%s: %w", ref.Owner, ref.Repo, err)
	}
	return nil
}
