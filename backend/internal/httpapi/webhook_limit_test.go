package httpapi_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
)

func tightWebhookRateLimit() httpapi.WebhookRateLimitConfig {
	return httpapi.WebhookRateLimitConfig{
		GlobalPerSecond: 1000,
		GlobalBurst:     1000,
		PerIPPerSecond:  1,
		PerIPBurst:      1,
	}
}

func TestWebhookRateLimitRejectsBeforeBodyRead(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	secret := []byte("test-secret")
	handler := httpapi.NewHandlerWithWebhookRateLimit(logger, secret, newFakeEnqueuer(), fakeRunLookup{}, tightWebhookRateLimit())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader([]byte(`{"zen":"x"}`)))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sign(secret, []byte(`{"zen":"x"}`)))
	req.RemoteAddr = "203.0.113.10:1234"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first request = %d, want 202", rec.Code)
	}

	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader([]byte(`{"zen":"x"}`)))
	req2.Header.Set("X-GitHub-Event", "ping")
	req2.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	req2.RemoteAddr = "203.0.113.10:1234"

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429 (not 401 from signature check)", rec2.Code)
	}
}

func TestWebhookRateLimitPerIPIndependent(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	secret := []byte("test-secret")
	handler := httpapi.NewHandlerWithWebhookRateLimit(logger, secret, newFakeEnqueuer(), fakeRunLookup{}, tightWebhookRateLimit())

	payload := []byte(`{"zen":"x"}`)
	sig := sign(secret, payload)

	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "ping")
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("X-Forwarded-For", ip)
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("first from %s = %d, want 202", ip, rec.Code)
		}
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second from 198.51.100.1 = %d, want 429", rec.Code)
	}

	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(payload))
	req2.Header.Set("X-GitHub-Event", "ping")
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-Forwarded-For", "198.51.100.2")
	req2.RemoteAddr = "127.0.0.1:1234"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second from 198.51.100.2 = %d, want 429", rec2.Code)
	}
}

func TestWebhookRateLimitIgnoresForwardedForFromNonLoopbackPeer(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	secret := []byte("test-secret")
	handler := httpapi.NewHandlerWithWebhookRateLimit(logger, secret, newFakeEnqueuer(), fakeRunLookup{}, tightWebhookRateLimit())

	payload := []byte(`{"zen":"x"}`)
	sig := sign(secret, payload)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.RemoteAddr = "203.0.113.10:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first = %d, want 202", rec.Code)
	}

	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(payload))
	req2.Header.Set("X-GitHub-Event", "ping")
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-Forwarded-For", "198.51.100.2")
	req2.RemoteAddr = "203.0.113.10:1234"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second with different X-Forwarded-For same peer = %d, want 429", rec2.Code)
	}
}

func TestWebhookRateLimitGlobalBucket(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	secret := []byte("test-secret")
	cfg := httpapi.WebhookRateLimitConfig{
		GlobalPerSecond: 1,
		GlobalBurst:     1,
		PerIPPerSecond:  100,
		PerIPBurst:      100,
	}
	handler := httpapi.NewHandlerWithWebhookRateLimit(logger, secret, newFakeEnqueuer(), fakeRunLookup{}, cfg)

	payload := []byte(`{"zen":"x"}`)
	sig := sign(secret, payload)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-Forwarded-For", "203.0.113.1")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first = %d, want 202", rec.Code)
	}

	req2 := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(payload))
	req2.Header.Set("X-GitHub-Event", "ping")
	req2.Header.Set("X-Hub-Signature-256", sig)
	req2.Header.Set("X-Forwarded-For", "203.0.113.2")
	req2.RemoteAddr = "127.0.0.1:1234"
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second IP under global cap = %d, want 429", rec2.Code)
	}
}
