// Package httpapi is the webhook transport: it turns GitHub webhooks into durable
// jobs, built by the jobs package.
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobs"
)

const maxWebhookBodyBytes = 25 << 20 // GitHub's webhook payload cap

// Enqueuer accepts a durable job for later processing by a worker.
type Enqueuer interface {
	Enqueue(ctx context.Context, job jobqueue.NewJob) (bool, error)
}

// RunLookup finds stored pull requests: the one an analysis workflow run was
// dispatched for, and those at a head commit.
type RunLookup interface {
	PRForRun(ctx context.Context, owner, repo string, runID int64) (number int, ok bool, err error)
	PRsForHead(ctx context.Context, owner, repo, headSHA string) ([]int, error)
	// ScaffoldForRun reports whether the repo's scaffold awaits the workflow run runID.
	ScaffoldForRun(ctx context.Context, owner, repo string, runID int64) (bool, error)
}

func NewHandler(logger *slog.Logger, webhookSecret []byte, enqueuer Enqueuer, runs RunLookup) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok")); err != nil {
			logger.Warn("write healthz response", "err", err)
		}
	})
	mux.HandleFunc("POST /webhook", webhookHandler(logger, webhookSecret, enqueuer, runs))
	return mux
}

// repoInstallation is the repository and installation every handled webhook
// payload carries.
type repoInstallation struct {
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func (p repoInstallation) complete() bool {
	return p.Repository.Owner.Login != "" && p.Repository.Name != "" && p.Installation.ID != 0
}

// sender is the user behind a comment webhook.
type sender struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// workflowRunEvent is the subset of GitHub's workflow_run webhook payload the
// handler needs.
type workflowRunEvent struct {
	repoInstallation
	Action      string `json:"action"`
	WorkflowRun struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		Conclusion string `json:"conclusion"`
	} `json:"workflow_run"`
}

// pullRequestEvent is the subset of GitHub's pull_request webhook payload the
// handler needs.
type pullRequestEvent struct {
	repoInstallation
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Base struct {
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			SHA  string `json:"sha"`
			Ref  string `json:"ref"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	} `json:"pull_request"`
}

// reviewCommentEvent is the subset of GitHub's pull_request_review_comment
// webhook payload the handler needs.
type reviewCommentEvent struct {
	repoInstallation
	Action  string `json:"action"`
	Changes struct {
		Body struct {
			From string `json:"from"`
		} `json:"body"`
	} `json:"changes"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	PullRequest struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Sender sender `json:"sender"`
}

// issueCommentEvent is the subset of GitHub's issue_comment webhook payload the
// handler needs.
type issueCommentEvent struct {
	repoInstallation
	Action  string `json:"action"`
	Changes struct {
		Body struct {
			From string `json:"from"`
		} `json:"body"`
	} `json:"changes"`
	Comment struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	} `json:"comment"`
	Issue struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Sender sender `json:"sender"`
}

// checkRunEvent is the subset of GitHub's check_run webhook payload the handler
// needs. PullRequests is empty when GitHub cannot associate the run with a PR.
type checkRunEvent struct {
	repoInstallation
	Action   string `json:"action"`
	CheckRun struct {
		Name         string `json:"name"`
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
}

// badRequestError marks a webhook the sender got wrong: undecodable or missing
// required fields. Any other handler error is ours and maps to 500.
type badRequestError struct {
	reason string
	err    error
}

func (e *badRequestError) Error() string {
	if e.err == nil {
		return e.reason
	}
	return e.reason + ": " + e.err.Error()
}

func (e *badRequestError) Unwrap() error { return e.err }

func decodePayload[T any](event string, body []byte) (T, error) {
	var payload T
	if err := json.Unmarshal(body, &payload); err != nil {
		return payload, &badRequestError{reason: "decode " + event + " payload", err: err}
	}
	return payload, nil
}

func missingFields(event string) error {
	return &badRequestError{reason: event + " payload missing fields"}
}

func webhookHandler(logger *slog.Logger, webhookSecret []byte, enqueuer Enqueuer, runs RunLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deliveryID := r.Header.Get("X-GitHub-Delivery")

		r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesErr *http.MaxBytesError
			status := http.StatusBadRequest
			if errors.As(err, &maxBytesErr) {
				status = http.StatusRequestEntityTooLarge
			}
			logger.Warn("read webhook body", "delivery_id", deliveryID, "err", err)
			w.WriteHeader(status)
			return
		}

		if !validSignature(webhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
			logger.Warn("invalid webhook signature", "delivery_id", deliveryID)
			w.WriteHeader(http.StatusUnauthorized)
			if _, err := w.Write([]byte("invalid signature")); err != nil {
				logger.Warn("write webhook response", "delivery_id", deliveryID, "err", err)
			}
			return
		}

		event := r.Header.Get("X-GitHub-Event")
		logger.Info("received webhook", "event", event, "delivery_id", deliveryID)

		ctx := r.Context()
		var newJobs []jobqueue.NewJob
		switch event {
		case "pull_request":
			newJobs, err = handlePullRequestEvent(deliveryID, body)
		case "pull_request_review_comment":
			newJobs, err = handleReviewCommentEvent(deliveryID, body)
		case "issue_comment":
			newJobs, err = handleIssueCommentEvent(deliveryID, body)
		case "check_run":
			newJobs, err = handleCheckRunEvent(ctx, runs, deliveryID, body)
		case "workflow_run":
			newJobs, err = handleWorkflowRunEvent(ctx, runs, deliveryID, body)
		}
		if err != nil {
			var badRequest *badRequestError
			if errors.As(err, &badRequest) {
				logger.Warn("reject webhook", "event", event, "delivery_id", deliveryID, "err", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			logger.Error("handle webhook", "event", event, "delivery_id", deliveryID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		for _, job := range newJobs {
			enqueued, err := enqueuer.Enqueue(ctx, job)
			if err != nil {
				logger.Error("enqueue webhook job", "event", event, "delivery_id", deliveryID, "kind", job.Kind, "key", job.Key, "err", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if !enqueued {
				logger.Info("duplicate webhook delivery", "delivery_id", deliveryID, "job_delivery_id", job.DeliveryID)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// The handle*Event functions return the jobs a webhook enqueues, none when the
// event is ignored, or an error: *badRequestError for a bad payload, any other
// for a failure of ours.

func handlePullRequestEvent(deliveryID string, body []byte) ([]jobqueue.NewJob, error) {
	payload, err := decodePayload[pullRequestEvent]("pull_request", body)
	if err != nil {
		return nil, err
	}

	switch payload.Action {
	case "opened", "synchronize", "reopened":
	default:
		return nil, nil
	}

	if payload.PullRequest.Head.SHA == "" || !payload.complete() || deliveryID == "" {
		return nil, missingFields("pull_request")
	}

	pr := gate.PullRequest{
		InstallationID: payload.Installation.ID,
		Owner:          payload.Repository.Owner.Login,
		Repo:           payload.Repository.Name,
		Number:         payload.Number,
		BaseSHA:        payload.PullRequest.Base.SHA,
		HeadSHA:        payload.PullRequest.Head.SHA,
		HeadRef:        payload.PullRequest.Head.Ref,
		Fork:           payload.PullRequest.Head.Repo == nil || payload.PullRequest.Head.Repo.FullName != payload.Repository.FullName,
	}

	job, err := jobs.PullRequest(deliveryID, pr)
	if err != nil {
		return nil, fmt.Errorf("build pull request job: %w", err)
	}
	return []jobqueue.NewJob{job}, nil
}

func handleReviewCommentEvent(deliveryID string, body []byte) ([]jobqueue.NewJob, error) {
	payload, err := decodePayload[reviewCommentEvent]("pull_request_review_comment", body)
	if err != nil {
		return nil, err
	}

	if payload.Action != "edited" || payload.Sender.Type == "Bot" {
		return nil, nil
	}
	ticked, ok := tickedLine(payload.Changes.Body.From, payload.Comment.Body)
	if !ok {
		return nil, nil
	}

	if payload.Comment.ID == 0 || payload.PullRequest.Number == 0 || payload.Sender.Login == "" ||
		!payload.complete() || deliveryID == "" {
		return nil, missingFields("pull_request_review_comment")
	}

	return commentJobs(deliveryID, gate.CommentEvent{
		InstallationID: payload.Installation.ID,
		Owner:          payload.Repository.Owner.Login,
		Repo:           payload.Repository.Name,
		Number:         payload.PullRequest.Number,
		Sender:         payload.Sender.Login,
		CommentID:      payload.Comment.ID,
		Kind:           gate.CommentKindReview,
		Ticked:         ticked,
		Body:           payload.Comment.Body,
	})
}

func handleIssueCommentEvent(deliveryID string, body []byte) ([]jobqueue.NewJob, error) {
	payload, err := decodePayload[issueCommentEvent]("issue_comment", body)
	if err != nil {
		return nil, err
	}

	if (payload.Action != "created" && payload.Action != "edited") ||
		payload.Issue.PullRequest == nil || payload.Sender.Type == "Bot" {
		return nil, nil
	}
	var ticked string
	if payload.Action == "edited" {
		var ok bool
		if ticked, ok = tickedLine(payload.Changes.Body.From, payload.Comment.Body); !ok {
			return nil, nil
		}
	} else if payload.Comment.Body == "" {
		return nil, nil
	}

	if payload.Comment.ID == 0 || payload.Issue.Number == 0 || payload.Sender.Login == "" ||
		!payload.complete() || deliveryID == "" {
		return nil, missingFields("issue_comment")
	}

	return commentJobs(deliveryID, gate.CommentEvent{
		InstallationID: payload.Installation.ID,
		Owner:          payload.Repository.Owner.Login,
		Repo:           payload.Repository.Name,
		Number:         payload.Issue.Number,
		Sender:         payload.Sender.Login,
		CommentID:      payload.Comment.ID,
		Kind:           gate.CommentKindIssue,
		Ticked:         ticked,
		Body:           payload.Comment.Body,
	})
}

// tickedLine returns the line that went from "- [ ]" to "- [x]" when from became
// now, and false unless that is the only change.
func tickedLine(from, now string) (string, bool) {
	before, after := strings.Split(from, "\n"), strings.Split(now, "\n")
	if len(before) != len(after) {
		return "", false
	}
	var ticked string
	for i := range before {
		if before[i] == after[i] {
			continue
		}
		if ticked != "" || strings.Replace(before[i], "- [ ]", "- [x]", 1) != after[i] {
			return "", false
		}
		ticked = after[i]
	}
	return ticked, ticked != ""
}

func commentJobs(deliveryID string, ev gate.CommentEvent) ([]jobqueue.NewJob, error) {
	job, err := jobs.Comment(deliveryID, ev)
	if err != nil {
		return nil, fmt.Errorf("build comment job: %w", err)
	}
	return []jobqueue.NewJob{job}, nil
}

// handleCheckRunEvent returns a re-run job for each pull request of our check
// run when a user clicks "Re-run" on it.
func handleCheckRunEvent(ctx context.Context, runs RunLookup, deliveryID string, body []byte) ([]jobqueue.NewJob, error) {
	payload, err := decodePayload[checkRunEvent]("check_run", body)
	if err != nil {
		return nil, err
	}

	if payload.Action != "rerequested" || payload.CheckRun.Name != gate.CheckName {
		return nil, nil
	}

	if !payload.complete() || deliveryID == "" {
		return nil, missingFields("check_run")
	}
	owner, repo := payload.Repository.Owner.Login, payload.Repository.Name

	numbers := make([]int, 0, len(payload.CheckRun.PullRequests))
	for _, pr := range payload.CheckRun.PullRequests {
		numbers = append(numbers, pr.Number)
	}
	if len(numbers) == 0 && payload.CheckRun.HeadSHA != "" {
		numbers, err = runs.PRsForHead(ctx, owner, repo, payload.CheckRun.HeadSHA)
		if err != nil {
			return nil, fmt.Errorf("look up pull requests for check run head %s: %w", payload.CheckRun.HeadSHA, err)
		}
	}

	newJobs := make([]jobqueue.NewJob, 0, len(numbers))
	for _, number := range numbers {
		job, err := jobs.Rerun(fmt.Sprintf("%s:%d", deliveryID, number), gate.RerunRequest{
			InstallationID: payload.Installation.ID,
			PRRef:          gate.PRRef{Owner: owner, Repo: repo, Number: number},
		})
		if err != nil {
			return nil, fmt.Errorf("build rerun job: %w", err)
		}
		newJobs = append(newJobs, job)
	}
	return newJobs, nil
}

func handleWorkflowRunEvent(ctx context.Context, runs RunLookup, deliveryID string, body []byte) ([]jobqueue.NewJob, error) {
	payload, err := decodePayload[workflowRunEvent]("workflow_run", body)
	if err != nil {
		return nil, err
	}

	if payload.Action != "completed" || payload.WorkflowRun.Path != gate.WorkflowPath {
		return nil, nil
	}

	if payload.WorkflowRun.ID == 0 || !payload.complete() || deliveryID == "" {
		return nil, missingFields("workflow_run")
	}

	owner, repo := payload.Repository.Owner.Login, payload.Repository.Name
	number, ok, err := runs.PRForRun(ctx, owner, repo, payload.WorkflowRun.ID)
	if err != nil {
		return nil, fmt.Errorf("look up pull request for workflow run %d: %w", payload.WorkflowRun.ID, err)
	}
	rc := gate.RunCompleted{
		InstallationID: payload.Installation.ID,
		Owner:          owner,
		Repo:           repo,
		Number:         number,
		RunID:          payload.WorkflowRun.ID,
		Conclusion:     payload.WorkflowRun.Conclusion,
	}
	newJob := jobs.WorkflowRun
	if !ok {
		scaffold, err := runs.ScaffoldForRun(ctx, owner, repo, payload.WorkflowRun.ID)
		if err != nil {
			return nil, fmt.Errorf("look up scaffold for workflow run %d: %w", payload.WorkflowRun.ID, err)
		}
		if !scaffold {
			return nil, nil
		}
		newJob = jobs.ScaffoldRun
	}

	job, err := newJob(deliveryID, rc)
	if err != nil {
		return nil, err
	}
	return []jobqueue.NewJob{job}, nil
}

func validSignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}

	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := mac.Sum(nil)

	return hmac.Equal(got, want)
}
