package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/config"
	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewHandler(logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serveErr := make(chan error, 1) // buffered so the goroutine can exit if run already returned
	go func() {
		logger.Info("listening", "addr", cfg.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve %s: %w", cfg.Addr, err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", cfg.Addr, err)
	}
	return nil
}
