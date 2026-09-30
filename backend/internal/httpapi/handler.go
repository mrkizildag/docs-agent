// Package httpapi is the HTTP transport: routing, decoding, and error-to-status mapping.
package httpapi

import (
	"log/slog"
	"net/http"
)

func NewHandler(logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("ok")); err != nil {
			logger.Warn("write healthz response", "err", err)
		}
	})
	return mux
}
