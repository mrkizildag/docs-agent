package llmrunner_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

// A nested file matched by a ** covers glob reaches triage with its doc and
// patch; a doc whose covers do not match is not triaged.
func TestStart_GlobCoveredNestedFileTriagesOnlyItsDoc(t *testing.T) {
	t.Parallel()

	dir := initGitRepo(t)
	writeRepoFile(t, dir, "src/pkg/deep/x.go", "package deep\n\nfunc X() {}\n")
	writeRepoFile(t, dir, "other/y.go", "package other\n")
	writeRepoFile(t, dir, "docs/a.md", "---\ntitle: A\nsummary: Describes A.\ncovers:\n  - src/**/*.go\n---\n# A\n\nold.\n")
	writeRepoFile(t, dir, "docs/b.md", "---\ntitle: B\nsummary: Describes B.\ncovers:\n  - other/*.go\n---\n# B\n\nold.\n")
	headSHA := commitAll(t, dir, "init")

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){triageResponse(false)}}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")
	runner.SetRemote(dir)

	const patch = "@@ -1,2 +1,3 @@\n package deep\n+func X() {}\n"
	req := review.Request{
		InstallationID: 1, Owner: "o", Repo: "r", Number: 1, HeadSHA: headSHA, BaseSHA: headSHA,
		ChangedFiles: []review.ChangedFile{{Path: "src/pkg/deep/x.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: patch}},
	}

	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	if _, ok := result.Verdict.(review.NoImpact); !ok {
		t.Fatalf("Verdict = %T, want review.NoImpact", result.Verdict)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model saw %d calls, want exactly 1 triage call (docs/a.md only)", len(model.calls))
	}
	prompt := model.calls[0].Messages[0].Text
	for _, want := range []string{"docs/a.md", patch} {
		if !strings.Contains(prompt, want) {
			t.Errorf("triage prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "docs/b.md") {
		t.Errorf("triage prompt mentions uncovered docs/b.md:\n%s", prompt)
	}
}
