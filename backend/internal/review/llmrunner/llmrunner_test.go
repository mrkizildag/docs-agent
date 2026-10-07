package llmrunner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
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

func testRequest(headSHA string) review.Request {
	return review.Request{
		InstallationID: 1,
		Owner:          "o",
		Repo:           "r",
		Number:         1,
		HeadSHA:        headSHA,
		BaseSHA:        headSHA,
		ChangedFiles: []review.ChangedFile{
			{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"},
		},
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
	if err := os.WriteFile(filepath.Join(dir, "docs", "x.md"), []byte("---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n# X\n\nold behavior.\n"), 0o600); err != nil {
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

func noToken(context.Context, int64, string) (string, error) { return "", nil }

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

	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)

	req := review.Request{
		InstallationID: 1,
		Owner:          "o",
		Repo:           "r",
		Number:         1,
		HeadSHA:        headSHA,
		BaseSHA:        headSHA,
		ChangedFiles: []review.ChangedFile{
			{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"},
		},
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

	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)

	req := review.Request{
		InstallationID: 1,
		Owner:          "o",
		Repo:           "r",
		Number:         1,
		HeadSHA:        headSHA,
		BaseSHA:        headSHA,
		ChangedFiles: []review.ChangedFile{
			{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"},
		},
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

func TestStart_CoveredFileIsTriagedWithItsPatch(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(false),
	}}

	if _, _, err := startResult(t, model); err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model saw %d calls, want 1 triage call", len(model.calls))
	}
	prompt := model.calls[0].Messages[0].Text
	for _, want := range []string{"docs/x.md", "@@ -1,2 +1,3 @@\n     1  func main() {}\n"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("triage prompt = %q, want it to contain %q", prompt, want)
		}
	}
}

func newDocResponse(needed bool) func(llm.Request) (llm.Response, error) {
	return textResponse(fmt.Sprintf(`{"needed": %t, "reason": "scripted"}`, needed))
}

func startUncovered(t *testing.T, changed []review.ChangedFile, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, *fakeModel) {
	t.Helper()

	repoDir, headSHA := newGitRepo(t)
	model := &fakeModel{script: script}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)

	req := testRequest(headSHA)
	req.ChangedFiles = changed
	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	return result.Verdict, model
}

func otherGoChange() review.ChangedFile {
	return review.ChangedFile{Path: "other.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -0,0 +1,3 @@\n+func other() {}\n"}
}

func newDocProposal(covers string) map[string]any {
	return map[string]any{
		"doc_path":    "docs/other.md",
		"section":     "",
		"anchor":      map[string]any{"file": "other.go", "line": 2},
		"reason":      "other.go adds a feature",
		"content":     "---\ntitle: Other\nsummary: About other.\ncovers:\n  - " + covers + "\n---\n# Other\n",
		"index_entry": "- [Other](other.md): about other.",
	}
}

func startCoveredOnly(t *testing.T, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, *llmrunner.Runner, *fakeModel) {
	t.Helper()

	model := &fakeModel{script: script}
	verdict, runner, err := startResult(t, model)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	return verdict, runner, model
}

func TestStart_UncoveredFileWithoutNeedIsNoImpactAfterOneCall(t *testing.T) {
	t.Parallel()

	verdict, model := startUncovered(t, []review.ChangedFile{otherGoChange()}, newDocResponse(false))
	noImpact, ok := verdict.(review.NoImpact)
	if !ok {
		t.Fatalf("Verdict = %T, want review.NoImpact", verdict)
	}
	for _, want := range []string{"other.go", "no new doc needed", "no doc covers"} {
		if !strings.Contains(noImpact.Reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", noImpact.Reason, want)
		}
	}
	if len(model.calls) != 1 {
		t.Fatalf("model saw %d calls, want exactly 1 new-doc decision", len(model.calls))
	}
	if prompt := model.calls[0].Messages[0].Text; !strings.Contains(prompt, "other.go") || !strings.Contains(prompt, "func other() {}") {
		t.Errorf("new-doc prompt = %q, want the uncovered file and its patch", prompt)
	}
}

func TestStart_UncoveredFileThatNeedsADocGetsANewDocProposal(t *testing.T) {
	t.Parallel()

	verdict, model := startUncovered(t, []review.ChangedFile{otherGoChange()},
		newDocResponse(true), submitResponse(newDocProposal("other.go")), verifyResponse(true))
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 1 {
		t.Fatalf("Verdict = %#v, want one proposal", verdict)
	}
	if p := proposals[0]; p.DocPath != "docs/other.md" || p.Section != "" || p.IndexEntry == "" || p.Original != "" {
		t.Errorf("proposal = %+v, want a new doc docs/other.md with an index entry", p)
	}
	for _, want := range []string{"---\n", "title:", "covers:", "other.go"} {
		if !strings.Contains(proposals[0].Content, want) {
			t.Errorf("new-doc Content = %q, want frontmatter containing %q", proposals[0].Content, want)
		}
	}
	if prompt := model.calls[1].Messages[0].Text; !strings.Contains(prompt, "no doc covers") || !strings.Contains(prompt, "other.go") {
		t.Errorf("draft prompt = %q, want it to list the uncovered file", prompt)
	}
}

func TestStart_NewDocWhoseCoversMissTheUncoveredFilesIsReturnedToModel(t *testing.T) {
	t.Parallel()

	_, model := startUncovered(t, []review.ChangedFile{otherGoChange()},
		newDocResponse(true), submitResponse(newDocProposal("elsewhere.go")), submitResponse(),
	)
	last := model.calls[len(model.calls)-1].Messages
	results := last[len(last)-1].ToolResults
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Content, "proposal 0: covers match none") {
		t.Errorf("tool results = %+v, want a proposal 0 error about covers", results)
	}
}

func TestStart_NewDocIsRejectedWhenNoneWasNeeded(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startUncovered(t, changed,
		triageResponse(true), newDocResponse(false), submitResponse(newDocProposal("other.go")), submitResponse(), verifyResponse(true))
	last := model.calls[len(model.calls)-1].Messages
	results := last[len(last)-1].ToolResults
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Content, "not allowed") {
		t.Errorf("tool results = %+v, want an error rejecting the new doc", results)
	}
}

func TestStart_HashOnlySectionCannotBypassNewDocChecks(t *testing.T) {
	t.Parallel()

	proposal := proposalFor("docs/x.md", 2)
	proposal["section"] = "#"
	verdict, _, model := startCoveredOnly(t, triageResponse(true), submitResponse(proposal), submitResponse())
	last := model.calls[len(model.calls)-1].Messages
	results := last[len(last)-1].ToolResults
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Content, "proposal 0: section: must name a heading") {
		t.Errorf("tool results = %+v, want an error rejecting the empty heading", results)
	}
	if _, ok := verdict.(review.NoImpact); !ok {
		t.Errorf("Verdict = %#v, want NoImpact, no new doc", verdict)
	}
}

func TestStart_NewDocAtAnExistingPathIsReturnedToModel(t *testing.T) {
	t.Parallel()

	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/x.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startUncovered(t, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	last := model.calls[len(model.calls)-1].Messages
	results := last[len(last)-1].ToolResults
	if len(results) != 1 || !results[0].IsError || !strings.Contains(results[0].Content, "already exists") {
		t.Errorf("tool results = %+v, want an error telling the model to pick a new path", results)
	}
}

func TestStart_ChangedPathWithNewlineIsQuotedInAnchorHunks(t *testing.T) {
	t.Parallel()

	evil := otherGoChange()
	evil.Path = "evil\nInjected: line.go"
	_, model := startUncovered(t, []review.ChangedFile{evil}, newDocResponse(true), submitResponse())
	prompt := model.calls[1].Messages[0].Text
	if want := `"evil\nInjected: line.go": 1-3`; !strings.Contains(prompt, want) {
		t.Errorf("draft prompt = %q, want it to contain the quoted path line %q", prompt, want)
	}
	if strings.Contains(prompt, "\nInjected: line.go: ") {
		t.Errorf("draft prompt = %q, want no line started by the raw path tail", prompt)
	}
}

func TestStart_MixedChangeTriagesCandidatesAndDecidesNewDoc(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	verdict, model := startUncovered(t, changed,
		triageResponse(true), newDocResponse(true),
		submitResponse(proposalFor("docs/x.md", 2), newDocProposal("other.go")), verifyResponse(true), verifyResponse(true))
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 2 {
		t.Fatalf("Verdict = %#v, want a section proposal and a new-doc proposal", verdict)
	}
	if len(model.calls) != 5 {
		t.Errorf("model saw %d calls, want 5 (triage, new-doc, draft, 2 verifies)", len(model.calls))
	}
}

func TestStart_RemovalsAndDocsOnlyAreNoImpactWithoutModelCalls(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{
		{Path: "gone.go", Removed: true},
		{Path: "docs/new.md", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -0,0 +1,3 @@\n+x\n"},
	}
	verdict, model := startUncovered(t, changed)
	if _, ok := verdict.(review.NoImpact); !ok {
		t.Fatalf("Verdict = %T, want review.NoImpact", verdict)
	}
	if len(model.calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(model.calls))
	}
}

func TestStart_NoChangedFilesIsNoImpactWithoutCloneOrModel(t *testing.T) {
	t.Parallel()

	model := &fakeModel{}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))

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

func startResult(t *testing.T, model llm.Model) (review.Verdict, *llmrunner.Runner, error) {
	t.Helper()
	repoDir, headSHA := newGitRepo(t)
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	started, err := runner.Start(t.Context(), testRequest(headSHA))
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
		submitResponse(proposalFor("docs/x.md", 2), proposalFor("docs/x.md", 3)),
		verifyResponse(true),
		verifyResponse(false),
	}}

	verdict, _, err := startResult(t, model)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	proposals, ok := verdict.(review.Proposals)
	if !ok || len(proposals) != 1 || proposals[0].Anchor.Line != 2 {
		t.Fatalf("Verdict = %#v, want exactly the first proposal", verdict)
	}
}

func TestStart_VerificationRejectsAllIsNoImpact(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(proposalFor("docs/x.md", 2)),
		textResponse(`{"supported": false, "reason": "diff\nunrelated"}`),
	}}

	verdict, _, err := startResult(t, model)
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

	verdict, _, err := startResult(t, model)
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

	verdict, _, err := startResult(t, model)
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

	_, _, err := startResult(t, model)
	if err == nil || !strings.Contains(err.Error(), "I cannot decide.") {
		t.Fatalf("Start() = %v, want an error quoting the reply", err)
	}
}

func TestStart_InvalidProposalIsReturnedToModel(t *testing.T) {
	t.Parallel()

	bad := proposalFor("docs/x.md", 2)
	bad["anchor"] = map[string]any{"file": "main.go", "line": 99}
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(bad),
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError || !strings.Contains(last.ToolResults[0].Content, `proposal 0: anchor.line 99: not a numbered line in the diff of "main.go"; commentable lines: 1-3`) {
				t.Errorf("last message = %+v, want a validation error tool result listing the commentable lines", last)
			}
			return submitResponse(proposalFor("docs/x.md", 2))(req)
		},
		verifyResponse(true),
	}}

	verdict, _, err := startResult(t, model)
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
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	runner.SetTokenBudget(10)

	_, err := runner.Start(t.Context(), testRequest(headSHA))
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
	runner := llmrunner.New(blockingModel{}, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	runner.SetTimeout(200 * time.Millisecond)

	_, err := runner.Start(t.Context(), testRequest(headSHA))
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
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))

	_, err := runner.Start(t.Context(), testRequest("--upload-pack=x"))
	if err == nil {
		t.Fatal("Start(head sha \"--upload-pack=x\") = nil error, want an error")
	}
	if len(model.calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(model.calls))
	}
}

func TestStart_SectionWithHashesIsNormalized(t *testing.T) {
	t.Parallel()

	p := proposalFor("docs/x.md", 2)
	p["section"] = "## X"
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(p),
		verifyResponse(true),
	}}

	verdict, _, err := startResult(t, model)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if proposals, ok := verdict.(review.Proposals); !ok || len(proposals) != 1 || proposals[0].Section != "X" {
		t.Fatalf("Verdict = %#v, want one proposal with section \"X\"", verdict)
	}
}

func TestStart_UnknownSectionIsReturnedToModelWithHeadings(t *testing.T) {
	t.Parallel()

	bad := proposalFor("docs/x.md", 2)
	bad["section"] = "Nope"
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(bad),
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError || !strings.Contains(last.ToolResults[0].Content, `"X"`) {
				t.Errorf("last message = %+v, want an error tool result listing heading \"X\"", last)
			}
			return submitResponse(proposalFor("docs/x.md", 2))(req)
		},
		verifyResponse(true),
	}}

	verdict, _, err := startResult(t, model)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if proposals, ok := verdict.(review.Proposals); !ok || len(proposals) != 1 {
		t.Fatalf("Verdict = %#v, want the resubmitted proposal", verdict)
	}
}

func TestStart_DraftPromptListsHunkRanges(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(),
	}}
	if _, _, err := startResult(t, model); err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if got := model.calls[1].Messages[0].Text; !strings.Contains(got, `"main.go": 1-3`) {
		t.Errorf("draft prompt = %q, want it to contain \"main.go: 1-3\"", got)
	}
}

func TestStart_RenameMatchesDocCoveringOnlyOldPath(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){triageResponse(false), newDocResponse(false)}}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)

	req := testRequest(headSHA)
	req.ChangedFiles[0].Path = "renamed.go"
	req.ChangedFiles[0].PreviousPath = "main.go"
	if _, err := runner.Start(t.Context(), req); err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model saw %d calls, want 1 triage call for docs/x.md", len(model.calls))
	}
}

func TestStart_TriageReplyWithoutImpactedErrors(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){textResponse(`{"reason": "hmm"}`)}}
	_, _, err := startResult(t, model)
	if err == nil || !strings.Contains(err.Error(), "impacted") || !strings.Contains(err.Error(), "hmm") {
		t.Fatalf("Start() = %v, want an error naming the missing field and quoting the reply", err)
	}
}

func TestStart_VerifyReplyWithoutSupportedErrors(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(proposalFor("docs/x.md", 2)),
		textResponse(`{"reason": "hmm"}`),
	}}
	_, _, err := startResult(t, model)
	if err == nil || !strings.Contains(err.Error(), "supported") || !strings.Contains(err.Error(), "hmm") {
		t.Fatalf("Start() = %v, want an error naming the missing field and quoting the reply", err)
	}
}

func TestStart_PromptsFencePatchAndMarkOmittedPatch(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){triageResponse(false), newDocResponse(false)}}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)

	req := testRequest(headSHA)
	req.ChangedFiles = append(req.ChangedFiles, review.ChangedFile{Path: "big.bin"})
	if _, err := runner.Start(t.Context(), req); err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	prompt := model.calls[0].Messages[0].Text
	patchAt := strings.Index(prompt, "func main() {}")
	open := strings.LastIndex(prompt[:patchAt], "<<<UNTRUSTED-")
	end := strings.Index(prompt[patchAt:], "<<<END-")
	if open < 0 || end < 0 {
		t.Errorf("triage prompt does not fence the patch:\n%s", prompt)
	}
	if !strings.Contains(prompt, "(patch omitted by GitHub: large or binary file)") {
		t.Errorf("triage prompt does not mark the omitted patch:\n%s", prompt)
	}
	if !strings.Contains(model.calls[0].System, "<<<UNTRUSTED-") {
		t.Errorf("triage system prompt does not explain the markers: %q", model.calls[0].System)
	}
}

func TestStart_TooManyCandidateDocsIsAnError(t *testing.T) {
	t.Parallel()

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
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o700); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	for i := range 11 {
		doc := fmt.Sprintf("---\ntitle: D%d\nsummary: Describes D.\ncovers:\n  - main.go\n---\n# D\n", i)
		if err := os.WriteFile(filepath.Join(dir, "docs", fmt.Sprintf("d%d.md", i)), []byte(doc), 0o600); err != nil {
			t.Fatalf("write doc: %v", err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // dir is a t.TempDir path
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	runner := llmrunner.New(&fakeModel{}, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(dir)
	_, err = runner.Start(t.Context(), testRequest(strings.TrimSpace(string(out))))
	if err == nil || !strings.Contains(err.Error(), "cap of 10") {
		t.Fatalf("Start() = %v, want an error naming the candidate cap", err)
	}
}

func commitDoc(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, relPath), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "doc"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // dir is a t.TempDir path, not external input
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestStart_ProposalCarriesOriginalSectionAndLines(t *testing.T) {
	t.Parallel()

	const frontmatter = "---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n"
	const body = "# X\n\n## Mid\nmid body\n\n## Last\nlast body"

	tests := []struct {
		name      string
		doc       string
		docPath   string
		section   string
		want      string
		wantLines review.LineRange
	}{
		{
			name:      "middle section followed by a blank line counts frontmatter",
			doc:       frontmatter + body + "\n",
			docPath:   "docs/x.md",
			section:   "Mid",
			want:      "## Mid\nmid body\n\n",
			wantLines: review.LineRange{Start: 9, End: 11},
		},
		{
			name:      "last section with trailing newline",
			doc:       frontmatter + body + "\n",
			docPath:   "docs/x.md",
			section:   "Last",
			want:      "## Last\nlast body\n",
			wantLines: review.LineRange{Start: 12, End: 13},
		},
		{
			name:      "last section without trailing newline",
			doc:       frontmatter + body,
			docPath:   "docs/x.md",
			section:   "Last",
			want:      "## Last\nlast body",
			wantLines: review.LineRange{Start: 12, End: 13},
		},
		{
			name:      "heading written with leading hashes",
			doc:       frontmatter + body + "\n",
			docPath:   "docs/x.md",
			section:   "## Mid",
			want:      "## Mid\nmid body\n\n",
			wantLines: review.LineRange{Start: 9, End: 11},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoDir, _ := newGitRepo(t)
			headSHA := commitDoc(t, repoDir, "docs/x.md", tc.doc)

			proposal := proposalFor(tc.docPath, 2)
			proposal["section"] = tc.section
			model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
				triageResponse(true),
				submitResponse(proposal),
				verifyResponse(true),
			}}
			runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
			runner.SetRemote(repoDir)

			started, err := runner.Start(t.Context(), testRequest(headSHA))
			if err != nil {
				t.Fatalf("Start() = %v, want nil error", err)
			}
			result, ok := started.(review.Result)
			if !ok {
				t.Fatalf("Start() = %T, want review.Result", started)
			}
			proposals, ok := result.Verdict.(review.Proposals)
			if !ok || len(proposals) != 1 {
				t.Fatalf("Verdict = %#v, want exactly one proposal", result.Verdict)
			}
			got := proposals[0]
			if got.Original != tc.want || got.Lines != tc.wantLines {
				t.Fatalf("Original, Lines = %q, %+v, want %q, %+v", got.Original, got.Lines, tc.want, tc.wantLines)
			}

			if tc.want == "" {
				return
			}
			docLines := strings.Split(strings.TrimSuffix(tc.doc, "\n"), "\n")
			replaced := strings.Join(docLines[got.Lines.Start-1:got.Lines.End], "\n")
			if replaced != strings.TrimSuffix(got.Original, "\n") {
				t.Errorf("doc lines %d-%d = %q, want them to equal Original %q", got.Lines.Start, got.Lines.End, replaced, got.Original)
			}
		})
	}
}

func TestStart_DocsFileAtHeadIsAbsentReadme(t *testing.T) {
	t.Parallel()

	dir, baseSHA := newGitRepo(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("rm", "-rq", "docs")
	if err := os.WriteFile(filepath.Join(dir, "docs"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("write docs file: %v", err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "docs becomes a file")
	headSHA := git("rev-parse", "HEAD")

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){newDocResponse(false)}}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(dir)
	req := testRequest(headSHA)
	req.BaseSHA = baseSHA
	req.ChangedFiles = []review.ChangedFile{otherGoChange()}

	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if _, ok := started.(review.Result); !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model saw %d calls, want exactly 1 new-doc decision", len(model.calls))
	}
}

func TestCombinedPatchCapCutsAtALineEnd(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString("@@ -0,0 +1,9000 @@ f")
	for i := range 9000 {
		fmt.Fprintf(&b, "\n+line %04d END", i)
	}
	got := llmrunner.CombinedPatch([]review.ChangedFile{{Path: "big.go", Patch: b.String()}})

	before, _, found := strings.Cut(got, "\n(patch truncated")
	if !found {
		t.Fatalf("combined patch was not truncated:\n%.200s", got)
	}
	if last := before[strings.LastIndexByte(before, '\n')+1:]; !strings.HasSuffix(last, " END") {
		t.Errorf("last line before the truncation note = %q, want a whole numbered line", last)
	}
}

// startOnRepo runs Start over a repo the caller prepared and returns the
// verdict and the model.
func startOnRepo(t *testing.T, repoDir, headSHA string, changed []review.ChangedFile, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, *fakeModel) {
	t.Helper()

	model := &fakeModel{script: script}
	runner := llmrunner.New(model, noToken, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	req := testRequest(headSHA)
	req.ChangedFiles = changed
	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	return result.Verdict, model
}

// returnedToModel is the error text of the tool result the model got after its
// first submission.
func returnedToModel(t *testing.T, model *fakeModel) string {
	t.Helper()

	for _, call := range model.calls {
		for _, m := range call.Messages {
			for _, r := range m.ToolResults {
				if r.IsError {
					return r.Content
				}
			}
		}
	}
	t.Fatal("model never received an error tool result")
	return ""
}

func TestStart_DuplicateHeadingIsReturnedToModelAsAmbiguous(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	headSHA := commitDoc(t, repoDir, "docs/x.md", "---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n# Top\n\n## X\none\n\n## X\ntwo\n")

	_, model := startOnRepo(t, repoDir, headSHA, testRequest(headSHA).ChangedFiles,
		triageResponse(true), submitResponse(proposalFor("docs/x.md", 2)), submitResponse())
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "ambiguous") {
		t.Errorf("tool error = %q, want proposal 0 reported as ambiguous", got)
	}
}

func TestStart_SectionEditOfDocMissingAtHeadIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	_, model := startOnRepo(t, repoDir, headSHA, testRequest(headSHA).ChangedFiles,
		triageResponse(true), submitResponse(proposalFor("docs/y.md", 2)), submitResponse())
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "no such doc at head") {
		t.Errorf("tool error = %q, want proposal 0 reported as a doc missing at head", got)
	}
}

func TestStart_NewDocAtADirectoryPathIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "docs", "other.md"), 0o700); err != nil {
		t.Fatalf("mkdir docs/other.md: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "docs/other.md/keep.txt", "keep\n")

	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(newDocProposal("other.go")), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_EveryBadProposalIsReturnedToModelWithItsIndex(t *testing.T) {
	t.Parallel()

	_, _, model := startCoveredOnly(t, triageResponse(true),
		submitResponse(proposalFor("docs/y.md", 2), proposalFor("docs/x.md", 2), proposalFor("docs/z.md", 2)), submitResponse())
	got := returnedToModel(t, model)
	for _, want := range []string{"proposal 0:", "proposal 2:"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool error = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "proposal 1:") {
		t.Errorf("tool error = %q, want no problem for the valid proposal 1", got)
	}
}
