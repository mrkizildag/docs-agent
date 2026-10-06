package llmrunner_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm/llmtest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
)

const sectionDocFrontmatter = "---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n"

// rejectedThenFixed scripts a draft whose first submission is bad: the model
// must get an error tool result containing wantErr, then resubmits a good one.
func rejectedThenFixed(t *testing.T, bad map[string]any, wantErr string) *llmtest.ScriptedModel {
	t.Helper()

	return &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(bad),
		func(req llm.Request) (llm.Response, error) {
			if res := lastToolResult(req); !res.IsError || !strings.Contains(res.Content, wantErr) {
				t.Errorf("tool result = %+v, want an error containing %q", res, wantErr)
			}
			return submitResponse(proposalFor("docs/x.md", 2))(req)
		},
		verifyResponse(true),
	}}
}

func TestStart_SectionOnUnknownDocIsReturnedToModel(t *testing.T) {
	t.Parallel()

	bad := proposalFor("docs/missing.md", 2)
	if _, _, err := startResult(t, rejectedThenFixed(t, bad, "leave section empty")); err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
}

func TestStart_AmbiguousHeadingIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	headSHA := commitDoc(t, repoDir, "docs/x.md", sectionDocFrontmatter+"# X\n\n## Dup\none\n\n## Dup\ntwo\n")

	bad := proposalFor("docs/x.md", 2)
	bad["section"] = "Dup"
	runner := llmrunner.New(rejectedThenFixed(t, bad, `"Dup", "Dup"`), noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)

	if _, err := runner.Start(t.Context(), testRequest(headSHA)); err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
}

func TestStart_VerifyPromptShowsProposalOriginalOrNewDoc(t *testing.T) {
	t.Parallel()

	newDoc := proposalFor("docs/new.md", 2)
	newDoc["section"] = ""
	newDoc["index_entry"] = "new: Describes new."

	tests := []struct {
		name     string
		proposal map[string]any
		want     string
	}{
		{name: "replaced section", proposal: proposalFor("docs/x.md", 2), want: "old behavior."},
		{name: "new doc", proposal: newDoc, want: "(new doc)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			model := &llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
				triageResponse(true),
				submitResponse(tc.proposal),
				verifyResponse(true),
			}}
			if _, _, err := startResult(t, model); err != nil {
				t.Fatalf("Start() = %v, want nil error", err)
			}
			if prompt := model.Calls[len(model.Calls)-1].Messages[0].Text; !strings.Contains(prompt, tc.want) {
				t.Errorf("verify prompt = %q, want it to contain %q", prompt, tc.want)
			}
		})
	}
}

func TestStart_LogsUnparseableDocsAndAgentStats(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	commitDoc(t, repoDir, "docs/broken.md", "no frontmatter\n")
	headSHA := commitDoc(t, repoDir, "docs/x.md", sectionDocFrontmatter+"# X\n\nnewer behavior.\n")

	var logs bytes.Buffer
	runner := llmrunner.New(&llmtest.ScriptedModel{Script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(proposalFor("docs/x.md", 2)),
		verifyResponse(true),
	}}, noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)
	runner.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	if _, err := runner.Start(t.Context(), review.Request{
		InstallationID: 1, Owner: "o", Repo: "r", Number: 1, HeadSHA: headSHA, BaseSHA: headSHA,
		ChangedFiles: testRequest(headSHA).ChangedFiles,
	}); err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}

	for _, want := range []string{"docs/broken.md", "steps=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want them to contain %q", logs.String(), want)
		}
	}
}
