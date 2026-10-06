package e2e_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
)

// A webhook dispatches the repo's workflow through the GitHub API, waits for
// the workflow_run webhook, reads back the result artifact, and concludes the
// check run with the proposal.
func TestWebhookToActionsRunnerConcludesCheckRun(t *testing.T) {
	t.Parallel()

	api := newGitHubAPI(t, apiConfig{
		docs:         true,
		nextCheckRun: 555,
		result: func(inputs map[string]any) any {
			return map[string]any{
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
			}
		},
	})
	store := sqlitetest.Open(t)
	sys := start(t, store, api.client, gate.Runners{Actions: actions.New(api.client, gate.AnalysisDeadline)})

	sys.deliver("pull_request", pullRequestBody(t, 1, "sha1", pushOpts{}))

	created := waitNth(t, "check run create", api.checkRunsCreated, 1)
	if created["status"] != "in_progress" || created["conclusion"] != nil || created["head_sha"] != "sha1" {
		t.Errorf("created check run = %v, want in_progress on sha1 with no conclusion", created)
	}
	var saved gate.PRState
	waitFor(t, "the awaited run to be saved", func() bool {
		var err error
		if saved, err = store.LoadPR(t.Context(), "acme", "widgets", 1); err != nil {
			t.Fatalf("LoadPR() error = %v", err)
		}
		return saved.Run != nil && saved.Run.RunID != 0
	})
	if saved.Run.RunID != 4242 || saved.CheckRunID != 555 {
		t.Errorf("saved state = %+v, want awaiting run 4242 with check run 555", saved)
	}

	dispatched := waitNth(t, "the workflow dispatch", api.dispatches, 1)
	inputs, _ := dispatched["inputs"].(map[string]any)
	if dispatched["ref"] != "main" || dispatched["return_run_details"] != true ||
		inputs["head_sha"] != "sha1" || inputs["pr_number"] != "1" || inputs["nonce"] == "" {
		t.Errorf("dispatch body = %v, want ref main, return_run_details, and head sha1, PR 1, a nonce", dispatched)
	}

	sys.deliver("workflow_run", workflowRunBody(t, 4242, "success"))

	updated := waitNth(t, "check run update", api.checkRunsUpdated, 1)
	if updated["status"] != "completed" || updated["conclusion"] != "action_required" {
		t.Errorf("updated check run = %v, want completed action_required", updated)
	}
	output, _ := updated["output"].(map[string]any)
	if summary, _ := output["summary"].(string); !strings.Contains(summary, "docs/features/greeting.md") {
		t.Errorf("updated check run output = %v, want the proposal for docs/features/greeting.md", output)
	}
}
