package httpapi_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
)

type e2eCheckRunCall struct {
	installationID int64
	owner          string
	repo           string
	run            gate.CheckRun
}

type e2eGitHub struct {
	noComments
	calls chan e2eCheckRunCall
}

// noComments is the comment and pull request lookup surface of a fake that
// never posts comments or re-runs.
type noComments struct{}

func (noComments) GetPullRequest(_ context.Context, installationID int64, owner, repo string, number int) (gate.PullRequest, error) {
	return gate.PullRequest{InstallationID: installationID, Owner: owner, Repo: repo, Number: number}, nil
}

func (noComments) ListComments(context.Context, int64, string, string, int) ([]gate.Comment, error) {
	return nil, nil
}

func (noComments) CreateReviewComment(context.Context, int64, string, string, int, gate.ReviewComment) (gate.Comment, error) {
	return gate.Comment{}, nil
}

func (noComments) EditReviewComment(context.Context, int64, string, string, int64, string) error {
	return nil
}

func (noComments) CreateIssueComment(context.Context, int64, string, string, int, string) (gate.Comment, error) {
	return gate.Comment{}, nil
}

func (noComments) EditIssueComment(context.Context, int64, string, string, int64, string) error {
	return nil
}

func (f *e2eGitHub) WorkflowExists(_ context.Context, _ int64, _, _ string) (bool, error) {
	return false, nil
}

func (f *e2eGitHub) ListChangedFiles(_ context.Context, _ int64, _, _ string, _ int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *e2eGitHub) CreateCheckRun(_ context.Context, installationID int64, owner, repo string, run gate.CheckRun) (int64, error) {
	f.calls <- e2eCheckRunCall{installationID: installationID, owner: owner, repo: repo, run: run}
	return 0, nil
}

func (f *e2eGitHub) UpdateCheckRun(context.Context, int64, string, string, int64, gate.CheckRun) error {
	return nil
}

func e2ePullRequestBody(t *testing.T, number int, sha string) []byte {
	t.Helper()

	payload := map[string]any{
		"action": "opened",
		"number": number,
		"pull_request": map[string]any{
			"head": map[string]any{"sha": sha},
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

func waitCheckRun(t *testing.T, calls chan e2eCheckRunCall) e2eCheckRunCall {
	t.Helper()

	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for check run")
		return e2eCheckRunCall{}
	}
}

func TestWebhookToCheckRunEndToEnd(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	dbPath := filepath.Join(t.TempDir(), "pollux.db")

	store, err := sqlite.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) error = %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	gh := &e2eGitHub{calls: make(chan e2eCheckRunCall, 10)}
	gateSvc := gate.NewService(gh, store, gate.Runners{})

	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), logger, 8)

	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- worker.Run(workerCtx)
	}()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	handler := httpapi.NewHandler(logger, secret, worker, store)

	post := func(deliveryID string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-GitHub-Delivery", deliveryID)
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	firstBody := e2ePullRequestBody(t, 1, "sha1")
	if rec := post("d1", firstBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook first delivery = %d, want %d", rec.Code, http.StatusAccepted)
	}

	firstRun := waitCheckRun(t, gh.calls)
	if firstRun.run.HeadSHA != "sha1" {
		t.Errorf("first check run HeadSHA = %q, want %q", firstRun.run.HeadSHA, "sha1")
	}

	if rec := post("d1", firstBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook duplicate delivery = %d, want %d", rec.Code, http.StatusAccepted)
	}

	// A distinct PR acts as a barrier: it runs on a different key, in parallel with any
	// (incorrect) duplicate job, giving the worker a chance to have drained one if it existed.
	barrierBody := e2ePullRequestBody(t, 2, "sha2")
	if rec := post("d2", barrierBody); rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook barrier delivery = %d, want %d", rec.Code, http.StatusAccepted)
	}

	barrierRun := waitCheckRun(t, gh.calls)
	if barrierRun.run.HeadSHA != "sha2" {
		t.Errorf("barrier check run HeadSHA = %q, want %q", barrierRun.run.HeadSHA, "sha2")
	}

	select {
	case extra := <-gh.calls:
		t.Errorf("unexpected extra check run: %+v", extra)
	default:
	}
}

// savedStore reports each saved PRState, so a test knows the gate has recorded
// an awaited run before the workflow_run webhook arrives.
type savedStore struct {
	*sqlite.Store
	saved chan gate.PRState
}

func (s *savedStore) SavePR(ctx context.Context, state gate.PRState) error {
	if err := s.Store.SavePR(ctx, state); err != nil {
		return fmt.Errorf("save pr: %w", err)
	}
	s.saved <- state
	return nil
}

// fakeActionsGitHub serves the GitHub API surface of a repo with the pollux-agent
// workflow, recording the dispatch and check run requests.
type fakeActionsGitHub struct {
	t       *testing.T
	created chan map[string]any
	updated chan map[string]any

	mu         sync.Mutex
	dispatched map[string]any
	blobURL    string
}

func (f *fakeActionsGitHub) json(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil {
		f.t.Errorf("write response: %v", err)
	}
}

func (f *fakeActionsGitHub) decode(r *http.Request) map[string]any {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode %s %s body: %v", r.Method, r.URL.Path, err)
	}
	return body
}

func (f *fakeActionsGitHub) resultZip() []byte {
	f.mu.Lock()
	inputs, _ := f.dispatched["inputs"].(map[string]any)
	f.mu.Unlock()

	result, err := json.Marshal(map[string]any{
		"head_sha": "sha1",
		"nonce":    inputs["nonce"],
		"claude": map[string]any{
			"is_error": false,
			"structured_output": map[string]any{
				"proposals": []any{map[string]any{
					"doc_path": "docs/features/greeting.md", "section": "Greeting",
					"anchor": map[string]any{"file": "src/greet.py", "line": 3},
					"reason": "greeting changed", "content": "Hello!",
				}},
			},
		},
	})
	if err != nil {
		f.t.Errorf("marshal result: %v", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	file, err := zw.Create("result.json")
	if err != nil {
		f.t.Errorf("create zip entry: %v", err)
	}
	if _, err := file.Write(result); err != nil {
		f.t.Errorf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		f.t.Errorf("close zip: %v", err)
	}
	return buf.Bytes()
}

func (f *fakeActionsGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)))
	})
	mux.HandleFunc("GET /repos/acme/widgets/contents/.github/workflows/pollux-agent.yml", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"type":"file","name":"pollux-agent.yml","path":".github/workflows/pollux-agent.yml"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"default_branch":"main"}`)
	})
	mux.HandleFunc("POST /repos/acme/widgets/actions/workflows/pollux-agent.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.dispatched = body
		f.mu.Unlock()
		f.json(w, http.StatusOK, `{"workflow_run_id":4242}`)
	})
	mux.HandleFunc("POST /repos/acme/widgets/check-runs", func(w http.ResponseWriter, r *http.Request) {
		f.created <- f.decode(r)
		f.json(w, http.StatusCreated, `{"id":555}`)
	})
	mux.HandleFunc("PATCH /repos/acme/widgets/check-runs/555", func(w http.ResponseWriter, r *http.Request) {
		f.updated <- f.decode(r)
		f.json(w, http.StatusOK, `{"id":555}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/runs/4242/artifacts", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"total_count":1,"artifacts":[{"id":9,"name":"pollux-agent-result","workflow_run":{"id":4242}}]}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/artifacts/9/zip", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		blobURL := f.blobURL
		f.mu.Unlock()
		http.Redirect(w, r, blobURL, http.StatusFound)
	})
	mux.HandleFunc("GET /repos/acme/widgets/pulls/{number}/files", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `[{"filename":"src/greet.py","status":"modified","patch":"@@ -1,3 +1,4 @@\n a\n b\n+c\n d"}]`)
	})
	mux.HandleFunc("GET /blob", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(f.resultZip()); err != nil {
			f.t.Errorf("write blob: %v", err)
		}
	})
	return mux
}

func e2eWorkflowRunBody(t *testing.T) []byte {
	t.Helper()

	return e2eWorkflowRunCompletedBody(t, 4242, "success")
}

func e2eWorkflowRunCompletedBody(t *testing.T, runID int64, conclusion string) []byte {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"action": "completed",
		"workflow_run": map[string]any{
			"id": runID, "path": ".github/workflows/pollux-agent.yml", "conclusion": conclusion,
		},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatalf("marshal workflow_run payload: %v", err)
	}
	return body
}

func TestActionsRunnerEndToEnd(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")

	baseStore, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "pollux.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := baseStore.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	store := &savedStore{Store: baseStore, saved: make(chan gate.PRState, 10)}

	github := &fakeActionsGitHub{t: t, created: make(chan map[string]any, 1), updated: make(chan map[string]any, 1)}
	srv := httptest.NewServer(github.handler())
	t.Cleanup(srv.Close)
	github.blobURL = srv.URL + "/blob"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, keyPEM, srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	gateSvc := gate.NewService(client, store, gate.Runners{Actions: actions.New(client, 10*time.Minute)})
	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(baseStore, httpapi.HandleJob(gateSvc), logger, 8)

	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	handler := httpapi.NewHandler(logger, secret, worker, baseStore)
	post := func(event, deliveryID string, body []byte) {
		t.Helper()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", deliveryID)
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST /webhook %s = %d, want %d", event, rec.Code, http.StatusAccepted)
		}
	}
	wait := func(ch chan map[string]any, what string) map[string]any {
		t.Helper()

		select {
		case body := <-ch:
			return body
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
			return nil
		}
	}

	post("pull_request", "d1", e2ePullRequestBody(t, 1, "sha1"))

	created := wait(github.created, "check run create")
	if created["status"] != "in_progress" || created["conclusion"] != nil || created["head_sha"] != "sha1" {
		t.Errorf("created check run = %v, want in_progress on sha1 with no conclusion", created)
	}
	// The first save arms the check run before the dispatch; the awaited run follows it.
	var saved gate.PRState
	for saved.Run == nil || saved.Run.RunID == 0 {
		select {
		case saved = <-store.saved:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for awaited run to be saved")
		}
	}
	if saved.Run.RunID != 4242 || saved.CheckRunID != 555 {
		t.Errorf("saved state = %+v, want awaiting run 4242 with check run 555", saved)
	}

	github.mu.Lock()
	dispatched := github.dispatched
	github.mu.Unlock()
	inputs, _ := dispatched["inputs"].(map[string]any)
	if dispatched["ref"] != "main" || dispatched["return_run_details"] != true ||
		inputs["head_sha"] != "sha1" || inputs["pr_number"] != "1" || inputs["nonce"] == "" {
		t.Errorf("dispatch body = %v, want ref main, return_run_details, and head sha1, PR 1, a nonce", dispatched)
	}

	post("workflow_run", "d2", e2eWorkflowRunBody(t))

	updated := wait(github.updated, "check run update")
	if updated["status"] != "completed" || updated["conclusion"] != "action_required" {
		t.Errorf("updated check run = %v, want completed action_required", updated)
	}
	output, _ := updated["output"].(map[string]any)
	if summary, _ := output["summary"].(string); !strings.Contains(summary, "docs/features/greeting.md") {
		t.Errorf("updated check run output = %v, want the proposal for docs/features/greeting.md", output)
	}
}

type commentGitHub struct {
	noComments
	checkRuns chan gate.CheckRun
	review    chan gate.ReviewComment
	issue     chan string
	created   int64
}

func (f *commentGitHub) WorkflowExists(context.Context, int64, string, string) (bool, error) {
	return false, nil
}

func (f *commentGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, _ int64, run gate.CheckRun) error {
	f.checkRuns <- run
	return nil
}

func (f *commentGitHub) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *commentGitHub) CreateCheckRun(context.Context, int64, string, string, gate.CheckRun) (int64, error) {
	return 1, nil
}

func (f *commentGitHub) CreateReviewComment(_ context.Context, _ int64, _, _ string, _ int, c gate.ReviewComment) (gate.Comment, error) {
	f.review <- c
	f.created++
	id := f.created
	return gate.Comment{ID: id, URL: fmt.Sprintf("https://github.com/acme/widgets/pull/1#discussion_r%d", id)}, nil
}

func (f *commentGitHub) CreateIssueComment(_ context.Context, _ int64, _, _ string, _ int, body string) (gate.Comment, error) {
	f.issue <- body
	return gate.Comment{ID: 900}, nil
}

type proposalRunner struct{ proposals review.Proposals }

func (r proposalRunner) Start(context.Context, review.Request) (review.Started, error) {
	return review.Result{Runner: "fake", Verdict: r.proposals}, nil
}

func TestWebhookToProposalCommentsEndToEnd(t *testing.T) {
	t.Parallel()

	secret := []byte("test-secret")
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "pollux.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	gh := &commentGitHub{checkRuns: make(chan gate.CheckRun, 2), review: make(chan gate.ReviewComment, 4), issue: make(chan string, 2)}
	runner := proposalRunner{proposals: review.Proposals{
		{DocPath: "docs/a.md", Section: "Usage", Anchor: review.Anchor{File: "a.go", Line: 4}, Reason: "flag renamed", Original: "## Usage\nold\n", Lines: review.LineRange{Start: 3, End: 4}, Content: "## Usage\nnew\n"},
		{DocPath: "docs/b.md", Anchor: review.Anchor{File: "b.go", Line: 9}, Reason: "new feature", Content: "# B\n", IndexEntry: "- [B](b.md)"},
	}}
	gateSvc := gate.NewService(gh, store, gate.Runners{Server: runner})

	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), logger, 8)
	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	body := e2ePullRequestBody(t, 1, "sha1")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "d1")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	httpapi.NewHandler(logger, secret, worker, store).ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook = %d, want %d", rec.Code, http.StatusAccepted)
	}

	timeout := time.After(5 * time.Second)
	var reviews []gate.ReviewComment
	for len(reviews) < 2 {
		select {
		case c := <-gh.review:
			reviews = append(reviews, c)
		case <-timeout:
			t.Fatalf("timed out waiting for review comments, got %d", len(reviews))
		}
	}
	var summary string
	select {
	case summary = <-gh.issue:
	case <-timeout:
		t.Fatal("timed out waiting for summary comment")
	}
	var run gate.CheckRun
	select {
	case run = <-gh.checkRuns:
	case <-timeout:
		t.Fatal("timed out waiting for check run")
	}

	if run.Conclusion != gate.ConclusionActionRequired {
		t.Errorf("check run conclusion = %q, want %q", run.Conclusion, gate.ConclusionActionRequired)
	}
	for i, want := range []struct{ path, marker string }{
		{"a.go", "<!-- pollux-agent:proposal:" + gate.ProposalID("docs/a.md", "Usage") + " -->"},
		{"b.go", "<!-- pollux-agent:proposal:" + gate.ProposalID("docs/b.md", "") + " -->"},
	} {
		if reviews[i].Path != want.path || reviews[i].CommitSHA != "sha1" || !strings.Contains(reviews[i].Body, want.marker) {
			t.Errorf("review comment %d = %+v, want path %s on sha1 with marker %s", i, reviews[i], want.path, want.marker)
		}
		if !strings.Contains(summary, fmt.Sprintf("discussion_r%d", i+1)) {
			t.Errorf("summary missing link to comment %d:\n%s", i+1, summary)
		}
	}
	if !strings.Contains(summary, "<!-- pollux-agent:summary -->") {
		t.Errorf("summary missing marker:\n%s", summary)
	}

	var state gate.PRState
	for state.SummaryCommentID == 0 {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for saved state")
		case <-time.After(10 * time.Millisecond):
		}
		if state, err = store.LoadPR(t.Context(), "acme", "widgets", 1); err != nil {
			t.Fatalf("LoadPR() error = %v", err)
		}
	}
	if len(state.Proposals) != 2 || state.Proposals[0].CommentID != 1 || state.Proposals[1].CommentID != 2 {
		t.Errorf("saved proposals = %+v, want comment IDs 1 and 2", state.Proposals)
	}
}

// statefulGitHub keeps the PR's comments like GitHub does: created comments are
// listed back and edits replace bodies.
type statefulGitHub struct {
	checkRuns chan gate.CheckRun // each concluded check run, in order

	mu         sync.Mutex
	head       string
	workflow   bool
	comments   []gate.Comment
	creates    int
	edits      int
	checkRunID int64
	concluded  []concludedCheckRun
}

type concludedCheckRun struct {
	id  int64
	run gate.CheckRun
}

func (f *statefulGitHub) GetPullRequest(_ context.Context, installationID int64, owner, repo string, number int) (gate.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return gate.PullRequest{InstallationID: installationID, Owner: owner, Repo: repo, Number: number, HeadSHA: f.head}, nil
}

func (f *statefulGitHub) WorkflowExists(context.Context, int64, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workflow, nil
}

func (f *statefulGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, id int64, run gate.CheckRun) error {
	f.mu.Lock()
	f.concluded = append(f.concluded, concludedCheckRun{id: id, run: run})
	f.mu.Unlock()
	f.checkRuns <- run
	return nil
}

func (f *statefulGitHub) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *statefulGitHub) CreateCheckRun(context.Context, int64, string, string, gate.CheckRun) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkRunID++
	return f.checkRunID, nil
}

func (f *statefulGitHub) ListComments(context.Context, int64, string, string, int) ([]gate.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments), nil
}

func (f *statefulGitHub) CreateReviewComment(_ context.Context, _ int64, _, _ string, _ int, c gate.ReviewComment) (gate.Comment, error) {
	return f.create(gate.CommentKindReview, c.Body), nil
}

func (f *statefulGitHub) CreateIssueComment(_ context.Context, _ int64, _, _ string, _ int, body string) (gate.Comment, error) {
	return f.create(gate.CommentKindIssue, body), nil
}

func (f *statefulGitHub) EditReviewComment(_ context.Context, _ int64, _, _ string, id int64, body string) error {
	return f.edit(gate.CommentKindReview, id, body)
}

func (f *statefulGitHub) EditIssueComment(_ context.Context, _ int64, _, _ string, id int64, body string) error {
	return f.edit(gate.CommentKindIssue, id, body)
}

func (f *statefulGitHub) create(kind gate.CommentKind, body string) gate.Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	id := int64(len(f.comments) + 1)
	c := gate.Comment{ID: id, Mine: true, Kind: kind, URL: fmt.Sprintf("https://github.com/acme/widgets/pull/1#comment_%d", id), Body: body}
	f.comments = append(f.comments, c)
	return c
}

func (f *statefulGitHub) edit(kind gate.CommentKind, id int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.comments {
		if c.Kind == kind && c.ID == id {
			f.comments[i].Body = body
			f.edits++
			return nil
		}
	}
	return fmt.Errorf("edit %s comment %d: not found", kind, id)
}

// snapshot returns the comments and the create and edit counts so far.
func (f *statefulGitHub) snapshot() (comments []gate.Comment, creates, edits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments), f.creates, f.edits
}

// scriptedRunner plays one queued outcome per run: a review.Verdict is a
// finished analysis, a review.Pending is an external run, an error is a failed
// one. Collect never finds a result, which is what a failed workflow run leaves.
type scriptedRunner struct{ outcomes chan any }

// blockedRun is an outcome that holds the analysis until its context is
// cancelled, then fails the way an interrupted server analysis does.
type blockedRun struct{ started chan struct{} }

func (r scriptedRunner) Start(ctx context.Context, rq review.Request) (review.Started, error) {
	switch o := (<-r.outcomes).(type) {
	case blockedRun:
		close(o.started)
		<-ctx.Done()
		return nil, &review.FailedError{Cause: review.CauseTimeout, Err: ctx.Err()}
	case review.Verdict:
		return review.Result{Runner: "fake", Verdict: o}, nil
	case review.Pending:
		return o, nil
	case error:
		return nil, o
	default:
		return nil, fmt.Errorf("scriptedRunner: unsupported outcome %T", o)
	}
}

func (scriptedRunner) Collect(context.Context, review.Completion) (review.Result, error) {
	return review.Result{}, errors.New("scriptedRunner: no result artifact")
}

func twoProposals() review.Proposals {
	return review.Proposals{
		{DocPath: "docs/a.md", Section: "Usage", Anchor: review.Anchor{File: "a.go", Line: 4}, Reason: "flag renamed", Original: "## Usage\nold\n", Lines: review.LineRange{Start: 3, End: 4}, Content: "## Usage\nnew\n"},
		{DocPath: "docs/b.md", Anchor: review.Anchor{File: "b.go", Line: 9}, Reason: "new feature", Content: "# B\n", IndexEntry: "- [B](b.md)"},
	}
}

// pushHarness drives synchronize webhooks for PR 1 through the real handler,
// worker and sqlite store.
type pushHarness struct {
	t       *testing.T
	gh      *statefulGitHub
	store   *sqlite.Store
	handler http.Handler
	secret  []byte
	deliver int
}

func newPushHarness(t *testing.T, outcomes ...any) *pushHarness {
	t.Helper()

	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "pollux.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	queued := make(chan any, len(outcomes))
	for _, o := range outcomes {
		queued <- o
	}
	gh := &statefulGitHub{checkRuns: make(chan gate.CheckRun, len(outcomes))}
	gateSvc := gate.NewService(gh, store, gate.Runners{Actions: scriptedRunner{outcomes: queued}, Server: scriptedRunner{outcomes: queued}})

	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), logger, 8)
	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	secret := []byte("test-secret")
	return &pushHarness{t: t, gh: gh, store: store, handler: httpapi.NewHandler(logger, secret, worker, store), secret: secret}
}

// push delivers a synchronize webhook for sha and returns the check run and the
// saved state once the run has finished.
func (h *pushHarness) push(sha string) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	h.send(sha)
	return h.waitConcluded(sha)
}

// send delivers a synchronize webhook for sha without waiting for the run.
func (h *pushHarness) send(sha string) {
	h.t.Helper()

	h.gh.mu.Lock()
	h.gh.head = sha
	h.gh.mu.Unlock()

	h.deliver++
	body := bytes.Replace(e2ePullRequestBody(h.t, 1, sha), []byte(`"opened"`), []byte(`"synchronize"`), 1)
	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("d%d", h.deliver))
	req.Header.Set("X-Hub-Signature-256", sign(h.secret, body))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		h.t.Fatalf("POST /webhook for %s = %d, want %d", sha, rec.Code, http.StatusAccepted)
	}
}

// waitConcluded waits for the next concluded check run and the saved state of sha.
func (h *pushHarness) waitConcluded(sha string) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	var run gate.CheckRun
	select {
	case run = <-h.gh.checkRuns:
	case <-time.After(5 * time.Second):
		h.t.Fatalf("timed out waiting for check run on %s", sha)
	}

	// The check run is concluded before comments are written and state is saved.
	deadline := time.After(5 * time.Second)
	for {
		state, err := h.store.LoadPR(h.t.Context(), "acme", "widgets", 1)
		if err != nil {
			h.t.Fatalf("LoadPR() error = %v", err)
		}
		if state.HeadSHA == sha && state.Run == nil && h.commentIDsSaved(state) {
			return run, state
		}
		select {
		case <-deadline:
			h.t.Fatalf("timed out waiting for state on %s", sha)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// commentIDsSaved reports whether state already records every comment written
// so far: gate saves the concluded state before it writes comments.
func (h *pushHarness) commentIDsSaved(state gate.PRState) bool {
	comments, _, _ := h.gh.snapshot()
	if len(comments) > 0 && state.SummaryCommentID == 0 {
		return false
	}
	for _, p := range state.Proposals {
		if p.CommentID == 0 {
			return false
		}
	}
	return true
}

func (h *pushHarness) commentWith(marker string) gate.Comment {
	h.t.Helper()

	comments, _, _ := h.gh.snapshot()
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return c
		}
	}
	h.t.Fatalf("no comment contains %q in %+v", marker, comments)
	return gate.Comment{}
}

func TestWebhookReconcilesProposalCommentsAcrossPushes(t *testing.T) {
	t.Parallel()

	markerA := "<!-- pollux-agent:proposal:" + gate.ProposalID("docs/a.md", "Usage") + " -->"
	markerB := "<!-- pollux-agent:proposal:" + gate.ProposalID("docs/b.md", "") + " -->"
	summaryMarker := "<!-- pollux-agent:summary -->"

	t.Run("same proposals edit in place", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), twoProposals())

		h.push("sha1")
		_, creates, _ := h.gh.snapshot()
		if creates != 3 {
			t.Fatalf("creates after first push = %d, want 3", creates)
		}

		run, _ := h.push("sha2")
		comments, creates, edits := h.gh.snapshot()
		if creates != 3 || len(comments) != 3 {
			t.Errorf("after second push creates = %d, comments = %d, want 3 and 3", creates, len(comments))
		}
		if edits != 3 {
			t.Errorf("edits = %d, want 3 (two review comments and the summary)", edits)
		}
		if run.Conclusion != gate.ConclusionActionRequired {
			t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionActionRequired)
		}
		if body := h.commentWith(markerA).Body; strings.Contains(body, "Outdated") {
			t.Errorf("comment A marked outdated:\n%s", body)
		}
	})

	t.Run("dropped proposal is marked outdated", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), review.Proposals{twoProposals()[0]})

		h.push("sha1")
		run, state := h.push("sha2")

		if run.Conclusion != gate.ConclusionActionRequired {
			t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionActionRequired)
		}
		if body := h.commentWith(markerB).Body; !strings.Contains(body, "Outdated") {
			t.Errorf("dropped proposal comment not outdated:\n%s", body)
		}
		if body := h.commentWith(markerA).Body; strings.Contains(body, "Outdated") {
			t.Errorf("kept proposal comment marked outdated:\n%s", body)
		}
		summary := h.commentWith(summaryMarker).Body
		if !strings.Contains(summary, "| outdated |") || !strings.Contains(summary, "| open |") {
			t.Errorf("summary should show one open and one outdated row:\n%s", summary)
		}
		if comments, _, _ := h.gh.snapshot(); len(comments) != 3 {
			t.Errorf("comments = %d, want 3", len(comments))
		}
		if len(state.Proposals) != 2 {
			t.Errorf("saved proposals = %+v, want 2", state.Proposals)
		}
	})

	t.Run("no impact outdates everything", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), review.NoImpact{Reason: "docs already match"})

		h.push("sha1")
		run, _ := h.push("sha2")

		if run.Conclusion != gate.ConclusionSuccess {
			t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
		}
		for _, marker := range []string{markerA, markerB} {
			if body := h.commentWith(marker).Body; !strings.Contains(body, "Outdated") {
				t.Errorf("comment %s not outdated:\n%s", marker, body)
			}
		}
		summary := h.commentWith(summaryMarker).Body
		if strings.Contains(summary, "| open |") || strings.Count(summary, "| outdated |") != 2 {
			t.Errorf("summary should show two outdated rows:\n%s", summary)
		}
		if comments, _, _ := h.gh.snapshot(); len(comments) != 3 {
			t.Errorf("comments = %d, want 3", len(comments))
		}
	})

	t.Run("recovers after a crash before saving comment IDs", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), twoProposals())

		_, state := h.push("sha1")
		state.SummaryCommentID = 0
		for i := range state.Proposals {
			state.Proposals[i].CommentID = 0
			state.Proposals[i].CommentURL = ""
		}
		if err := h.store.SavePR(t.Context(), state); err != nil {
			t.Fatalf("SavePR() error = %v", err)
		}

		_, state = h.push("sha2")
		comments, creates, edits := h.gh.snapshot()
		if creates != 3 || len(comments) != 3 {
			t.Errorf("creates = %d, comments = %d, want 3 and 3", creates, len(comments))
		}
		if edits != 3 {
			t.Errorf("edits = %d, want 3", edits)
		}
		if state.SummaryCommentID == 0 || state.Proposals[0].CommentID == 0 || state.Proposals[1].CommentID == 0 {
			t.Errorf("saved state did not re-adopt comment IDs: %+v", state)
		}
	})
}

// issueCommentBody is an issue_comment webhook for comment id on PR 1, edited
// from before to after by a user of the given type.
func issueCommentBody(t *testing.T, id int64, userType, before, after string) []byte {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"action":       "edited",
		"changes":      map[string]any{"body": map[string]any{"from": before}},
		"issue":        map[string]any{"number": 1, "pull_request": map[string]any{}},
		"comment":      map[string]any{"id": id, "body": after, "user": map[string]any{"type": userType}},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
	})
	if err != nil {
		t.Fatalf("marshal issue_comment payload: %v", err)
	}
	return body
}

func (h *pushHarness) deliverEvent(event string, body []byte) {
	h.t.Helper()

	h.deliver++
	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("d%d", h.deliver))
	req.Header.Set("X-Hub-Signature-256", sign(h.secret, body))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		h.t.Fatalf("POST /webhook %s = %d, want %d", event, rec.Code, http.StatusAccepted)
	}
}

func TestFailedAnalysisIsRerunFromSummaryCheckbox(t *testing.T) {
	t.Parallel()

	const (
		providerText = "provider said: leak-me"
		rerunBox     = "- [ ] Re-run analysis"
		summaryTag   = "<!-- pollux-agent:summary -->"
	)
	failure := &review.FailedError{Cause: review.CauseProvider, Err: errors.New(providerText)}
	h := newPushHarness(t, failure, review.NoImpact{Reason: "docs already match"})

	run, state := h.push("sha1")
	if run.Conclusion != gate.ConclusionNeutral || run.Title != "Analysis failed" || run.Summary != "The model provider returned an error." {
		t.Fatalf("failed check run = %+v, want neutral \"Analysis failed\" with the fixed provider cause", run)
	}
	if strings.Contains(run.Summary, "leak-me") {
		t.Errorf("check run summary leaks the provider text: %q", run.Summary)
	}
	summary := h.commentWith(summaryTag)
	if !strings.Contains(summary.Body, "The model provider returned an error.") || !strings.HasSuffix(summary.Body, rerunBox+"\n") || strings.Contains(summary.Body, "leak-me") {
		t.Errorf("summary after failure:\n%s\nwant the fixed cause and an unticked Re-run box", summary.Body)
	}
	if state.SummaryCommentID != summary.ID {
		t.Errorf("saved SummaryCommentID = %d, want %d", state.SummaryCommentID, summary.ID)
	}

	ticked := strings.Replace(summary.Body, rerunBox, "- [x] Re-run analysis", 1)
	h.deliverEvent("issue_comment", issueCommentBody(t, summary.ID, "User", summary.Body, summary.Body+"\nedited"))
	h.deliverEvent("issue_comment", issueCommentBody(t, summary.ID+1, "Bot", summary.Body, ticked))
	h.deliverEvent("issue_comment", issueCommentBody(t, summary.ID, "Bot", summary.Body, ticked))

	select {
	case run = <-h.gh.checkRuns:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the re-run's check run")
	}
	if run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("re-run check conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
	}

	deadline := time.After(5 * time.Second)
	for {
		var err error
		if state, err = h.store.LoadPR(t.Context(), "acme", "widgets", 1); err != nil {
			t.Fatalf("LoadPR() error = %v", err)
		}
		if state.CheckRunID == 2 && state.Run == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for the re-run to finish, state = %+v", state)
		case <-time.After(10 * time.Millisecond):
		}
	}

	comments, creates, _ := h.gh.snapshot()
	if creates != 1 || len(comments) != 1 {
		t.Errorf("creates = %d, comments = %d, want the one summary comment edited in place", creates, len(comments))
	}
	if body := h.commentWith(summaryTag).Body; strings.Contains(body, "Analysis failed") || !strings.HasSuffix(body, rerunBox+"\n") {
		t.Errorf("summary after re-run:\n%s\nwant no failure cause and an unticked Re-run box", body)
	}

	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if len(h.gh.concluded) != 2 || h.gh.concluded[0].id != 1 || h.gh.concluded[1].id != 2 {
		t.Errorf("concluded check runs = %+v, want two, on check runs 1 and 2 (the extra edits start nothing)", h.gh.concluded)
	}
}

func checkRunRerequestedBody(t *testing.T) []byte {
	t.Helper()

	return checkRunBody(t, "rerequested", "pollux-agent", 1)
}

func (h *pushHarness) waitState(what string, done func(gate.PRState) bool) gate.PRState {
	h.t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		state, err := h.store.LoadPR(h.t.Context(), "acme", "widgets", 1)
		if err != nil {
			h.t.Fatalf("LoadPR() error = %v", err)
		}
		if done(state) {
			return state
		}
		select {
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s, state = %+v", what, state)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCheckRunRerequestedStartsFreshAnalysis(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, review.NoImpact{Reason: "docs already match"}, review.NoImpact{Reason: "docs already match"})

	if run, _ := h.push("sha1"); run.Conclusion != gate.ConclusionSuccess {
		t.Fatalf("first conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
	}

	h.deliverEvent("check_run", checkRunRerequestedBody(t))

	select {
	case run := <-h.gh.checkRuns:
		if run.Conclusion != gate.ConclusionSuccess {
			t.Errorf("re-run conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the re-run's check run")
	}
	state := h.waitState("the re-run to finish", func(s gate.PRState) bool { return s.CheckRunID == 2 && s.Run == nil })
	if state.HeadSHA != "sha1" {
		t.Errorf("HeadSHA = %q, want sha1", state.HeadSHA)
	}
}

func TestRerunAndPushTogetherEndOnNewestHead(t *testing.T) {
	t.Parallel()

	noImpact := review.NoImpact{Reason: "docs already match"}
	h := newPushHarness(t, noImpact, noImpact, noImpact)
	h.push("sha1")

	h.gh.mu.Lock()
	h.gh.head = "sha2"
	h.gh.mu.Unlock()
	h.deliverEvent("check_run", checkRunRerequestedBody(t))
	h.deliverEvent("pull_request", bytes.Replace(e2ePullRequestBody(t, 1, "sha2"), []byte(`"opened"`), []byte(`"synchronize"`), 1))

	var state gate.PRState
	h.waitState("the newest head to finish", func(s gate.PRState) bool {
		state = s
		h.gh.mu.Lock()
		defer h.gh.mu.Unlock()
		last := h.gh.concluded[len(h.gh.concluded)-1]
		return s.HeadSHA == "sha2" && s.Run == nil && s.CheckRunID > 1 && last.id == s.CheckRunID
	})

	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	seen := map[int64]bool{}
	for _, c := range h.gh.concluded {
		if seen[c.id] {
			t.Errorf("check run %d concluded twice: %+v", c.id, h.gh.concluded)
		}
		seen[c.id] = true
	}
	if last := h.gh.concluded[len(h.gh.concluded)-1]; last.id != state.CheckRunID {
		t.Errorf("last concluded = %+v, want the newest check run %d", last, state.CheckRunID)
	}
}

func TestActionsRerunFailingAgainEditsSummaryInPlace(t *testing.T) {
	t.Parallel()

	const (
		rerunBox   = "- [ ] Re-run analysis"
		summaryTag = "<!-- pollux-agent:summary -->"
		cause      = "The pollux-agent workflow run failed."
	)
	pending := func(runID int64) review.Pending {
		return review.Pending{RunID: runID, Nonce: fmt.Sprintf("n%d", runID), Deadline: time.Now().Add(time.Hour)}
	}
	h := newPushHarness(t, twoProposals(), pending(101), pending(102))
	h.gh.mu.Lock()
	h.gh.workflow = true
	h.gh.mu.Unlock()

	h.push("sha1")

	failRun := func(runID int64, checkRunID int64) gate.CheckRun {
		t.Helper()

		h.waitState(fmt.Sprintf("run %d to be awaited", runID), func(s gate.PRState) bool { return s.Run != nil && s.Run.RunID == runID })
		h.deliverEvent("workflow_run", e2eWorkflowRunCompletedBody(t, runID, "failure"))

		var run gate.CheckRun
		select {
		case run = <-h.gh.checkRuns:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for run %d's check run", runID)
		}
		h.waitState(fmt.Sprintf("run %d to be concluded", runID), func(s gate.PRState) bool { return s.CheckRunID == checkRunID && s.Run == nil })
		return run
	}

	h.deliverEvent("check_run", checkRunRerequestedBody(t))
	failRun(101, 2)

	summary := h.commentWith(summaryTag)
	if !strings.Contains(summary.Body, cause) || !strings.HasSuffix(summary.Body, rerunBox+"\n") {
		t.Fatalf("summary after the first failure:\n%s\nwant the workflow-failure cause and an unticked Re-run box", summary.Body)
	}

	ticked := strings.Replace(summary.Body, rerunBox, "- [x] Re-run analysis", 1)
	h.deliverEvent("issue_comment", issueCommentBody(t, summary.ID, "Bot", summary.Body, ticked))
	run := failRun(102, 3)

	if run.Conclusion != gate.ConclusionNeutral || run.Title != "Analysis failed" || run.Summary != cause {
		t.Errorf("re-run check run = %+v, want neutral \"Analysis failed\" with summary %q", run, cause)
	}
	comments, creates, _ := h.gh.snapshot()
	if creates != 3 || len(comments) != 3 {
		t.Errorf("creates = %d, comments = %d, want 3 and 3 (two proposals and one summary edited in place)", creates, len(comments))
	}
	summaries := 0
	for _, c := range comments {
		if strings.Contains(c.Body, summaryTag) {
			summaries++
		}
	}
	if summaries != 1 {
		t.Errorf("summary comments = %d, want 1", summaries)
	}
	body := h.commentWith(summaryTag).Body
	for _, want := range []string{cause, "`docs/a.md`", "`docs/b.md`", "| open |"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary after the second failure lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(body, rerunBox+"\n") {
		t.Errorf("summary after the second failure:\n%s\nwant it to end with an unticked Re-run box", body)
	}
}

// A push that supersedes a running server analysis closes its check run as
// superseded; the interrupted analysis is not reported as a failure.
func TestSupersededServerAnalysisIsNotReportedFailed(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	h := newPushHarness(t, blockedRun{started: started}, review.NoImpact{Reason: "fine"})

	h.send("sha1")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first analysis to start")
	}
	h.push("sha2")

	h.gh.mu.Lock()
	concluded := slices.Clone(h.gh.concluded)
	h.gh.mu.Unlock()
	if len(concluded) != 2 || concluded[0].id != 1 || concluded[0].run.Title != "Superseded" || concluded[1].id != 2 {
		t.Fatalf("concluded = %+v, want check run 1 superseded, then check run 2", concluded)
	}
	comments, _, _ := h.gh.snapshot()
	for _, c := range comments {
		if strings.Contains(c.Body, "**Analysis failed:**") {
			t.Errorf("comment reports a failure:\n%s", c.Body)
		}
	}
}
