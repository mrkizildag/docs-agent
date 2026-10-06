// Package httpapi is the HTTP transport: routing, decoding, and error-to-status mapping.
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

func NewHandler(logger *slog.Logger, webhookSecret []byte, jobs Enqueuer, runs RunLookup) *http.ServeMux {
	return NewHandlerWithWebhookRateLimit(logger, webhookSecret, jobs, runs, DefaultWebhookRateLimitConfig())
}

// NewHandlerWithWebhookRateLimit is like NewHandler but accepts custom webhook rate
// limits (used in tests).
func NewHandlerWithWebhookRateLimit(logger *slog.Logger, webhookSecret []byte, jobs Enqueuer, runs RunLookup, rateLimit WebhookRateLimitConfig) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok")); err != nil {
			logger.Warn("write healthz response", "err", err)
		}
	})
	lim := newWebhookRateLimiter(rateLimit)
	mux.HandleFunc("POST /webhook", withWebhookRateLimit(logger, lim, webhookHandler(logger, webhookSecret, jobs, runs)))
	return mux
}

// workflowRunEvent is the subset of GitHub's workflow_run webhook payload the
// handler needs.
type workflowRunEvent struct {
	Action      string `json:"action"`
	WorkflowRun struct {
		ID         int64  `json:"id"`
		Path       string `json:"path"`
		Conclusion string `json:"conclusion"`
	} `json:"workflow_run"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// pullRequestEvent is the subset of GitHub's pull_request webhook payload the
// handler needs.
type pullRequestEvent struct {
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

// reviewCommentEvent is the subset of GitHub's pull_request_review_comment
// webhook payload the handler needs.
type reviewCommentEvent struct {
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
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"sender"`
}

// issueCommentEvent is the subset of GitHub's issue_comment webhook payload the
// handler needs.
type issueCommentEvent struct {
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
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"sender"`
}

// checkRunEvent is the subset of GitHub's check_run webhook payload the handler
// needs. PullRequests is empty when GitHub cannot associate the run with a PR.
type checkRunEvent struct {
	Action   string `json:"action"`
	CheckRun struct {
		Name         string `json:"name"`
		HeadSHA      string `json:"head_sha"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_run"`
	Repository struct {
		Name  string `json:"name"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func webhookHandler(logger *slog.Logger, webhookSecret []byte, jobs Enqueuer, runs RunLookup) http.HandlerFunc {
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

		switch event {
		case "pull_request":
			handlePullRequestEvent(logger, jobs, w, r, deliveryID, body)
		case "pull_request_review_comment":
			handleReviewCommentEvent(logger, jobs, w, r, deliveryID, body)
		case "issue_comment":
			handleIssueCommentEvent(logger, jobs, w, r, deliveryID, body)
		case "check_run":
			handleCheckRunEvent(logger, jobs, runs, w, r, deliveryID, body)
		case "workflow_run":
			handleWorkflowRunEvent(logger, jobs, runs, w, r, deliveryID, body)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}
}

func handlePullRequestEvent(logger *slog.Logger, jobs Enqueuer, w http.ResponseWriter, r *http.Request, deliveryID string, body []byte) {
	var payload pullRequestEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Warn("decode pull_request payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	switch payload.Action {
	case "opened", "synchronize", "reopened":
	default:
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if payload.PullRequest.Head.SHA == "" || payload.Repository.Owner.Login == "" ||
		payload.Repository.Name == "" || payload.Installation.ID == 0 {
		logger.Warn("pull_request payload missing fields", "delivery_id", deliveryID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if deliveryID == "" {
		logger.Warn("pull_request event missing delivery id", "delivery_id", deliveryID)
		w.WriteHeader(http.StatusBadRequest)
		return
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

	jobPayload, err := json.Marshal(pullRequestJobPayload{PullRequest: pr})
	if err != nil {
		logger.Error("encode pull_request job payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	job := jobqueue.NewJob{
		DeliveryID: deliveryID,
		Key:        prJobKey(pr.Owner, pr.Repo, pr.Number),
		Kind:       pullRequestJobKind,
		Payload:    jobPayload,
		Supersedes: true,
	}

	enqueued, err := jobs.Enqueue(r.Context(), job)
	if err != nil {
		logger.Error("enqueue pull_request job", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !enqueued {
		logger.Info("duplicate webhook delivery", "delivery_id", deliveryID)
	}

	w.WriteHeader(http.StatusAccepted)
}

func handleReviewCommentEvent(logger *slog.Logger, jobs Enqueuer, w http.ResponseWriter, r *http.Request, deliveryID string, body []byte) {
	var payload reviewCommentEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Warn("decode pull_request_review_comment payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if payload.Action != "edited" || payload.Sender.Type == "Bot" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	ticked, ok := tickedLine(payload.Changes.Body.From, payload.Comment.Body)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if payload.Comment.ID == 0 || payload.PullRequest.Number == 0 || payload.Sender.Login == "" ||
		payload.Repository.Owner.Login == "" || payload.Repository.Name == "" || payload.Installation.ID == 0 || deliveryID == "" {
		logger.Warn("pull_request_review_comment payload missing fields", "delivery_id", deliveryID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ev := gate.CommentEvent{
		InstallationID: payload.Installation.ID,
		Owner:          payload.Repository.Owner.Login,
		Repo:           payload.Repository.Name,
		Number:         payload.PullRequest.Number,
		Sender:         payload.Sender.Login,
		CommentID:      payload.Comment.ID,
		Kind:           gate.CommentKindReview,
		Ticked:         ticked,
		Body:           payload.Comment.Body,
	}
	enqueued, err := enqueueComment(r.Context(), jobs, deliveryID, ev)
	if err != nil {
		logger.Error("enqueue comment job", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !enqueued {
		logger.Info("duplicate webhook delivery", "delivery_id", deliveryID)
	}

	w.WriteHeader(http.StatusAccepted)
}

func handleIssueCommentEvent(logger *slog.Logger, jobs Enqueuer, w http.ResponseWriter, r *http.Request, deliveryID string, body []byte) {
	var payload issueCommentEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Warn("decode issue_comment payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if (payload.Action != "created" && payload.Action != "edited") ||
		payload.Issue.PullRequest == nil || payload.Sender.Type == "Bot" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var ticked string
	if payload.Action == "edited" {
		var ok bool
		if ticked, ok = tickedLine(payload.Changes.Body.From, payload.Comment.Body); !ok {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	} else if payload.Comment.Body == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if payload.Comment.ID == 0 || payload.Issue.Number == 0 || payload.Sender.Login == "" ||
		payload.Repository.Owner.Login == "" || payload.Repository.Name == "" || payload.Installation.ID == 0 || deliveryID == "" {
		logger.Warn("issue_comment payload missing fields", "delivery_id", deliveryID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ev := gate.CommentEvent{
		InstallationID: payload.Installation.ID,
		Owner:          payload.Repository.Owner.Login,
		Repo:           payload.Repository.Name,
		Number:         payload.Issue.Number,
		Sender:         payload.Sender.Login,
		CommentID:      payload.Comment.ID,
		Kind:           gate.CommentKindIssue,
		Ticked:         ticked,
		Body:           payload.Comment.Body,
	}
	enqueued, err := enqueueComment(r.Context(), jobs, deliveryID, ev)
	if err != nil {
		logger.Error("enqueue comment job", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !enqueued {
		logger.Info("duplicate webhook delivery", "delivery_id", deliveryID)
	}

	w.WriteHeader(http.StatusAccepted)
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

// enqueueComment enqueues the job for a comment event. A Re-run tick runs as a
// pull request job that a push supersedes, queued behind a running analysis
// like enqueueRerun; any other comment is a comment job. It reports whether the
// delivery was new.
func enqueueComment(ctx context.Context, jobs Enqueuer, deliveryID string, ev gate.CommentEvent) (bool, error) {
	kind, supersedes := commentJobKind, false
	var payload any = ev
	if ev.Ticked != "" && gate.IsRerunTick(ev.Ticked) {
		kind, payload = pullRequestJobKind, pullRequestJobPayload{Comment: &ev}
	}
	jobPayload, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("encode comment job payload: %w", err)
	}
	enqueued, err := jobs.Enqueue(ctx, jobqueue.NewJob{
		DeliveryID: deliveryID,
		Key:        prJobKey(ev.Owner, ev.Repo, ev.Number),
		Kind:       kind,
		Payload:    jobPayload,
		Supersedes: supersedes,
	})
	if err != nil {
		return false, fmt.Errorf("enqueue comment job for %s/%s#%d: %w", ev.Owner, ev.Repo, ev.Number, err)
	}
	return enqueued, nil
}

// enqueueRerun enqueues a re-run job for req's pull request. It queues behind a
// running analysis instead of superseding it: HandleRerun skips a head whose
// analysis is still armed, so cancelling that analysis would leave no analysis.
func enqueueRerun(ctx context.Context, jobs Enqueuer, deliveryID string, req gate.RerunRequest) error {
	jobPayload, err := json.Marshal(pullRequestJobPayload{Rerun: &req})
	if err != nil {
		return fmt.Errorf("encode rerun job payload: %w", err)
	}
	if _, err := jobs.Enqueue(ctx, jobqueue.NewJob{
		DeliveryID: deliveryID,
		Key:        prJobKey(req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number),
		Kind:       pullRequestJobKind,
		Payload:    jobPayload,
		Supersedes: false,
	}); err != nil {
		return fmt.Errorf("enqueue rerun job for %s/%s#%d: %w", req.PRRef.Owner, req.PRRef.Repo, req.PRRef.Number, err)
	}
	return nil
}

// handleCheckRunEvent enqueues a re-run for each pull request of our check run
// when a user clicks "Re-run" on it.
func handleCheckRunEvent(logger *slog.Logger, jobs Enqueuer, runs RunLookup, w http.ResponseWriter, r *http.Request, deliveryID string, body []byte) {
	var payload checkRunEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Warn("decode check_run payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if payload.Action != "rerequested" || payload.CheckRun.Name != gate.CheckName {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	owner, repo := payload.Repository.Owner.Login, payload.Repository.Name
	if owner == "" || repo == "" || payload.Installation.ID == 0 || deliveryID == "" {
		logger.Warn("check_run payload missing fields", "delivery_id", deliveryID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	numbers := make([]int, 0, len(payload.CheckRun.PullRequests))
	for _, pr := range payload.CheckRun.PullRequests {
		numbers = append(numbers, pr.Number)
	}
	if len(numbers) == 0 && payload.CheckRun.HeadSHA != "" {
		var err error
		numbers, err = runs.PRsForHead(r.Context(), owner, repo, payload.CheckRun.HeadSHA)
		if err != nil {
			logger.Error("look up pull requests for check run head", "delivery_id", deliveryID, "head_sha", payload.CheckRun.HeadSHA, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	for _, number := range numbers {
		if err := enqueueRerun(r.Context(), jobs, fmt.Sprintf("%s:%d", deliveryID, number), gate.RerunRequest{
			InstallationID: payload.Installation.ID,
			PRRef:          gate.PRRef{Owner: owner, Repo: repo, Number: number},
		}); err != nil {
			logger.Error("enqueue rerun job", "delivery_id", deliveryID, "number", number, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusAccepted)
}

func handleWorkflowRunEvent(logger *slog.Logger, jobs Enqueuer, runs RunLookup, w http.ResponseWriter, r *http.Request, deliveryID string, body []byte) {
	var payload workflowRunEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Warn("decode workflow_run payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if payload.Action != "completed" || payload.WorkflowRun.Path != gate.WorkflowPath {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if payload.WorkflowRun.ID == 0 || payload.Repository.Owner.Login == "" ||
		payload.Repository.Name == "" || payload.Installation.ID == 0 || deliveryID == "" {
		logger.Warn("workflow_run payload missing fields", "delivery_id", deliveryID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	owner, repo := payload.Repository.Owner.Login, payload.Repository.Name
	number, ok, err := runs.PRForRun(r.Context(), owner, repo, payload.WorkflowRun.ID)
	if err != nil {
		logger.Error("look up pull request for workflow run", "delivery_id", deliveryID, "run_id", payload.WorkflowRun.ID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	key, kind := prJobKey(owner, repo, number), workflowRunJobKind
	if !ok {
		scaffold, err := runs.ScaffoldForRun(r.Context(), owner, repo, payload.WorkflowRun.ID)
		if err != nil {
			logger.Error("look up scaffold for workflow run", "delivery_id", deliveryID, "run_id", payload.WorkflowRun.ID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !scaffold {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		key, kind, number = scaffoldJobKey(gate.RepoRef{Owner: owner, Repo: repo}), scaffoldRunJobKind, 0
	}

	completed := gate.RunCompleted{
		InstallationID: payload.Installation.ID,
		Owner:          owner,
		Repo:           repo,
		Number:         number,
		RunID:          payload.WorkflowRun.ID,
		Conclusion:     payload.WorkflowRun.Conclusion,
	}
	jobPayload, err := json.Marshal(completed)
	if err != nil {
		logger.Error("encode workflow_run job payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	enqueued, err := jobs.Enqueue(r.Context(), jobqueue.NewJob{
		DeliveryID: deliveryID,
		Key:        key,
		Kind:       kind,
		Payload:    jobPayload,
	})
	if err != nil {
		logger.Error("enqueue workflow_run job", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !enqueued {
		logger.Info("duplicate webhook delivery", "delivery_id", deliveryID)
	}

	w.WriteHeader(http.StatusAccepted)
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
