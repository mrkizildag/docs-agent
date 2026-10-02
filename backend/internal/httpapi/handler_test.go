package httpapi_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
)

func TestHealthz(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, []byte("secret")).ServeHTTP(rec, req)

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

			httpapi.NewHandler(logger, secret).ServeHTTP(rec, req)

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

	httpapi.NewHandler(logger, secret).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("POST /webhook with oversized body = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}
