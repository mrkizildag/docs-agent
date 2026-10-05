package httpapi_test

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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

	gocmp "github.com/google/go-cmp/cmp"

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

// noComments is the comment surface of a fake that never posts comments.
type noComments struct{}

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

	return e2ePullRequestFrom(t, number, sha, pushOpts{})
}

// pushOpts varies a pull_request delivery; zero values mean a push by user dev
// from a branch of the base repository.
type pushOpts struct {
	headRepo   string
	sender     string
	senderType string
}

func e2ePullRequestFrom(t *testing.T, number int, sha string, o pushOpts) []byte {
	t.Helper()

	o.headRepo = cmp.Or(o.headRepo, "acme/widgets")
	o.sender = cmp.Or(o.sender, "dev")
	o.senderType = cmp.Or(o.senderType, "User")
	payload := map[string]any{
		"action": "opened",
		"number": number,
		"pull_request": map[string]any{
			"head": map[string]any{"sha": sha, "ref": "feature", "repo": map[string]any{"full_name": o.headRepo}},
		},
		"repository": map[string]any{
			"name":      "widgets",
			"full_name": "acme/widgets",
			"owner":     map[string]any{"login": "acme"},
		},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": o.sender, "type": o.senderType},
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

	body, err := json.Marshal(map[string]any{
		"action": "completed",
		"workflow_run": map[string]any{
			"id": 4242, "path": ".github/workflows/pollux-agent.yml", "conclusion": "success",
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
	select {
	case saved := <-store.saved:
		if saved.Run == nil || saved.Run.RunID != 4242 || saved.CheckRunID != 555 {
			t.Errorf("saved state = %+v, want awaiting run 4242 with check run 555", saved)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for awaited run to be saved")
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
	edited    chan string
	created   int64
}

func (f *commentGitHub) WorkflowExists(context.Context, int64, string, string) (bool, error) {
	return false, nil
}

func (f *commentGitHub) UpdateCheckRun(context.Context, int64, string, string, int64, gate.CheckRun) error {
	return nil
}

func (f *commentGitHub) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *commentGitHub) CreateCheckRun(_ context.Context, _ int64, _, _ string, run gate.CheckRun) (int64, error) {
	f.checkRuns <- run
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

func (f *commentGitHub) EditIssueComment(_ context.Context, _ int64, _, _ string, id int64, body string) error {
	if id != 900 {
		return fmt.Errorf("edit issue comment %d: not found", id)
	}
	f.edited <- body
	return nil
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

	gh := &commentGitHub{checkRuns: make(chan gate.CheckRun, 2), review: make(chan gate.ReviewComment, 4), issue: make(chan string, 2), edited: make(chan string, 2)}
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
	if strings.Contains(summary, "discussion_r") {
		t.Errorf("created summary should not link comments yet:\n%s", summary)
	}
	select {
	case summary = <-gh.edited:
	case <-timeout:
		t.Fatal("timed out waiting for summary edit with comment links")
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
	checkRuns chan gate.CheckRun

	mu       sync.Mutex
	comments []gate.Comment
	creates  int
	edits    int

	files           map[string]string
	permissionCalls int
	commits         []commitCall
	replies         []replyCall
	runs            []gate.CheckRun

	nextReaction int64
	reactions    map[reactionKey]map[int64]gate.Reaction
}

type reactionKey struct {
	kind gate.CommentKind
	id   int64
}

// reactionsOn returns the reactions currently on the comment.
func (f *statefulGitHub) reactionsOn(kind gate.CommentKind, id int64) []gate.Reaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	var got []gate.Reaction
	for _, r := range f.reactions[reactionKey{kind, id}] {
		got = append(got, r)
	}
	return got
}

type commitCall struct {
	Branch, Parent, Message string
	Files                   []gate.FileChange
}

type replyCall struct {
	InReplyTo int64
	Body      string
}

func (f *statefulGitHub) Permission(_ context.Context, _ int64, _, _, user string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permissionCalls++
	return user == "dev", nil
}

func (f *statefulGitHub) FileAtRef(_ context.Context, _ int64, _, _, path, _ string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[path]
	return []byte(content), ok, nil
}

func (f *statefulGitHub) CommitFiles(_ context.Context, _ int64, _, _, branch, parentSHA string, files []gate.FileChange, message string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits = append(f.commits, commitCall{Branch: branch, Parent: parentSHA, Files: files, Message: message})
	return "commit1234567890", nil
}

func (f *statefulGitHub) ReplyToReviewComment(_ context.Context, _ int64, _, _ string, _ int, inReplyTo int64, body string) (gate.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, replyCall{InReplyTo: inReplyTo, Body: body})
	return gate.Comment{ID: 500 + int64(len(f.replies)), Kind: gate.CommentKindReview}, nil
}

func (f *statefulGitHub) WorkflowExists(context.Context, int64, string, string) (bool, error) {
	return false, nil
}

func (f *statefulGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, _ int64, run gate.CheckRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run)
	return nil
}

func (f *statefulGitHub) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return nil, nil
}

func (f *statefulGitHub) CreateCheckRun(_ context.Context, _ int64, _, _ string, run gate.CheckRun) (int64, error) {
	f.mu.Lock()
	f.runs = append(f.runs, run)
	f.mu.Unlock()
	f.checkRuns <- run
	return 1, nil
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

// scriptedRunner returns one queued verdict per run.
type scriptedRunner struct{ verdicts chan review.Verdict }

func (r scriptedRunner) Start(context.Context, review.Request) (review.Started, error) {
	return review.Result{Runner: "fake", Verdict: <-r.verdicts}, nil
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
	queued  chan review.Verdict
}

func newPushHarness(t *testing.T, verdicts ...review.Verdict) *pushHarness {
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

	queued := make(chan review.Verdict, len(verdicts))
	for _, v := range verdicts {
		queued <- v
	}
	gh := &statefulGitHub{checkRuns: make(chan gate.CheckRun, len(verdicts)+8), files: map[string]string{}}
	gateSvc := gate.NewService(gh, store, gate.Runners{Server: scriptedRunner{verdicts: queued}}).WithComments(gh)

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
	return &pushHarness{t: t, gh: gh, store: store, handler: httpapi.NewHandler(logger, secret, worker, store), secret: secret, queued: queued}
}

// push delivers a synchronize webhook for sha and returns the check run and the
// saved state once the run has finished.
func (h *pushHarness) push(sha string) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	return h.pushWith(sha, pushOpts{})
}

func (h *pushHarness) pushWith(sha string, o pushOpts) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	h.deliver++
	body := bytes.Replace(e2ePullRequestFrom(h.t, 1, sha, o), []byte(`"opened"`), []byte(`"synchronize"`), 1)
	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("d%d", h.deliver))
	req.Header.Set("X-Hub-Signature-256", sign(h.secret, body))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		h.t.Fatalf("POST /webhook for %s = %d, want %d", sha, rec.Code, http.StatusAccepted)
	}

	var run gate.CheckRun
	select {
	case run = <-h.gh.checkRuns:
	case <-time.After(5 * time.Second):
		h.t.Fatalf("timed out waiting for check run on %s", sha)
	}

	// The check run is created before comments are written and state is saved.
	deadline := time.After(5 * time.Second)
	for {
		state, err := h.store.LoadPR(h.t.Context(), "acme", "widgets", 1)
		if err != nil {
			h.t.Fatalf("LoadPR() error = %v", err)
		}
		if state.HeadSHA == sha {
			return run, state
		}
		select {
		case <-deadline:
			h.t.Fatalf("timed out waiting for state on %s", sha)
		case <-time.After(10 * time.Millisecond):
		}
	}
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

// tick delivers a pull_request_review_comment edited webhook in which sender
// ticks the checkbox of comment id.
func (h *pushHarness) tick(deliveryID, sender string, id int64, unticked string) {
	h.t.Helper()

	body, err := json.Marshal(map[string]any{
		"action":       "edited",
		"changes":      map[string]any{"body": map[string]any{"from": unticked}},
		"comment":      map[string]any{"id": id, "body": strings.Replace(unticked, "- [ ]", "- [x]", 1)},
		"pull_request": map[string]any{"number": 1},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": sender, "type": "User"},
	})
	if err != nil {
		h.t.Fatalf("marshal comment payload: %v", err)
	}
	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request_review_comment")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", sign(h.secret, body))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		h.t.Fatalf("POST /webhook for %s = %d, want %d", deliveryID, rec.Code, http.StatusAccepted)
	}
}

func (h *pushHarness) waitFor(what string, done func() bool) {
	h.t.Helper()

	deadline := time.After(5 * time.Second)
	for !done() {
		select {
		case <-deadline:
			h.t.Fatalf("timed out waiting for %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestWebhookApplyCommitsOnceEndToEnd(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals())
	h.gh.files["docs/a.md"] = "# A\n\n## Usage\nold\n\n## Other\nx\n"
	_, state := h.push("sha1")
	idA := gate.ProposalID("docs/a.md", "Usage")
	commentA := h.commentWith("<!-- pollux-agent:proposal:" + idA + " -->")

	h.tick("t1", "dev", commentA.ID, commentA.Body)
	h.waitFor("applied proposal with reply", func() bool {
		st, err := h.store.LoadPR(t.Context(), "acme", "widgets", 1)
		if err != nil {
			t.Fatalf("LoadPR() error = %v", err)
		}
		return len(st.Proposals) > 0 && st.Proposals[0].ReplyID != 0
	})
	h.waitFor("proposal comment reacted done", func() bool {
		return gocmp.Equal(h.gh.reactionsOn(gate.CommentKindReview, commentA.ID), []gate.Reaction{gate.ReactionDone})
	})

	h.gh.mu.Lock()
	wantCommit := []commitCall{{
		Branch: "feature", Parent: "sha1", Message: "docs: apply pollux-agent proposal for docs/a.md § Usage",
		Files: []gate.FileChange{{Path: "docs/a.md", Content: "# A\n\n## Usage\nnew\n\n## Other\nx\n"}},
	}}
	wantReply := []replyCall{{InReplyTo: commentA.ID, Body: "✅ Applied in commit1"}}
	if diff := gocmp.Diff(wantCommit, h.gh.commits); diff != "" {
		t.Errorf("commits (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(wantReply, h.gh.replies); diff != "" {
		t.Errorf("replies (-want +got):\n%s", diff)
	}
	h.gh.mu.Unlock()

	got, err := h.store.LoadPR(t.Context(), "acme", "widgets", 1)
	if err != nil {
		t.Fatalf("LoadPR() error = %v", err)
	}
	if p := got.Proposals[0]; p.State != gate.ProposalApplied || p.AppliedSHA != "commit1234567890" || p.ReplyID != 501 {
		t.Errorf("proposal = %+v, want applied at commit1234567890 with reply 501", p)
	}
	if other := state.Proposals[1].State; got.Proposals[1].State != other {
		t.Errorf("other proposal state = %s, want unchanged %s", got.Proposals[1].State, other)
	}

	// Jobs on the PR run in order, so the third call to Permission starts after
	// the redelivered job has finished.
	h.tick("t2", "dev", commentA.ID, commentA.Body)
	h.tick("t3", "dev", commentA.ID, commentA.Body)
	h.waitFor("third comment job", func() bool {
		h.gh.mu.Lock()
		defer h.gh.mu.Unlock()
		return h.gh.permissionCalls >= 3
	})
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if len(h.gh.commits) != 1 || len(h.gh.replies) != 1 {
		t.Errorf("after redelivery: %d commits, %d replies, want 1, 1", len(h.gh.commits), len(h.gh.replies))
	}
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
		if edits != 4 {
			t.Errorf("edits = %d, want 4 (two review comments and the summary, which is also edited once to add links on its first run)", edits)
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
		if edits != 4 {
			t.Errorf("edits = %d, want 4", edits)
		}
		if state.SummaryCommentID == 0 || state.Proposals[0].CommentID == 0 || state.Proposals[1].CommentID == 0 {
			t.Errorf("saved state did not re-adopt comment IDs: %+v", state)
		}
	})
}

func (f *statefulGitHub) BranchCommit(context.Context, int64, string, string, string) (gate.Commit, error) {
	return gate.Commit{}, nil
}

const (
	e2eSummaryMarker = "<!-- pollux-agent:summary -->"
	e2eAppliedSHA    = "commit1234567890"
)

func (h *pushHarness) post(event string, payload map[string]any) {
	h.t.Helper()

	payload["installation"] = map[string]any{"id": 42}
	payload["repository"] = map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}}
	body, err := json.Marshal(payload)
	if err != nil {
		h.t.Fatalf("marshal %s payload: %v", event, err)
	}
	h.deliver++
	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("x%d", h.deliver))
	req.Header.Set("X-Hub-Signature-256", sign(h.secret, body))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		h.t.Fatalf("POST /webhook %s = %d, want %d", event, rec.Code, http.StatusAccepted)
	}
}

// issueComment delivers an issue_comment webhook; from is the previous body of an edit.
func (h *pushHarness) issueComment(action, sender, senderType string, id int64, body, from string) {
	h.t.Helper()

	payload := map[string]any{
		"action":  action,
		"comment": map[string]any{"id": id, "body": body},
		"issue":   map[string]any{"number": 1, "pull_request": map[string]any{}},
		"sender":  map[string]any{"login": sender, "type": senderType},
	}
	if from != "" {
		payload["changes"] = map[string]any{"body": map[string]any{"from": from}}
	}
	h.post("issue_comment", payload)
}

func (h *pushHarness) command(sender, text string) {
	h.t.Helper()

	h.issueComment("created", sender, "User", 100, text, "")
}

// tickSummary delivers the edit in which sender ticks label on the summary comment.
func (h *pushHarness) tickSummary(sender, senderType, label string) {
	h.t.Helper()

	summary := h.commentWith(e2eSummaryMarker)
	h.issueComment("edited", sender, senderType, summary.ID, strings.Replace(summary.Body, "- [ ] "+label, "- [x] "+label, 1), summary.Body)
}

func (h *pushHarness) waitState(what string, done func(gate.PRState) bool) gate.PRState {
	h.t.Helper()

	var state gate.PRState
	h.waitFor(what, func() bool {
		var err error
		if state, err = h.store.LoadPR(h.t.Context(), "acme", "widgets", 1); err != nil {
			h.t.Fatalf("LoadPR() error = %v", err)
		}
		return done(state)
	})
	return state
}

func (h *pushHarness) lastRun() gate.CheckRun {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	return h.gh.runs[len(h.gh.runs)-1]
}

// drainRuns discards the check runs the channel holds, which push would otherwise take for its own.
func (h *pushHarness) drainRuns() {
	for {
		select {
		case <-h.gh.checkRuns:
		default:
			return
		}
	}
}

func (h *pushHarness) allReplied(state gate.PRState) bool {
	for _, p := range state.Proposals {
		if p.State == gate.ProposalApplied && p.ReplyID == 0 {
			return false
		}
	}
	return slices.ContainsFunc(state.Proposals, func(p gate.ProposalState) bool { return p.State == gate.ProposalApplied })
}

func newApplyHarness(t *testing.T, extra ...review.Verdict) *pushHarness {
	t.Helper()

	h := newPushHarness(t, append([]review.Verdict{twoProposals()}, extra...)...)
	h.gh.files["docs/a.md"] = "# A\n\n## Usage\nold\n\n## Other\nx\n"
	h.push("sha1")
	return h
}

// requireAppliedAll checks that one commit applied both proposals and every
// surface shows it.
func (h *pushHarness) requireAppliedAll() {
	h.t.Helper()

	state := h.waitState("both proposals applied with replies", h.allReplied)
	h.gh.mu.Lock()
	wantCommits := []commitCall{{
		Branch: "feature", Parent: "sha1", Message: "docs: apply 2 pollux-agent proposals",
		Files: []gate.FileChange{
			{Path: "docs/a.md", Content: "# A\n\n## Usage\nnew\n\n## Other\nx\n"},
			{Path: "docs/b.md", Content: "# B\n"},
			{Path: "docs/README.md", Content: "- [B](b.md)\n"},
		},
	}}
	wantReplies := []replyCall{{InReplyTo: 2, Body: "✅ Applied in commit1"}, {InReplyTo: 3, Body: "✅ Applied in commit1"}}
	if diff := gocmp.Diff(wantCommits, h.gh.commits); diff != "" {
		h.t.Errorf("commits (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(wantReplies, h.gh.replies); diff != "" {
		h.t.Errorf("replies (-want +got):\n%s", diff)
	}
	h.gh.mu.Unlock()

	for _, p := range state.Proposals {
		if p.State != gate.ProposalApplied || p.AppliedSHA != e2eAppliedSHA {
			h.t.Errorf("proposal %s = %s at %q, want applied at %s", p.ID, p.State, p.AppliedSHA, e2eAppliedSHA)
		}
		if body := h.commentWith(proposalMarker(p.ID)).Body; !strings.Contains(body, "- [x] Apply this change") {
			h.t.Errorf("proposal comment not ticked:\n%s", body)
		}
	}
	summary := h.commentWith(e2eSummaryMarker).Body
	if strings.Count(summary, "applied (commit1)") != 2 || !strings.Contains(summary, "✅ All proposals applied.") || strings.Contains(summary, "Apply all") {
		h.t.Errorf("summary should show both proposals applied and the all-applied status line:\n%s", summary)
	}
}

func proposalMarker(id string) string { return "<!-- pollux-agent:proposal:" + id + " -->" }

func TestWebhookApplyAllEndToEnd(t *testing.T) {
	t.Parallel()

	t.Run("summary tick", func(t *testing.T) {
		t.Parallel()
		h := newApplyHarness(t)
		h.tickSummary("dev", "User", "Apply all")
		h.requireAppliedAll()
	})

	t.Run("command", func(t *testing.T) {
		t.Parallel()
		h := newApplyHarness(t)
		h.command("dev", "/pollux-agent apply")
		h.requireAppliedAll()
	})
}

func TestWebhookBotCommitIsAnalyzedOnceEndToEnd(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, review.NoImpact{Reason: "docs already match"})
	h.tickSummary("dev", "User", "Apply all")
	h.requireAppliedAll()

	run, state := h.pushWith(e2eAppliedSHA, pushOpts{sender: "pollux-agent[bot]", senderType: "Bot"})

	if run.Conclusion != gate.ConclusionSuccess || run.HeadSHA != e2eAppliedSHA {
		t.Errorf("check run for the bot's commit = %s on %s, want success on %s", run.Conclusion, run.HeadSHA, e2eAppliedSHA)
	}
	if len(h.queued) != 0 {
		t.Errorf("%d scripted verdicts left, want the bot's push analyzed once", len(h.queued))
	}
	for _, p := range state.Proposals {
		if p.State != gate.ProposalApplied {
			t.Errorf("proposal %s = %s after the bot's push, want applied", p.ID, p.State)
		}
	}
}

func TestWebhookSkipCommitEndToEnd(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals(), twoProposals())
	h.push("sha1")

	h.tickSummary("dev", "User", "Skip this commit")
	state := h.waitState("pending skip", func(s gate.PRState) bool { return s.PendingSkip != nil })
	if want := (gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}); *state.PendingSkip != want {
		t.Errorf("pending skip = %+v, want %+v", *state.PendingSkip, want)
	}
	comments, _, _ := h.gh.snapshot()
	if ask := comments[len(comments)-1].Body; !strings.Contains(ask, "@dev") || !strings.Contains(ask, "reason") {
		t.Errorf("bot's question = %q, want it to ask @dev for a reason", ask)
	}
	if got := h.lastRun(); got.Conclusion == gate.ConclusionSuccess {
		t.Fatalf("check run succeeded before the reason arrived: %+v", got)
	}

	h.command("dev", "docs are generated for this one")
	state = h.waitState("skip recorded", func(s gate.PRState) bool { return s.Skip != nil })
	if want := (gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "docs are generated for this one", HeadSHA: "sha1"}); *state.Skip != want || state.PendingSkip != nil {
		t.Errorf("skip = %+v, pending = %+v, want %+v and no pending ask", *state.Skip, state.PendingSkip, want)
	}
	run := h.lastRun()
	if run.Conclusion != gate.ConclusionSuccess || run.HeadSHA != "sha1" ||
		!strings.Contains(run.Title, "dev") || !strings.Contains(run.Summary, "commit") || !strings.Contains(run.Summary, "docs are generated for this one") {
		t.Errorf("skip check run = %+v, want success naming dev, commit scope and the reason", run)
	}
	if summary := h.commentWith(e2eSummaryMarker).Body; !strings.Contains(summary, "Skipped by @dev for this commit: docs are generated for this one") {
		t.Errorf("summary does not show the skip:\n%s", summary)
	}

	h.drainRuns()
	next, _ := h.push("sha2")
	if next.Conclusion != gate.ConclusionActionRequired || next.HeadSHA != "sha2" {
		t.Errorf("check run for the next push = %s on %s, want action_required on sha2", next.Conclusion, next.HeadSHA)
	}
}

func TestWebhookSkipPREndToEnd(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals(), twoProposals())
	h.push("sha1")

	h.command("dev", "/pollux-agent skip-pr vendored docs")
	h.waitState("skip recorded", func(s gate.PRState) bool { return s.Skip != nil })
	run := h.lastRun()
	if run.Conclusion != gate.ConclusionSuccess || !strings.Contains(run.Summary, "vendored docs") || !strings.Contains(run.Summary, "PR") {
		t.Errorf("skip-pr check run = %+v, want success naming the PR scope and the reason", run)
	}

	h.drainRuns()
	next, state := h.push("sha2")
	if next.Conclusion != gate.ConclusionSuccess || next.HeadSHA != "sha2" || !strings.Contains(next.Summary, "vendored docs") {
		t.Errorf("check run for the later push = %+v, want the skip's success on sha2", next)
	}
	if len(h.queued) != 1 {
		t.Errorf("%d scripted verdicts left, want 1: the later push must not call the runner", len(h.queued))
	}
	if state.Skip == nil || state.Skip.Scope != gate.SkipPR {
		t.Errorf("skip after the later push = %+v, want it carried over", state.Skip)
	}
}

func TestWebhookWithoutWriteAccessEndToEnd(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t)
	_, creates, _ := h.gh.snapshot()

	h.command("stranger", "/pollux-agent apply")
	h.waitFor("reply", func() bool {
		_, n, _ := h.gh.snapshot()
		return n > creates
	})

	comments, n, _ := h.gh.snapshot()
	if n != creates+1 {
		t.Errorf("comments created = %d, want exactly one reply", n-creates)
	}
	if reply := comments[len(comments)-1].Body; !strings.Contains(reply, "@stranger") || !strings.Contains(reply, "write access") {
		t.Errorf("reply = %q, want it to tell @stranger about write access", reply)
	}
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if len(h.gh.commits) != 0 || len(h.gh.replies) != 0 {
		t.Errorf("%d commits and %d thread replies, want none", len(h.gh.commits), len(h.gh.replies))
	}
}

func TestWebhookForkPullRequestEndToEnd(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals())
	h.gh.files["docs/a.md"] = "# A\n\n## Usage\nold\n\n## Other\nx\n"
	_, state := h.pushWith("sha1", pushOpts{headRepo: "forker/widgets"})

	for _, p := range state.Proposals {
		body := h.commentWith(proposalMarker(p.ID)).Body
		if strings.Contains(body, "- [ ] Apply this change") || !strings.Contains(body, "Apply is not available") {
			t.Errorf("fork proposal comment should explain instead of offering Apply:\n%s", body)
		}
	}
	if summary := h.commentWith(e2eSummaryMarker).Body; !strings.Contains(summary, "Apply all is not available") || strings.Contains(summary, "- [ ] Apply all") {
		t.Errorf("fork summary should say why Apply all is missing:\n%s", summary)
	}

	_, creates, _ := h.gh.snapshot()
	h.command("dev", "/pollux-agent apply")
	h.waitFor("reply", func() bool {
		_, n, _ := h.gh.snapshot()
		return n > creates
	})
	comments, _, _ := h.gh.snapshot()
	if reply := comments[len(comments)-1].Body; !strings.Contains(reply, "@dev") || !strings.Contains(reply, "fork") {
		t.Errorf("reply = %q, want it to tell @dev about the fork", reply)
	}
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if len(h.gh.commits) != 0 {
		t.Errorf("commits = %+v, want none on a fork", h.gh.commits)
	}
}

func TestWebhookIgnoredEditsEndToEnd(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, review.NoImpact{Reason: "barrier"})
	summary := h.commentWith(e2eSummaryMarker)
	commentA := h.commentWith(proposalMarker(gate.ProposalID("docs/a.md", "Usage")))

	h.tickSummary("pollux-agent[bot]", "Bot", "Apply all")
	h.issueComment("edited", "dev", "User", summary.ID, summary.Body, strings.Replace(summary.Body, "- [ ] Apply all", "- [x] Apply all", 1))
	h.post("pull_request_review_comment", map[string]any{
		"action":       "edited",
		"changes":      map[string]any{"body": map[string]any{"from": commentA.Body}},
		"comment":      map[string]any{"id": commentA.ID, "body": strings.Replace(commentA.Body, "- [ ]", "- [x]", 1)},
		"pull_request": map[string]any{"number": 1},
		"sender":       map[string]any{"login": "pollux-agent[bot]", "type": "Bot"},
	})

	// Jobs on a PR run in order, so a finished push means the edits above were handled.
	h.pushWith("sha2", pushOpts{})
	state := h.waitState("barrier push saved", func(s gate.PRState) bool { return s.HeadSHA == "sha2" })

	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if h.gh.permissionCalls != 0 || len(h.gh.commits) != 0 || len(h.gh.replies) != 0 {
		t.Errorf("permission checks = %d, commits = %d, replies = %d, want none from ignored edits",
			h.gh.permissionCalls, len(h.gh.commits), len(h.gh.replies))
	}
	for _, p := range state.Proposals {
		if p.State == gate.ProposalApplied {
			t.Errorf("proposal %s applied by an ignored edit", p.ID)
		}
	}
}

func (f *statefulGitHub) React(_ context.Context, _ int64, _, _ string, kind gate.CommentKind, id int64, reaction gate.Reaction) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := reactionKey{kind, id}
	for rid, r := range f.reactions[key] {
		if r == reaction {
			return rid, nil
		}
	}
	if f.reactions == nil {
		f.reactions = map[reactionKey]map[int64]gate.Reaction{}
	}
	if f.reactions[key] == nil {
		f.reactions[key] = map[int64]gate.Reaction{}
	}
	f.nextReaction++
	f.reactions[key][f.nextReaction] = reaction
	return f.nextReaction, nil
}

func (f *statefulGitHub) Unreact(_ context.Context, _ int64, _, _ string, kind gate.CommentKind, id, reactionID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.reactions[reactionKey{kind, id}], reactionID)
	return nil
}
