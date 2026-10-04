package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
)

const pullRequestJobKind = "pull_request"

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

func TestHealthz(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, []byte("secret"), newFakeEnqueuer()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 \"ok\"", rec.Code, rec.Body.String())
	}
}

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhook(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	body := []byte(`{"zen":"test"}`)

	tests := []struct {
		name       string
		body       []byte
		signature  string
		wantStatus int
	}{
		{
			name:       "valid signature",
			body:       body,
			signature:  sign(secret, body),
			wantStatus: http.StatusAccepted,
		},
		{
			name:       "missing header",
			body:       body,
			signature:  "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong signature",
			body:       body,
			signature:  "sha256=" + strings.Repeat("0", 64),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "signature from different secret",
			body:       body,
			signature:  sign([]byte("other-secret"), body),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "sha1 prefix",
			body:       body,
			signature:  "sha1=" + strings.TrimPrefix(sign(secret, body), "sha256="),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "non-hex signature",
			body:       body,
			signature:  "sha256=not-hex-zz",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logger := slog.New(slog.DiscardHandler)
			enqueuer := newFakeEnqueuer()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(tc.body))
			req.Header.Set("X-GitHub-Delivery", "delivery-id")
			req.Header.Set("X-GitHub-Event", "ping")
			if tc.signature != "" {
				req.Header.Set("X-Hub-Signature-256", tc.signature)
			}
			rec := httptest.NewRecorder()

			httpapi.NewHandler(logger, secret, enqueuer).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("POST /webhook = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusUnauthorized && len(enqueuer.jobs) != 0 {
				t.Errorf("Enqueue calls = %v, want none", enqueuer.jobs)
			}
		})
	}
}

func TestWebhookBodyTooLarge(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	body := bytes.Repeat([]byte("a"), 26<<20)

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, secret, newFakeEnqueuer()).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /webhook with oversized body = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func pullRequestPayload(t *testing.T, action string) []byte {
	t.Helper()

	payload := map[string]any{
		"action": action,
		"number": 7,
		"pull_request": map[string]any{
			"base": map[string]any{"sha": "base123"},
			"head": map[string]any{"sha": "abc123"},
		},
		"repository": map[string]any{
			"name":  "widgets",
			"owner": map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": 42},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal pull_request payload: %v", err)
	}
	return body
}

func postWebhook(t *testing.T, secret []byte, jobs httpapi.Enqueuer, event string, deliveryID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, secret, jobs).ServeHTTP(rec, req)
	return rec
}

func TestWebhookPullRequest(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")

	tests := []struct {
		name       string
		action     string
		wantQueued bool
	}{
		{name: "opened", action: "opened", wantQueued: true},
		{name: "synchronize", action: "synchronize", wantQueued: true},
		{name: "reopened", action: "reopened", wantQueued: true},
		{name: "closed", action: "closed", wantQueued: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			enqueuer := newFakeEnqueuer()
			body := pullRequestPayload(t, tc.action)
			rec := postWebhook(t, secret, enqueuer, "pull_request", "delivery-id", body)

			if rec.Code != http.StatusAccepted {
				t.Errorf("POST /webhook action=%s = %d, want %d", tc.action, rec.Code, http.StatusAccepted)
			}

			if !tc.wantQueued {
				if len(enqueuer.jobs) != 0 {
					t.Errorf("Enqueue calls = %v, want none", enqueuer.jobs)
				}
				return
			}

			if len(enqueuer.jobs) != 1 {
				t.Fatalf("Enqueue calls = %d, want 1", len(enqueuer.jobs))
			}
			job := enqueuer.jobs[0]
			if job.Key != "acme/widgets#7" || job.Kind != pullRequestJobKind || !job.Supersedes || job.DeliveryID != "delivery-id" {
				t.Errorf("NewJob = %+v, want Key=acme/widgets#7 Kind=%s Supersedes=true DeliveryID=delivery-id", job, pullRequestJobKind)
			}

			var pr gate.PullRequest
			if err := json.Unmarshal(job.Payload, &pr); err != nil {
				t.Fatalf("decode job payload: %v", err)
			}
			want := gate.PullRequest{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, BaseSHA: "base123", HeadSHA: "abc123"}
			if diff := cmp.Diff(want, pr); diff != "" {
				t.Errorf("job payload (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWebhookPullRequestDuplicate(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	enqueuer := newFakeEnqueuer()
	enqueuer.result = false
	rec := postWebhook(t, secret, enqueuer, "pull_request", "delivery-id", pullRequestPayload(t, "opened"))

	if rec.Code != http.StatusAccepted {
		t.Errorf("POST /webhook duplicate delivery = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(enqueuer.jobs) != 1 {
		t.Errorf("Enqueue calls = %d, want 1", len(enqueuer.jobs))
	}
}

func TestWebhookPullRequestEnqueueError(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	enqueuer := newFakeEnqueuer()
	enqueuer.err = errors.New("boom")
	rec := postWebhook(t, secret, enqueuer, "pull_request", "delivery-id", pullRequestPayload(t, "opened"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("POST /webhook enqueue error = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestWebhookPullRequestMissingDeliveryID(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	enqueuer := newFakeEnqueuer()
	rec := postWebhook(t, secret, enqueuer, "pull_request", "", pullRequestPayload(t, "opened"))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST /webhook missing delivery id = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if len(enqueuer.jobs) != 0 {
		t.Errorf("Enqueue calls = %v, want none", enqueuer.jobs)
	}
}

func TestWebhookPing(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	enqueuer := newFakeEnqueuer()
	rec := postWebhook(t, secret, enqueuer, "ping", "delivery-id", []byte(`{"zen":"test"}`))

	if rec.Code != http.StatusAccepted {
		t.Errorf("POST /webhook ping = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(enqueuer.jobs) != 0 {
		t.Errorf("Enqueue calls = %v, want none", enqueuer.jobs)
	}
}

func TestWebhookPullRequestMalformedPayload(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body []byte
	}{
		{name: "not json", body: []byte(`not json`)},
		{name: "missing installation", body: []byte(`{"action":"opened","number":7,"pull_request":{"head":{"sha":"abc123"}},"repository":{"name":"widgets","owner":{"login":"acme"}}}`)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			secret := []byte("test-secret")
			enqueuer := newFakeEnqueuer()
			rec := postWebhook(t, secret, enqueuer, "pull_request", "delivery-id", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /webhook = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if len(enqueuer.jobs) != 0 {
				t.Errorf("Enqueue calls = %v, want none", enqueuer.jobs)
			}
		})
	}
}

func TestWebhookPullRequestClosedWithMissingFields(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	enqueuer := newFakeEnqueuer()
	body := []byte(`{"action":"closed","number":7,"pull_request":{"head":{"sha":""}},"repository":{"name":"","owner":{"login":""}}}`)
	rec := postWebhook(t, secret, enqueuer, "pull_request", "delivery-id", body)

	if rec.Code != http.StatusAccepted {
		t.Errorf("POST /webhook closed with missing fields = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(enqueuer.jobs) != 0 {
		t.Errorf("Enqueue calls = %v, want none", enqueuer.jobs)
	}
}

type fakePullRequestHandler struct {
	calls []gate.PullRequest
	err   error
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

		if err := httpapi.HandleJob(handler)(t.Context(), job); err != nil {
			t.Fatalf("HandleJob() error = %v", err)
		}

		if diff := cmp.Diff([]gate.PullRequest{pr}, handler.calls); diff != "" {
			t.Errorf("HandlePullRequest calls (-want +got):\n%s", diff)
		}
	})

	t.Run("unknown kind errors", func(t *testing.T) {
		t.Parallel()

		handler := &fakePullRequestHandler{}
		job := jobqueue.Job{ID: 2, Kind: "unknown", Payload: payload}

		if err := httpapi.HandleJob(handler)(t.Context(), job); err == nil {
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

		err := httpapi.HandleJob(handler)(t.Context(), job)
		if !errors.Is(err, wantErr) {
			t.Errorf("HandleJob() error = %v, want wrapping %v", err, wantErr)
		}
	})
}
