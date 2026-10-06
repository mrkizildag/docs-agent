package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/config"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobs"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

const (
	maxParallelJobs   = 8
	actionsRunTimeout = 10 * time.Minute
	githubHTTPTimeout = 20 * time.Second
	// shutdownTimeout must fit within compose's stop_grace_period (20s).
	shutdownTimeout = 10 * time.Second
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

	ghHTTPClient := &http.Client{Timeout: githubHTTPTimeout}
	ghClient, err := github.NewClient(ghHTTPClient, cfg.GitHubAppID, []byte(cfg.GitHubPrivateKey.Reveal()), "")
	if err != nil {
		return fmt.Errorf("create GitHub client: %w", err)
	}
	runners, err := buildRunners(cfg, ghClient)
	if err != nil {
		return fmt.Errorf("build analysis runners: %w", err)
	}

	// The gate enqueues scaffold jobs through the worker, and the worker's handler
	// is the gate, so the handler reads gateSvc once the gate is built.
	var gateSvc *gate.Service
	worker := jobqueue.NewWorker(store, func(ctx context.Context, job jobqueue.Job) error {
		return jobs.HandleJob(gateSvc)(ctx, job)
	}, logger, maxParallelJobs)
	gateSvc = gate.NewService(ghClient, ghClient, store, runners, ghClient, jobs.NewScaffoldQueue(worker))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.NewHandler(logger, []byte(cfg.WebhookSecret.Reveal()), worker, store),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return serve(ctx, logger, srv, worker, store)
}

// serve runs the HTTP server, the worker, and the deadline sweep until ctx is
// done or one of the server and the worker fails. It then shuts the server
// down first, so no new job arrives, and drains the sweep and the worker.
func serve(ctx context.Context, logger *slog.Logger, srv *http.Server, worker *jobqueue.Worker, overdue jobs.OverdueSource) error {
	// Background work outlives ctx so in-flight jobs finish during shutdown.
	workerCtx, cancelWorker := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWorker()
	workerErr := make(chan error, 1) // buffered so the goroutine can exit if serve already returned
	go func() { workerErr <- worker.Run(workerCtx) }()

	sweepCtx, cancelSweep := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelSweep()
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		jobs.SweepDeadlines(sweepCtx, overdue, worker, logger)
	}()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
		serveErr <- srv.ListenAndServe()
	}()

	var errs []error
	serverExited, workerExited := false, false
	select {
	case err := <-serveErr:
		serverExited = true
		errs = append(errs, fmt.Errorf("serve %s: %w", srv.Addr, err))
	case err := <-workerErr:
		workerExited = true
		errs = append(errs, fmt.Errorf("run worker: %w", err))
	case <-ctx.Done():
	}

	errs = append(errs, shutdownServer(ctx, srv))
	if !serverExited {
		if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("serve %s: %w", srv.Addr, err))
		}
	}

	cancelSweep()
	<-sweepDone
	cancelWorker()
	if !workerExited {
		if err := <-workerErr; err != nil {
			errs = append(errs, fmt.Errorf("run worker: %w", err))
		}
	}
	return errors.Join(errs...)
}

// llmHTTPTimeout is longer than githubHTTPTimeout: chat completions
// take longer than a REST call.
const llmHTTPTimeout = 60 * time.Second

// buildRunners wires the Actions runner and, from cfg.LLM, the server runner. A nil cfg.LLM
// leaves the server slot empty, so a repo must run the Actions workflow.
func buildRunners(cfg config.Config, ghClient *github.Client) (gate.Runners, error) {
	actionsRunner := actions.New(ghClient, actionsRunTimeout)
	if cfg.LLM == nil {
		return gate.Runners{Actions: actionsRunner}, nil
	}

	if _, err := exec.LookPath("git"); err != nil {
		return gate.Runners{}, fmt.Errorf("LLM_PROVIDER is set, so the server runner needs git on PATH: %w", err)
	}

	var model llm.Model
	switch cfg.LLM.Provider {
	case config.LLMProviderOpenAI:
		model = llm.NewOpenAI(&http.Client{Timeout: llmHTTPTimeout}, cfg.LLM.BaseURL, cfg.LLM.APIKey.Reveal())
	case config.LLMProviderAnthropic:
		model = llm.NewAnthropic(&http.Client{Timeout: llmHTTPTimeout}, cfg.LLM.BaseURL, cfg.LLM.APIKey.Reveal())
	default:
		return gate.Runners{}, errors.New("unreachable: config validated the LLM provider")
	}

	runner := llmrunner.New(model, ghClient.InstallationToken, cfg.LLM.TriageModel, cfg.LLM.Model)
	return gate.Runners{Actions: actionsRunner, Server: runner}, nil
}

func shutdownServer(ctx context.Context, srv *http.Server) error {
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
