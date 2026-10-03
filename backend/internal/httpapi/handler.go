// Package httpapi is the HTTP transport: routing, decoding, and error-to-status mapping.
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
)

const maxWebhookBodyBytes = 25 << 20 // GitHub's webhook payload cap

// handlePullRequestTimeout bounds work done after GitHub's webhook delivery
// abandons the request (10s) but stays under the server's WriteTimeout (30s)
// so the check-run write isn't cut off mid-flight.
const handlePullRequestTimeout = 25 * time.Second

// PullRequestHandler reports the docs-agent check run for a pull request.
type PullRequestHandler interface {
	HandlePullRequest(ctx context.Context, pr gate.PullRequest) error
}

func NewHandler(logger *slog.Logger, webhookSecret []byte, prs PullRequestHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok")); err != nil {
			logger.Warn("write healthz response", "err", err)
		}
	})
	mux.HandleFunc("POST /webhook", webhookHandler(logger, webhookSecret, prs))
	return mux
}

// pullRequestEvent is the subset of GitHub's pull_request webhook payload the
// handler needs.
type pullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
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

func webhookHandler(logger *slog.Logger, webhookSecret []byte, prs PullRequestHandler) http.HandlerFunc {
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

		if event != "pull_request" {
			w.WriteHeader(http.StatusAccepted)
			return
		}

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

		pr := gate.PullRequest{
			InstallationID: payload.Installation.ID,
			Owner:          payload.Repository.Owner.Login,
			Repo:           payload.Repository.Name,
			Number:         payload.Number,
			HeadSHA:        payload.PullRequest.Head.SHA,
		}

		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), handlePullRequestTimeout)
		defer cancel()
		if err := prs.HandlePullRequest(ctx, pr); err != nil {
			logger.Error("handle pull request", "delivery_id", deliveryID, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)
	}
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
