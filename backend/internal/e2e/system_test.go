// Package e2e_test drives the whole system: signed webhooks go through the
// real HTTP handler, the durable job queue and the real SQLite store into the
// gate, which reaches GitHub through a fake and runs the analysis runners.
package e2e_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobs"
)

const (
	webhookSecret = "test-secret"
	waitTimeout   = 10 * time.Second
)

// waitFor polls cond until it holds; the test fails if it does not within waitTimeout.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitNth waits until get returns at least n items and returns the nth.
func waitNth[T any](t *testing.T, what string, get func() []T, n int) T {
	t.Helper()

	waitFor(t, what, func() bool { return len(get()) >= n })
	return get()[n-1]
}

// system is the server as production wires it, minus the process: the HTTP
// handler, the worker over the store, and the gate service the worker runs.
type system struct {
	t          *testing.T
	store      *sqlite.Store
	worker     *jobqueue.Worker
	handler    http.Handler
	deliveries atomic.Int64

	stopOnce sync.Once
	stopErr  error
	cancel   context.CancelFunc
	done     chan error
}

// newWorker wires a gate service over gh and store and the worker that runs its
// jobs; the service enqueues scaffold jobs through the worker.
func newWorker(store *sqlite.Store, gh gate.GitHub, runners gate.Runners) *jobqueue.Worker {
	var svc *gate.Service
	worker := jobqueue.NewWorker(store, func(ctx context.Context, job jobqueue.Job) error {
		return jobs.HandleJob(svc)(ctx, job)
	}, slog.New(slog.DiscardHandler), 8)
	svc = gate.NewService(gh, store, runners, jobs.NewScaffoldQueue(worker))
	return worker
}

// newSystem accepts webhooks but runs no jobs until run is called.
func newSystem(t *testing.T, store *sqlite.Store, gh gate.GitHub, runners gate.Runners) *system {
	t.Helper()

	worker := newWorker(store, gh, runners)
	return &system{
		t:       t,
		store:   store,
		worker:  worker,
		handler: httpapi.NewHandler(slog.New(slog.DiscardHandler), []byte(webhookSecret), worker, store),
	}
}

// start is a newSystem whose worker is already running.
func start(t *testing.T, store *sqlite.Store, gh gate.GitHub, runners gate.Runners) *system {
	t.Helper()

	s := newSystem(t, store, gh, runners)
	s.run()
	return s
}

// run starts the worker; it stops when the test ends or stop is called.
func (s *system) run() {
	var ctx context.Context
	ctx, s.cancel = context.WithCancel(s.t.Context())
	s.done = make(chan error, 1)
	go func() { s.done <- s.worker.Run(ctx) }()
	s.t.Cleanup(func() {
		if err := s.stop(); err != nil {
			s.t.Errorf("worker.Run() error = %v", err)
		}
	})
}

// stop stops the worker and returns what Run returned; later calls return the same.
func (s *system) stop() error {
	s.stopOnce.Do(func() {
		s.cancel()
		s.stopErr = <-s.done
	})
	return s.stopErr
}

// postAs sends a signed webhook and returns the response status.
func (s *system) postAs(deliveryID, event string, body []byte) int {
	s.t.Helper()

	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(body)
	req := httptest.NewRequestWithContext(s.t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec.Code
}

// deliverAs sends a webhook under a chosen delivery ID and requires it be accepted.
func (s *system) deliverAs(deliveryID, event string, body []byte) {
	s.t.Helper()

	if code := s.postAs(deliveryID, event, body); code != http.StatusAccepted {
		s.t.Fatalf("POST /webhook %s %s = %d, want %d", event, deliveryID, code, http.StatusAccepted)
	}
}

// deliver sends a webhook under a fresh delivery ID and requires it be accepted.
func (s *system) deliver(event string, body []byte) {
	s.t.Helper()

	s.deliverAs(fmt.Sprintf("auto%d", s.deliveries.Add(1)), event, body)
}

// pushOpts varies a pull_request delivery for PR 1 of acme/widgets; zero values
// mean an "opened" push by user dev from a branch of the base repository.
type pushOpts struct {
	action     string
	baseSHA    string
	headRepo   string
	sender     string
	senderType string
}

func pullRequestBody(t *testing.T, number int, sha string, o pushOpts) []byte {
	t.Helper()

	return marshal(t, map[string]any{
		"action": cmp.Or(o.action, "opened"),
		"number": number,
		"pull_request": map[string]any{
			"base": map[string]any{"sha": cmp.Or(o.baseSHA, "base1")},
			"head": map[string]any{"sha": sha, "ref": "feature", "repo": map[string]any{"full_name": cmp.Or(o.headRepo, "acme/widgets")}},
		},
		"repository": map[string]any{
			"name":      "widgets",
			"full_name": "acme/widgets",
			"owner":     map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": cmp.Or(o.sender, "dev"), "type": cmp.Or(o.senderType, "User")},
	})
}

func workflowRunBody(t *testing.T, runID int64, conclusion string) []byte {
	t.Helper()

	return marshal(t, map[string]any{
		"action": "completed",
		"workflow_run": map[string]any{
			"id": runID, "path": ".github/workflows/pollux-agent.yml", "conclusion": conclusion,
		},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
}

func checkRunRerequestedBody(t *testing.T) []byte {
	t.Helper()

	return marshal(t, map[string]any{
		"action":       "rerequested",
		"check_run":    map[string]any{"name": "pollux-agent", "head_sha": "head1", "pull_requests": []map[string]any{{"number": 1}}},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()

	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal webhook payload: %v", err)
	}
	return body
}
