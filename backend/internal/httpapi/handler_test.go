package httpapi_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
)

const testAppID = 123

const (
	pullRequestJobKind = "pull_request"
	workflowRunJobKind = "workflow_run"
)

type fakeRunLookup struct {
	numbers map[int64]int
	heads   map[string][]int
	err     error
}

func (f fakeRunLookup) PRsForHead(_ context.Context, _, _, headSHA string) ([]int, error) {
	return f.heads[headSHA], f.err
}

func (f fakeRunLookup) PRForRun(_ context.Context, _, _ string, runID int64) (int, bool, error) {
	number, ok := f.numbers[runID]
	return number, ok, f.err
}

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

	httpapi.NewHandler(logger, []byte("secret"), testAppID, newFakeEnqueuer(), fakeRunLookup{}).ServeHTTP(rec, req)

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

			httpapi.NewHandler(logger, secret, testAppID, enqueuer, fakeRunLookup{}).ServeHTTP(rec, req)

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

	httpapi.NewHandler(logger, secret, testAppID, newFakeEnqueuer(), fakeRunLookup{}).ServeHTTP(rec, req)

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

	return postWebhookWithLookup(t, secret, jobs, fakeRunLookup{}, event, deliveryID, body)
}

func postWebhookWithLookup(t *testing.T, secret []byte, jobs httpapi.Enqueuer, runs httpapi.RunLookup, event string, deliveryID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	if deliveryID != "" {
		req.Header.Set("X-GitHub-Delivery", deliveryID)
	}
	rec := httptest.NewRecorder()

	httpapi.NewHandler(logger, secret, testAppID, jobs, runs).ServeHTTP(rec, req)
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
	calls    []gate.PullRequest
	runCalls []gate.RunCompleted
	err      error
}

func (f *fakePullRequestHandler) HandleDeadline(context.Context, gate.PRRef, string, time.Time) error {
	return f.err
}

func (f *fakePullRequestHandler) HandleRerun(context.Context, gate.RerunRequest) error {
	return f.err
}

func (f *fakePullRequestHandler) HandleRunCompleted(_ context.Context, rc gate.RunCompleted) error {
	f.runCalls = append(f.runCalls, rc)
	return f.err
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

	t.Run("dispatches workflow runs", func(t *testing.T) {
		t.Parallel()

		rc := gate.RunCompleted{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, RunID: 99, Conclusion: "success"}
		rcPayload, err := json.Marshal(rc)
		if err != nil {
			t.Fatalf("marshal run completed: %v", err)
		}
		handler := &fakePullRequestHandler{}
		job := jobqueue.Job{ID: 4, Key: "acme/widgets#7", Kind: workflowRunJobKind, Payload: rcPayload}

		if err := httpapi.HandleJob(handler)(t.Context(), job); err != nil {
			t.Fatalf("HandleJob() error = %v", err)
		}

		if diff := cmp.Diff([]gate.RunCompleted{rc}, handler.runCalls); diff != "" {
			t.Errorf("HandleRunCompleted calls (-want +got):\n%s", diff)
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

func workflowRunBody(t *testing.T, action, path string, runID int64) []byte {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"action": action,
		"workflow_run": map[string]any{
			"id": runID, "path": path, "conclusion": "success",
		},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatalf("marshal workflow_run payload: %v", err)
	}
	return body
}

func postWorkflowRun(t *testing.T, runs httpapi.RunLookup, jobs httpapi.Enqueuer, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	secret := []byte("test-secret")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "workflow_run")
	req.Header.Set("X-GitHub-Delivery", "d-run")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()

	httpapi.NewHandler(slog.New(slog.DiscardHandler), secret, testAppID, jobs, runs).ServeHTTP(rec, req)
	return rec
}

func TestWebhookWorkflowRun(t *testing.T) {
	t.Parallel()

	const path = ".github/workflows/pollux-agent.yml"
	known := fakeRunLookup{numbers: map[int64]int{99: 7}}

	tests := []struct {
		name     string
		runs     fakeRunLookup
		body     []byte
		wantCode int
		wantJob  bool
	}{
		{name: "known run", runs: known, body: workflowRunBody(t, "completed", path, 99), wantCode: http.StatusAccepted, wantJob: true},
		{name: "unknown run", runs: known, body: workflowRunBody(t, "completed", path, 100), wantCode: http.StatusAccepted},
		{name: "other workflow", runs: known, body: workflowRunBody(t, "completed", ".github/workflows/ci.yml", 99), wantCode: http.StatusAccepted},
		{name: "not completed", runs: known, body: workflowRunBody(t, "requested", path, 99), wantCode: http.StatusAccepted},
		{name: "lookup error", runs: fakeRunLookup{err: errors.New("boom")}, body: workflowRunBody(t, "completed", path, 99), wantCode: http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			jobs := newFakeEnqueuer()
			if rec := postWorkflowRun(t, tc.runs, jobs, tc.body); rec.Code != tc.wantCode {
				t.Fatalf("POST /webhook = %d, want %d", rec.Code, tc.wantCode)
			}
			if !tc.wantJob {
				if len(jobs.jobs) != 0 {
					t.Errorf("Enqueue calls = %v, want none", jobs.jobs)
				}
				return
			}

			wantPayload, err := json.Marshal(gate.RunCompleted{InstallationID: 42, Owner: "acme", Repo: "widgets", Number: 7, RunID: 99, Conclusion: "success"})
			if err != nil {
				t.Fatalf("marshal want payload: %v", err)
			}
			want := []jobqueue.NewJob{{DeliveryID: "d-run", Key: "acme/widgets#7", Kind: workflowRunJobKind, Payload: wantPayload}}
			if diff := cmp.Diff(want, jobs.jobs); diff != "" {
				t.Errorf("Enqueue calls (-want +got):\n%s", diff)
			}
		})
	}
}

func checkRunBody(t *testing.T, action, name string, prNumbers ...int) []byte {
	t.Helper()

	return checkRunBodyAt(t, action, name, "head1", prNumbers...)
}

func checkRunBodyAt(t *testing.T, action, name, headSHA string, prNumbers ...int) []byte {
	t.Helper()

	prs := []map[string]any{}
	for _, n := range prNumbers {
		prs = append(prs, map[string]any{"number": n})
	}
	body, err := json.Marshal(map[string]any{
		"action":       action,
		"check_run":    map[string]any{"name": name, "head_sha": headSHA, "pull_requests": prs},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatalf("marshal check_run payload: %v", err)
	}
	return body
}

func TestWebhookCheckRun(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")

	t.Run("rerequested enqueues a rerun per pull request", func(t *testing.T) {
		t.Parallel()

		jobs := newFakeEnqueuer()
		rec := postWebhook(t, secret, jobs, "check_run", "d1", checkRunBody(t, "rerequested", "pollux-agent", 7, 8))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
		}
		if len(jobs.jobs) != 2 {
			t.Fatalf("jobs = %d, want 2", len(jobs.jobs))
		}
		for i, number := range []int{7, 8} {
			job := jobs.jobs[i]
			if job.Kind != pullRequestJobKind || job.Supersedes || job.Key != fmt.Sprintf("acme/widgets#%d", number) {
				t.Errorf("job %d = %+v, want a non-superseding pull_request job for PR %d", i, job, number)
			}
			var payload struct{ Rerun gate.RerunRequest }
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			want := gate.RerunRequest{InstallationID: 42, PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: number}}
			if diff := cmp.Diff(want, payload.Rerun); diff != "" {
				t.Errorf("job %d rerun (-want +got):\n%s", i, diff)
			}
		}
		if jobs.jobs[0].DeliveryID == jobs.jobs[1].DeliveryID {
			t.Errorf("delivery IDs equal (%q), want one per pull request", jobs.jobs[0].DeliveryID)
		}
	})

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"other check name", checkRunBody(t, "rerequested", "lint", 7)},
		{"other action", checkRunBody(t, "completed", "pollux-agent", 7)},
		{"empty pull_requests", checkRunBody(t, "rerequested", "pollux-agent")},
	} {
		t.Run(tc.name+" enqueues nothing", func(t *testing.T) {
			t.Parallel()

			jobs := newFakeEnqueuer()
			rec := postWebhook(t, secret, jobs, "check_run", "d1", tc.body)
			if rec.Code != http.StatusAccepted || len(jobs.jobs) != 0 {
				t.Errorf("status = %d, jobs = %d, want %d and none", rec.Code, len(jobs.jobs), http.StatusAccepted)
			}
		})
	}

	t.Run("empty pull_requests falls back to the stored pull requests at the head", func(t *testing.T) {
		t.Parallel()

		jobs := newFakeEnqueuer()
		runs := fakeRunLookup{heads: map[string][]int{"fork-head": {7, 8}}}
		rec := postWebhookWithLookup(t, secret, jobs, runs, "check_run", "d1", checkRunBodyAt(t, "rerequested", "pollux-agent", "fork-head"))
		if rec.Code != http.StatusAccepted || len(jobs.jobs) != 2 {
			t.Fatalf("status = %d, jobs = %d, want %d and 2", rec.Code, len(jobs.jobs), http.StatusAccepted)
		}
		for i, number := range []int{7, 8} {
			if want := fmt.Sprintf("acme/widgets#%d", number); jobs.jobs[i].Key != want {
				t.Errorf("job %d key = %q, want %q", i, jobs.jobs[i].Key, want)
			}
		}
	})

	t.Run("head lookup error", func(t *testing.T) {
		t.Parallel()

		jobs := newFakeEnqueuer()
		rec := postWebhookWithLookup(t, secret, jobs, fakeRunLookup{err: errors.New("boom")}, "check_run", "d1", checkRunBody(t, "rerequested", "pollux-agent"))
		if rec.Code != http.StatusInternalServerError || len(jobs.jobs) != 0 {
			t.Errorf("status = %d, jobs = %d, want %d and none", rec.Code, len(jobs.jobs), http.StatusInternalServerError)
		}
	})

	t.Run("missing delivery id", func(t *testing.T) {
		t.Parallel()

		jobs := newFakeEnqueuer()
		rec := postWebhook(t, secret, jobs, "check_run", "", checkRunBody(t, "rerequested", "pollux-agent", 7))
		if rec.Code != http.StatusBadRequest || len(jobs.jobs) != 0 {
			t.Errorf("status = %d, jobs = %d, want %d and none", rec.Code, len(jobs.jobs), http.StatusBadRequest)
		}
	})
}

func TestWebhookIssueCommentRequiresThisApp(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	const before, after = "<!-- pollux-agent:summary -->\n- [ ] Re-run analysis", "<!-- pollux-agent:summary -->\n- [x] Re-run analysis"
	for _, tc := range []struct {
		name     string
		appID    int64
		wantJobs int
	}{
		{"this app", testAppID, 1},
		{"another app", testAppID + 1, 0},
		{"no app", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			jobs := newFakeEnqueuer()
			rec := postWebhook(t, secret, jobs, "issue_comment", "d1", issueCommentBody(t, 5, tc.appID, before, after))
			if rec.Code != http.StatusAccepted || len(jobs.jobs) != tc.wantJobs {
				t.Errorf("status = %d, jobs = %d, want %d and %d", rec.Code, len(jobs.jobs), http.StatusAccepted, tc.wantJobs)
			}
		})
	}
}
