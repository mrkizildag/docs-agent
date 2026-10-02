// Package httpapi is the HTTP transport: routing, decoding, and error-to-status mapping.
package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const maxWebhookBodyBytes = 25 << 20 // GitHub's webhook payload cap

func NewHandler(logger *slog.Logger, webhookSecret []byte) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok")); err != nil {
			logger.Warn("write healthz response", "err", err)
		}
	})
	mux.HandleFunc("POST /webhook", webhookHandler(logger, webhookSecret))
	return mux
}

func webhookHandler(logger *slog.Logger, webhookSecret []byte) http.HandlerFunc {
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
