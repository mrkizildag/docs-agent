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

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
)

const maxWebhookBodyBytes = 25 << 20 // GitHub's webhook payload cap

// Enqueuer accepts a durable job for later processing by a worker.
type Enqueuer interface {
	Enqueue(ctx context.Context, job jobqueue.NewJob) (bool, error)
}

// RunLookup maps an analysis workflow run to the pull request it was dispatched for.
type RunLookup interface {
	PRForRun(ctx context.Context, owner, repo string, runID int64) (number int, ok bool, err error)
}

func NewHandler(logger *slog.Logger, webhookSecret []byte, jobs Enqueuer, runs RunLookup) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok")); err != nil {
			logger.Warn("write healthz response", "err", err)
		}
	})
	mux.HandleFunc("POST /webhook", webhookHandler(logger, webhookSecret, jobs, runs))
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

// docsAgentWorkflowPath is the target-repo workflow whose completion carries an analysis result.
const docsAgentWorkflowPath = ".github/workflows/docs-agent.yml"

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
			SHA string `json:"sha"`
		} `json:"head"`
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
	}

	jobPayload, err := json.Marshal(pr)
	if err != nil {
		logger.Error("encode pull_request job payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	job := jobqueue.NewJob{
		DeliveryID: deliveryID,
		Key:        fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number),
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

func handleWorkflowRunEvent(logger *slog.Logger, jobs Enqueuer, runs RunLookup, w http.ResponseWriter, r *http.Request, deliveryID string, body []byte) {
	var payload workflowRunEvent
	if err := json.Unmarshal(body, &payload); err != nil {
		logger.Warn("decode workflow_run payload", "delivery_id", deliveryID, "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if payload.Action != "completed" || payload.WorkflowRun.Path != docsAgentWorkflowPath {
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
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
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
		Key:        fmt.Sprintf("%s/%s#%d", owner, repo, number),
		Kind:       workflowRunJobKind,
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
