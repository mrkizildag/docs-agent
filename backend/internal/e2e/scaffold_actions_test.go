package e2e_test

import (
	"context"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

const scaffoldTip = "tip1"

func noToken(context.Context, int64, string) (string, error) { return "", nil }

type actionsScaffold struct {
	api   *githubAPI
	store *sqlite.Store
	sys   *system
}

// newActionsScaffold serves a repo with the workflow and no docs/, whose
// default branch is at scaffoldTip; server, when not nil, is configured next to
// the Actions runner.
func newActionsScaffold(t *testing.T, server gate.ServerRunner) *actionsScaffold {
	t.Helper()

	api := newGitHubAPI(t, apiConfig{
		nextCheckRun: 101,
		branches:     map[string]string{"main": scaffoldTip},
		result: func(inputs map[string]any) any {
			return map[string]any{
				"head_sha": scaffoldTip,
				"nonce":    inputs["nonce"],
				"claude": map[string]any{
					"is_error":          false,
					"structured_output": map[string]string{"index": scaffoldIndex, "architecture": scaffoldArchitecture, "setup": scaffoldSetup},
				},
			}
		},
	})
	store := sqlitetest.Open(t)
	sys := start(t, store, api.client, gate.Runners{Actions: actions.New(api.client, gate.AnalysisDeadline), Server: server})
	return &actionsScaffold{api: api, store: store, sys: sys}
}

// waitState returns the scaffold state once ok holds.
func (h *actionsScaffold) waitState(what string, ok func(gate.ScaffoldState) bool) gate.ScaffoldState {
	h.sys.t.Helper()

	var state gate.ScaffoldState
	waitFor(h.sys.t, what, func() bool {
		state = scaffoldState(h.sys.t, h.store)
		return ok(state)
	})
	return state
}

func checkRunOutput(body map[string]any) (title, summary string) {
	output, _ := body["output"].(map[string]any)
	title, _ = output["title"].(string)
	summary, _ = output["summary"].(string)
	return title, summary
}

func awaiting(s gate.ScaffoldState) bool { return s.Phase == gate.ScaffoldAwaiting && s.Run != nil }

func TestWebhookToActionsScaffoldPullRequest(t *testing.T) {
	t.Parallel()

	h := newActionsScaffold(t, nil)
	testActionsScaffoldPullRequest(t, h)
}

func TestWebhookToActionsScaffoldPreferredOverServerRunner(t *testing.T) {
	t.Parallel()

	model := scriptedModel()
	h := newActionsScaffold(t, llmrunner.New(model, noToken, "triage", "draft"))
	testActionsScaffoldPullRequest(t, h)
	if err := h.sys.stop(); err != nil {
		t.Fatalf("worker.Run() error = %v", err)
	}
	if len(model.Calls) != 0 {
		t.Errorf("server model was called %d times, want the repo's workflow to write the scaffold", len(model.Calls))
	}
}

func testActionsScaffoldPullRequest(t *testing.T, h *actionsScaffold) {
	t.Helper()

	h.sys.deliver("pull_request", pullRequestBody(t, 1, "pr1sha", pushOpts{}))
	created := waitNth(t, "the waiting check run", h.api.checkRunsCreated, 1)
	if title, summary := checkRunOutput(created); created["conclusion"] != "neutral" || title != "No docs/ folder" || !strings.Contains(summary, "writing") {
		t.Errorf("created check run = %v, want neutral \"No docs/ folder\" saying the folder is being written", created)
	}

	dispatched := waitNth(t, "the workflow dispatch", h.api.dispatches, 1)
	inputs, _ := dispatched["inputs"].(map[string]any)
	if inputs["pr_number"] != "0" || inputs["head_sha"] != scaffoldTip || inputs["nonce"] == "" {
		t.Errorf("dispatch inputs = %v, want pr_number 0, head_sha %s and a nonce", inputs, scaffoldTip)
	}
	state := h.waitState("the awaited run", awaiting)
	if state.Run.RunID != 4242 || state.BaseSHA != scaffoldTip {
		t.Fatalf("awaiting state = %+v, want run 4242 started from %s", state, scaffoldTip)
	}

	h.sys.deliver("workflow_run", workflowRunBody(t, 4242, "success"))
	linked := waitNth(t, "the link in the waiting check run", h.api.checkRunsUpdated, 1)
	title, summary := checkRunOutput(linked)
	const prURL = "https://github.com/acme/widgets/pull/9"
	if linked["id"] != "101" || linked["conclusion"] != "neutral" || title != "No docs/ folder" || !strings.Contains(summary, prURL) {
		t.Errorf("updated check run = %v, want check run 101 neutral \"No docs/ folder\" linking %s", linked, prURL)
	}

	if n := h.api.branchCreations(); n != 1 {
		t.Errorf("branch creations = %d, want 1", n)
	}
	wantFiles := map[string]string{"docs/README.md": scaffoldIndex, "docs/architecture.md": scaffoldArchitecture, "docs/guides/setup.md": scaffoldSetup}
	if diff := gocmp.Diff([]map[string]string{wantFiles}, h.api.commitFiles()); diff != "" {
		t.Errorf("commits (-want +got):\n%s", diff)
	}
	prs := h.api.pullRequests()
	if len(prs) != 1 {
		t.Fatalf("pull requests created = %d, want 1", len(prs))
	}
	if pr := prs[0]; pr["head"] != scaffoldBranch || pr["base"] != "main" {
		t.Errorf("pull request = %v, want %s into main", pr, scaffoldBranch)
	}
}

func TestWebhookToActionsScaffoldRunFailure(t *testing.T) {
	t.Parallel()

	h := newActionsScaffold(t, nil)

	h.sys.deliver("pull_request", pullRequestBody(t, 1, "pr1sha", pushOpts{}))
	waitNth(t, "the first dispatch", h.api.dispatches, 1)
	h.waitState("the awaited run", awaiting)

	h.sys.deliver("workflow_run", workflowRunBody(t, 4242, "failure"))
	failed := h.waitState("the failed attempt", func(s gate.ScaffoldState) bool { return s.Phase == gate.ScaffoldIdle && s.Attempt == 1 })
	if failed.Run != nil {
		t.Errorf("state after the failed run = %+v, want Idle, attempt 1, no run", failed)
	}

	h.sys.deliver("pull_request", pullRequestBody(t, 2, "pr2sha", pushOpts{}))
	dispatched := waitNth(t, "the second dispatch", h.api.dispatches, 2)
	if inputs, _ := dispatched["inputs"].(map[string]any); inputs["pr_number"] != "0" {
		t.Errorf("second dispatch inputs = %v, want pr_number 0", inputs)
	}
	retrying := h.waitState("the second awaited run", func(s gate.ScaffoldState) bool { return awaiting(s) && s.Run.RunID == 4243 })
	if retrying.Run.RunID != 4243 {
		t.Errorf("state after the second dispatch = %+v, want run 4243", retrying)
	}

	if prs, creations, commits := h.api.pullRequests(), h.api.branchCreations(), h.api.commitFiles(); len(prs) != 0 || creations != 0 || len(commits) != 0 {
		t.Errorf("after a failed run: %d pull requests, %d branch creations, %d commits, want none", len(prs), creations, len(commits))
	}
}
