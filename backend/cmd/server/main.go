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
	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/docs-agent/backend/internal/github"
	"github.com/mrkizildag/docs-agent/backend/internal/httpapi"
	"github.com/mrkizildag/docs-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/docs-agent/backend/internal/review/actions"
)

const (
	maxParallelJobs    = 8
	actionsRunTimeout  = 10 * time.Minute
	deadlineSweepEvery = 30 * time.Second
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

	store, err := sqlite.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", cfg.DatabasePath, err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			logger.Error("close database", "err", err)
		}
	}()

	ghHTTPClient := &http.Client{Timeout: 20 * time.Second}
	ghClient, err := github.NewClient(ghHTTPClient, cfg.GitHubAppID, []byte(cfg.GitHubPrivateKey.Reveal()), "")
	if err != nil {
		return fmt.Errorf("create GitHub client: %w", err)
	}
	gateSvc := gate.NewService(ghClient, store, gate.Runners{Actions: actions.New(ghClient, actionsRunTimeout)})

	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), logger, maxParallelJobs)
	workerCtx, cancelWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorker()

	workerErr := make(chan error, 1)
	go func() {
		workerErr <- worker.Run(workerCtx)
	}()

	sweepCtx, cancelSweep := context.WithCancel(context.WithoutCancel(ctx))
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		sweepDeadlines(sweepCtx, store, worker, logger)
	}()
	defer func() {
		cancelSweep()
		<-sweepDone
	}()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewHandler(logger, []byte(cfg.WebhookSecret.Reveal()), worker, store),
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
		cancelWorker()
		<-workerErr
		return fmt.Errorf("serve %s: %w", cfg.Addr, err)
	case err := <-workerErr:
		shutdownErr := shutdownServer(ctx, srv)
		return errors.Join(fmt.Errorf("run worker: %w", err), shutdownErr)
	case <-ctx.Done():
	}

	if err := shutdownServer(ctx, srv); err != nil {
		cancelWorker()
		<-workerErr
		return err
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		cancelWorker()
		<-workerErr
		return fmt.Errorf("serve %s: %w", cfg.Addr, err)
	}

	cancelWorker()
	if err := <-workerErr; err != nil {
		return fmt.Errorf("run worker: %w", err)
	}
	return nil
}

// sweepDeadlines enqueues deadline jobs for overdue runs until ctx is done.
func sweepDeadlines(ctx context.Context, src httpapi.OverdueSource, jobs httpapi.Enqueuer, logger *slog.Logger) {
	ticker := time.NewTicker(deadlineSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := httpapi.EnqueueDeadlineJobs(ctx, src, jobs, now); err != nil {
				logger.Error("sweep deadlines", "err", err)
			}
		}
	}
}

func shutdownServer(ctx context.Context, srv *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
