package e2e_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gitfixture"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm/llmtest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

func TestWebhookToCheckRun(t *testing.T) {
	t.Parallel()

	gh := repoGitHub()
	sys := start(t, sqlitetest.Open(t), gh, gate.Runners{})

	first := pullRequestBody(t, 1, "sha1", pushOpts{})
	sys.deliverAs("d1", "pull_request", first)
	run := waitNth(t, "the first check run", gh.CheckRuns, 1)
	if run.Created.HeadSHA != "sha1" {
		t.Errorf("first check run HeadSHA = %q, want %q", run.Created.HeadSHA, "sha1")
	}

	sys.deliverAs("d1", "pull_request", first)

	// A distinct PR acts as a barrier: it runs on a different key, in parallel with any
	// (incorrect) duplicate job, giving the worker a chance to have drained one if it existed.
	sys.deliverAs("d2", "pull_request", pullRequestBody(t, 2, "sha2", pushOpts{}))
	barrier := waitNth(t, "the barrier check run", gh.CheckRuns, 2)
	if barrier.Created.HeadSHA != "sha2" {
		t.Errorf("barrier check run HeadSHA = %q, want %q", barrier.Created.HeadSHA, "sha2")
	}
	if n := len(gh.CheckRuns()); n != 2 {
		t.Errorf("check runs = %d, want 2: the duplicate delivery must not start another", n)
	}
}

const chainPatch = "@@ -1,3 +1,3 @@\n package app\n-// old wording\n+// new wording\n"

// A webhook reaches the server runner, which clones the repo, triages the doc
// covering the changed file with the patch in the prompt, and concludes the check run.
func TestWebhookToServerRunnerConcludesCheckRun(t *testing.T) {
	repoDir, baseSHA := gitfixture.NewRepo(t, map[string]string{
		"src/app.go":  "package app\n\n// old wording\n",
		"docs/app.md": "---\ntitle: App\nsummary: Describes the app.\ncovers:\n  - \"src/**\"\n---\n# App\n\n## Behavior\n\nThe app greets users.\n",
	})
	headSHA := gitfixture.Commit(t, repoDir, map[string]string{"src/app.go": "package app\n\n// new wording\n"}, "reword")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+repoDir+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/acme/widgets.git")

	gh := repoGitHub()
	gh.MergeBaseSHA = baseSHA
	gh.Changed = []review.ChangedFile{{Path: "src/app.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: chainPatch}}
	model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{Text: `{"impacted":false,"reason":"wording only"}`}, nil
		},
	}}
	noToken := func(context.Context, int64, string) (string, error) { return "", nil }
	sys := start(t, sqlitetest.Open(t), gh, gate.Runners{Server: llmrunner.New(model, noToken, "triage", "draft")})

	sys.deliver("pull_request", pullRequestBody(t, 1, headSHA, pushOpts{baseSHA: baseSHA}))

	run := waitConcluded(t, gh, 1)
	if run.Conclusion != gate.ConclusionSuccess || run.Title != "No doc impact" {
		t.Errorf("check run = %q %q, want %q %q", run.Conclusion, run.Title, gate.ConclusionSuccess, "No doc impact")
	}

	if len(model.Calls) != 1 {
		t.Fatalf("model saw %d requests, want 1 triage request", len(model.Calls))
	}
	var seen strings.Builder
	seen.WriteString(model.Calls[0].System)
	for _, m := range model.Calls[0].Messages {
		seen.WriteString(m.Text)
	}
	for _, want := range []string{"docs/app.md", "Describes the app.", chainPatch} {
		if !strings.Contains(seen.String(), want) {
			t.Errorf("triage request does not contain %q:\n%s", want, seen.String())
		}
	}
}

// waitConcluded waits until check run id is completed and returns it.
func waitConcluded(t *testing.T, gh *gatetest.GitHub, id int64) gate.CheckRun {
	t.Helper()

	var run gate.CheckRun
	waitFor(t, "check run to conclude", func() bool {
		for _, cr := range gh.CheckRuns() {
			if cr.ID == id {
				run = cr.Latest()
				return run.Status == gate.StatusCompleted
			}
		}
		return false
	})
	return run
}
