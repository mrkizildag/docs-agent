package llmrunner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/agent"
	"github.com/mrkizildag/docs-agent/backend/internal/llm"
	"github.com/mrkizildag/docs-agent/backend/internal/review"
	"github.com/mrkizildag/docs-agent/backend/internal/review/llmrunner"
)

// fakeModel scripts one llm.Response (or error) per call, in order, and
// records every request it saw.
type fakeModel struct {
	script []func(req llm.Request) (llm.Response, error)
	calls  []llm.Request
}

func (f *fakeModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.calls = append(f.calls, req)
	i := len(f.calls) - 1
	if i >= len(f.script) {
		return llm.Response{}, fmt.Errorf("fakeModel: unexpected call %d", i+1)
	}
	return f.script[i](req)
}

func triageResponse(impacted bool) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) {
		text, err := json.Marshal(map[string]any{"impacted": impacted, "reason": "scripted"})
		if err != nil {
			return llm.Response{}, fmt.Errorf("marshal triage verdict: %w", err)
		}
		return llm.Response{Text: string(text)}, nil
	}
}

func textResponse(text string) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) { return llm.Response{Text: text}, nil }
}

func verifyResponse(supported bool) func(llm.Request) (llm.Response, error) {
	return textResponse(fmt.Sprintf(`{"supported": %t, "reason": "scripted"}`, supported))
}

func proposalFor(docPath string, line int) map[string]any {
	return map[string]any{
		"doc_path": docPath,
		"section":  "X",
		"anchor":   map[string]any{"file": "main.go", "line": line},
		"reason":   "main.go's behavior changed",
		"content":  "new behavior.",
	}
}

func submitResponse(proposals ...any) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) {
		args, err := json.Marshal(map[string]any{"proposals": proposals})
		if err != nil {
			return llm.Response{}, fmt.Errorf("marshal proposals: %w", err)
		}
		return llm.Response{ToolCalls: []llm.ToolCall{{ID: "s", Name: "submit_proposals", Args: args}}}, nil
	}
}

func testRequest(headSHA string, docs ...string) review.Request {
	return review.Request{
		InstallationID: 1,
		Owner:          "o",
		Repo:           "r",
		Number:         1,
		HeadSHA:        headSHA,
		ChangedFiles: []review.ChangedFile{
			{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"},
		},
		CandidateDocs: docs,
	}
}

// newGitRepo creates a repo with a code file and a doc, commits it, and
// returns the repo's directory and the commit's SHA.
func newGitRepo(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o700); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "x.md"), []byte("# X\n\nold behavior.\n"), 0o600); err != nil {
		t.Fatalf("write docs/x.md: %v", err)
	}

	run("add", "-A")
	run("commit", "-q", "-m", "init")

	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // dir is a t.TempDir path, not external input
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	headSHA := string(out)
	headSHA = headSHA[:len(headSHA)-1] // trim trailing newline

	return dir, headSHA
}

func noToken(context.Context, int64) (string, error) { return "", nil }

func TestStart_ImpactedDocProducesProposal(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)

	validProposal := map[string]any{
		"doc_path": "docs/x.md",
		"section":  "X",
		"anchor":   map[string]any{"file": "main.go", "line": 2},
		"reason":   "main.go's behavior changed",
		"content":  "new behavior.",
	}

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		func(llm.Request) (llm.Response, error) {
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", Args: json.RawMessage(`{"path":"docs/x.md"}`)}},
			}, nil
		},
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || last.ToolResults[0].IsError {
				t.Fatalf("tool result = %+v, want a successful read_file result", last)
			}
			args, err := json.Marshal(map[string]any{"proposals": []any{validProposal}})
			if err != nil {
				return llm.Response{}, fmt.Errorf("marshal proposals: %w", err)
			}
			return llm.Response{
				ToolCalls: []llm.ToolCall{{ID: "2", Name: "submit_proposals", Args: args}},
			}, nil
		},
		verifyResponse(true),
	}}

	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)

	req := review.Request{
		InstallationID: 1,
		Owner:          "o",
		Repo:           "r",
		Number:         1,
		HeadSHA:        headSHA,
		ChangedFiles: []review.ChangedFile{
			{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"},
		},
		CandidateDocs: []string{"docs/x.md"},
	}

	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}

	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	proposals, ok := result.Verdict.(review.Proposals)
	if !ok {
		t.Fatalf("Verdict = %T, want review.Proposals", result.Verdict)
	}
	if len(proposals) != 1 {
		t.Fatalf("len(proposals) = %d, want 1", len(proposals))
	}
	if proposals[0].DocPath != "docs/x.md" || proposals[0].Anchor.File != "main.go" || proposals[0].Anchor.Line != 2 {
		t.Errorf("proposals[0] = %+v, want doc_path docs/x.md anchored at main.go:2", proposals[0])
	}
}

func TestStart_AllTriageNoIsNoImpact(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(false),
	}}

	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)

	req := review.Request{
		InstallationID: 1,
		Owner:          "o",
		Repo:           "r",
		Number:         1,
		HeadSHA:        headSHA,
		ChangedFiles: []review.ChangedFile{
			{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"},
		},
		CandidateDocs: []string{"docs/x.md"},
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
		t.Errorf("model saw %d calls, want exactly 1 (one triage call per candidate doc)", len(model.calls))
	}
}

func TestStart_NoCandidateDocsSkipsTheModel(t *testing.T) {
	t.Parallel()

	model := &fakeModel{}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")

	started, err := runner.Start(t.Context(), review.Request{Owner: "o", Repo: "r", Number: 1, HeadSHA: "deadbeef"})
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
	if len(model.calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(model.calls))
	}
}

func startResult(t *testing.T, model llm.Model, docs ...string) (review.Verdict, *llmrunner.Runner, error) {
	t.Helper()
	repoDir, headSHA := newGitRepo(t)
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)
	started, err := runner.Start(t.Context(), testRequest(headSHA, docs...))
	if err != nil {
		return nil, runner, fmt.Errorf("start: %w", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	return result.Verdict, runner, nil
}

func TestStart_VerificationDropsRejectedProposal(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(proposalFor("docs/x.md", 2), proposalFor("docs/y.md", 3)),
		verifyResponse(true),
		verifyResponse(false),
	}}

	verdict, _, err := startResult(t, model, "docs/x.md")
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 1 || proposals[0].DocPath != "docs/x.md" {
		t.Fatalf("Verdict = %#v, want exactly the docs/x.md proposal", verdict)
	}
}

func TestStart_VerificationRejectsAllIsNoImpact(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(proposalFor("docs/x.md", 2)),
		textResponse(`{"supported": false, "reason": "diff\nunrelated"}`),
	}}

	verdict, _, err := startResult(t, model, "docs/x.md")
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	noImpact, ok := verdict.(review.NoImpact)
	if !ok {
		t.Fatalf("Verdict = %T, want review.NoImpact", verdict)
	}
	if strings.Contains(noImpact.Reason, "\n") || !strings.Contains(noImpact.Reason, "unrelated") {
		t.Errorf("Reason = %q, want one line carrying the verification reason", noImpact.Reason)
	}
}

func TestStart_NoImpactReasonJoinsTriageReasons(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(false),
	}}

	verdict, _, err := startResult(t, model, "docs/x.md")
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	noImpact, ok := verdict.(review.NoImpact)
	if !ok || !strings.Contains(noImpact.Reason, "docs/x.md: scripted") {
		t.Fatalf("Verdict = %#v, want NoImpact carrying the triage reason", verdict)
	}
}

func TestStart_FencedTriageReplyParses(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		textResponse("Sure:\n```json\n{\"impacted\": false, \"reason\": \"fenced\"}\n```\nDone."),
	}}

	verdict, _, err := startResult(t, model, "docs/x.md")
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	noImpact, ok := verdict.(review.NoImpact)
	if !ok || !strings.Contains(noImpact.Reason, "fenced") {
		t.Fatalf("Verdict = %#v, want NoImpact with reason fenced", verdict)
	}
}

func TestStart_UnparseableTriageReplyErrorsWithReply(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		textResponse("I cannot decide."),
	}}

	_, _, err := startResult(t, model, "docs/x.md")
	if err == nil || !strings.Contains(err.Error(), "I cannot decide.") {
		t.Fatalf("Start() = %v, want an error quoting the reply", err)
	}
}

func TestStart_InvalidProposalIsReturnedToModel(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(proposalFor("docs/x.md", 99)),
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError || !strings.Contains(last.ToolResults[0].Content, "docs/x.md") {
				t.Errorf("last message = %+v, want a validation error tool result", last)
			}
			return submitResponse(proposalFor("docs/x.md", 2))(req)
		},
		verifyResponse(true),
	}}

	verdict, _, err := startResult(t, model, "docs/x.md")
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if proposals, ok := verdict.(review.Proposals); !ok || len(proposals) != 1 || proposals[0].Anchor.Line != 2 {
		t.Fatalf("Verdict = %#v, want the resubmitted valid proposal", verdict)
	}
}

func TestStart_TokenBudgetExceededDuringTriage(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{Text: `{"impacted": false, "reason": "x"}`, Usage: llm.Usage{InputTokens: 100}}, nil
		},
	}}

	repoDir, headSHA := newGitRepo(t)
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)
	runner.SetTokenBudget(10)

	_, err := runner.Start(t.Context(), testRequest(headSHA, "docs/x.md"))
	if !errors.Is(err, agent.ErrTokenBudget) {
		t.Fatalf("Start() = %v, want errors.Is agent.ErrTokenBudget", err)
	}
}

type blockingModel struct{}

func (blockingModel) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, fmt.Errorf("blocking model: %w", ctx.Err())
}

func TestStart_DeadlineIsErrDeadline(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	runner := llmrunner.New(blockingModel{}, noToken, "triage-model", "draft-model")
	runner.SetRemote(repoDir)
	runner.SetTimeout(200 * time.Millisecond)

	_, err := runner.Start(t.Context(), testRequest(headSHA, "docs/x.md"))
	if !errors.Is(err, agent.ErrDeadline) {
		t.Fatalf("Start() = %v, want errors.Is agent.ErrDeadline", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Start() = %v, must not match context.Canceled", err)
	}
}

func TestStart_RejectsHeadSHAThatIsNotAFullObjectID(t *testing.T) {
	t.Parallel()

	model := &fakeModel{}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model")

	_, err := runner.Start(t.Context(), testRequest("--upload-pack=x", "docs/x.md"))
	if err == nil {
		t.Fatal("Start(head sha \"--upload-pack=x\") = nil error, want an error")
	}
	if len(model.calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(model.calls))
	}
}
