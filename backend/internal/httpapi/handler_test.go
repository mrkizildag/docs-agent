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
)

type fakePullRequestHandler struct {
	calls []gate.PullRequest
	err   error
}

func (f *fakePullRequestHandler) HandlePullRequest(_ context.Context, pr gate.PullRequest) error {
	f.calls = append(f.calls, pr)
	return f.err
}

type fakePullRequestHandlerFunc struct {
	fn func(ctx context.Context, pr gate.PullRequest) error
}

func (f *fakePullRequestHandlerFunc) HandlePullRequest(ctx context.Context, pr gate.PullRequest) error {
	return f.fn(ctx, pr)
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, []byte("secret"), &fakePullRequestHandler{}).ServeHTTP(rec, req)

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
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(tc.body))
			req.Header.Set("X-GitHub-Delivery", "delivery-id")
			req.Header.Set("X-GitHub-Event", "ping")
			if tc.signature != "" {
				req.Header.Set("X-Hub-Signature-256", tc.signature)
			}
			rec := httptest.NewRecorder()

			httpapi.NewHandler(logger, secret, &fakePullRequestHandler{}).ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("POST /webhook = %d, want %d", rec.Code, tc.wantStatus)
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

	httpapi.NewHandler(logger, secret, &fakePullRequestHandler{}).ServeHTTP(rec, req)

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

func postWebhook(t *testing.T, secret []byte, prs httpapi.PullRequestHandler, event string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, secret, prs).ServeHTTP(rec, req)
	return rec
}

func TestWebhookPullRequest(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")

	tests := []struct {
		name       string
		action     string
		wantCalled bool
	}{
		{name: "opened", action: "opened", wantCalled: true},
		{name: "synchronize", action: "synchronize", wantCalled: true},
		{name: "reopened", action: "reopened", wantCalled: true},
		{name: "closed", action: "closed", wantCalled: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler := &fakePullRequestHandler{}
			body := pullRequestPayload(t, tc.action)
			rec := postWebhook(t, secret, handler, "pull_request", body)

			if rec.Code != http.StatusAccepted {
				t.Errorf("POST /webhook action=%s = %d, want %d", tc.action, rec.Code, http.StatusAccepted)
			}

			if !tc.wantCalled {
				if len(handler.calls) != 0 {
					t.Errorf("HandlePullRequest calls = %v, want none", handler.calls)
				}
				return
			}

			want := []gate.PullRequest{
				{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "abc123"},
			}
			if diff := cmp.Diff(want, handler.calls); diff != "" {
				t.Errorf("HandlePullRequest calls (-want +got):\n%s", diff)
			}
		})
	}
}

func TestWebhookPing(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	handler := &fakePullRequestHandler{}
	rec := postWebhook(t, secret, handler, "ping", []byte(`{"zen":"test"}`))

	if rec.Code != http.StatusAccepted {
		t.Errorf("POST /webhook ping = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(handler.calls) != 0 {
		t.Errorf("HandlePullRequest calls = %v, want none", handler.calls)
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
			handler := &fakePullRequestHandler{}
			rec := postWebhook(t, secret, handler, "pull_request", tc.body)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /webhook = %d, want %d", rec.Code, http.StatusBadRequest)
			}
			if len(handler.calls) != 0 {
				t.Errorf("HandlePullRequest calls = %v, want none", handler.calls)
			}
		})
	}
}

func TestWebhookPullRequestHandlerError(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	handler := &fakePullRequestHandler{err: errors.New("boom")}
	rec := postWebhook(t, secret, handler, "pull_request", pullRequestPayload(t, "opened"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("POST /webhook handler error = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestWebhookPullRequestClosedWithMissingFields(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	handler := &fakePullRequestHandler{}
	body := []byte(`{"action":"closed","number":7,"pull_request":{"head":{"sha":""}},"repository":{"name":"","owner":{"login":""}}}`)
	rec := postWebhook(t, secret, handler, "pull_request", body)

	if rec.Code != http.StatusAccepted {
		t.Errorf("POST /webhook closed with missing fields = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if len(handler.calls) != 0 {
		t.Errorf("HandlePullRequest calls = %v, want none", handler.calls)
	}
}

func TestWebhookPullRequestContextDetachedFromRequest(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	var sawErr error
	handler := &fakePullRequestHandlerFunc{
		fn: func(ctx context.Context, _ gate.PullRequest) error {
			sawErr = ctx.Err()
			return nil
		},
	}

	logger := slog.New(slog.DiscardHandler)
	body := pullRequestPayload(t, "opened")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, secret, handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Errorf("POST /webhook with cancelled request context = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if sawErr != nil {
		t.Errorf("ctx.Err() inside HandlePullRequest = %v, want nil", sawErr)
	}
}
