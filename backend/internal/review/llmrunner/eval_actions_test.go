//go:build eval

package llmrunner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/mrkizildag/pollux-agent/backend/internal/config"
	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
)

// actionsRunTimeout is the Deadline the runner stamps on a run; the local run
// is synchronous and never waits on it.
const actionsRunTimeout = 2 * time.Hour

var _ actions.WorkflowAPI = (*localWorkflow)(nil)

// claudeSandbox is the minimal environment Claude Code runs in: a clean CI
// runner has none of the developer's ~/.claude hooks, settings, or CLAUDE.md.
type claudeSandbox struct {
	path       string
	oauthToken config.Secret
	apiKey     config.Secret
}

// judgeEnv returns the whole environment for the judge; unset credentials are omitted.
func (s claudeSandbox) judgeEnv(home, tmp string) []string {
	env := []string{"PATH=" + s.path, "HOME=" + home, "TMPDIR=" + tmp}
	if s.oauthToken.Reveal() != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+s.oauthToken.Reveal())
	}
	if s.apiKey.Reveal() != "" {
		env = append(env, "ANTHROPIC_API_KEY="+s.apiKey.Reveal())
	}
	return env
}

// scriptEnv returns the whole environment for run-claude.sh, which reads both
// credentials under set -u, so both are always defined, empty when unset.
func (s claudeSandbox) scriptEnv(home, tmp string, extra ...string) []string {
	env := []string{
		"PATH=" + s.path, "HOME=" + home, "TMPDIR=" + tmp,
		"CLAUDE_CODE_OAUTH_TOKEN=" + s.oauthToken.Reveal(),
		"ANTHROPIC_API_KEY=" + s.apiKey.Reveal(),
	}
	return append(env, extra...)
}

// missingTools returns an error naming every tool that is not on PATH.
func missingTools(names ...string) error {
	var errs []error
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			errs = append(errs, fmt.Errorf("%s not found on PATH: %w", n, err))
		}
	}
	return errors.Join(errs...)
}

func mkdirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// claudeJudge judges with a tool-less `claude -p` in a fresh HOME and cwd; an
// empty model uses Claude Code's default.
func claudeJudge(sandbox claudeSandbox, model string) judgeFunc {
	return func(ctx context.Context, prompt string) (reply string, err error) {
		dir, err := os.MkdirTemp("", "pollux-eval-judge-")
		if err != nil {
			return "", fmt.Errorf("create judge dir: %w", err)
		}
		defer func() {
			if rerr := os.RemoveAll(dir); rerr != nil && err == nil {
				err = fmt.Errorf("remove judge dir %s: %w", dir, rerr)
			}
		}()
		home, tmp := filepath.Join(dir, "home"), filepath.Join(dir, "tmp")
		if err := mkdirs(home, tmp); err != nil {
			return "", err
		}

		args := []string{"-p", prompt, "--system-prompt", judgeSystem, "--output-format", "json", "--tools", "", "--strict-mcp-config"}
		if model != "" {
			args = append(args, "--model", model)
		}
		cmd := exec.CommandContext(ctx, "claude", args...) //nolint:gosec // fixed binary; the prompt is one argv entry and no shell runs
		cmd.Dir = dir
		cmd.Env = sandbox.judgeEnv(home, tmp)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			const maxStderr = 500
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > maxStderr {
				msg = msg[:maxStderr] + "..."
			}
			return "", fmt.Errorf("claude judge: %w: %s", err, msg)
		}

		var res struct {
			IsError bool   `json:"is_error"`
			Result  string `json:"result"`
		}
		if err := json.Unmarshal(out, &res); err != nil {
			return "", fmt.Errorf("decode claude judge output: %w", err)
		}
		if res.IsError {
			return "", errors.New("claude judge reported an error")
		}
		return res.Result, nil
	}
}

// localWorkflow is an actions.WorkflowAPI over the case's git repo that runs
// the action's Claude step on this machine instead of dispatching a workflow.
// It serves one run.
type localWorkflow struct {
	root      string // repository root holding action/
	work      string // per-run scratch directory
	logPrefix string // run log path without extension
	in        evalInput
	sandbox   claudeSandbox
	runID     int64
}

func (w *localWorkflow) artifactPath() string {
	return filepath.Join(w.work, "runner", "pollux-agent", "result.json")
}

// Dispatch checks out the synthetic head, writes the diff, and runs
// action/run-claude.sh with the environment action.yml gives it. A failing
// script is not an error: the artifact it left decides, as in CI.
func (w *localWorkflow) Dispatch(ctx context.Context, _ int64, _, _ string, in actions.DispatchInputs) (int64, error) {
	checkout, runnerTemp := filepath.Join(w.work, "pr"), filepath.Join(w.work, "runner")
	home, tmp := filepath.Join(w.work, "home"), filepath.Join(w.work, "tmp")
	out := filepath.Join(runnerTemp, "pollux-agent")
	if err := mkdirs(home, tmp, out); err != nil {
		return 0, err
	}

	if _, err := runEvalGit(ctx, w.in.Dir, nil, "worktree", "add", "-q", "--detach", checkout, in.HeadSHA); err != nil {
		return 0, fmt.Errorf("check out %s: %w", in.HeadSHA, err)
	}
	diff, err := runEvalGit(ctx, checkout, nil, "diff", "--no-color", "--no-ext-diff", w.in.Request.BaseSHA+"..."+in.HeadSHA)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(out, "pr.diff"), []byte(diff), 0o600); err != nil {
		return 0, fmt.Errorf("write pr.diff: %w", err)
	}

	docsJSON, err := json.Marshal(map[string][]string{"review": in.Docs, "uncovered": in.Uncovered})
	if err != nil {
		return 0, fmt.Errorf("encode docs input: %w", err)
	}
	actionDir := filepath.Join(w.root, "action")
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(actionDir, "run-claude.sh")) //nolint:gosec // the script path is inside this repository
	cmd.Dir = w.work
	cmd.Env = w.sandbox.scriptEnv(home, tmp,
		"HEAD_SHA="+in.HeadSHA,
		"PR_NUMBER="+strconv.Itoa(in.PRNumber),
		"NONCE="+in.Nonce,
		"DOCS="+string(docsJSON),
		"DEFAULT_BRANCH=main",
		"ACTION_PATH="+actionDir,
		"CHECKOUT="+checkout,
		"RUNNER_TEMP="+runnerTemp,
	)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		return 0, fmt.Errorf("run action/run-claude.sh: %w", err)
	}
	if err := os.WriteFile(w.logPrefix+".action.log", log.Bytes(), 0o600); err != nil {
		return 0, fmt.Errorf("write action log: %w", err)
	}

	transcript, err := os.ReadFile(filepath.Join(out, "transcript.jsonl")) //nolint:gosec // the path is inside the per-run scratch directory
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return 0, fmt.Errorf("read transcript: %w", err)
	default:
		if err := os.WriteFile(w.logPrefix+".transcript.jsonl", transcript, 0o600); err != nil { //nolint:gosec // logPrefix is built from a validated case id inside the results directory
			return 0, fmt.Errorf("write transcript: %w", err)
		}
	}
	return w.runID, nil
}

func (w *localWorkflow) ResultArtifact(_ context.Context, _ int64, _, _ string, runID int64) ([]byte, error) {
	if runID != w.runID {
		return nil, fmt.Errorf("no run %d, this workflow ran %d", runID, w.runID)
	}
	data, err := os.ReadFile(w.artifactPath())
	if err != nil {
		return nil, fmt.Errorf("read result artifact of run %d: %w", runID, err)
	}
	return data, nil
}

func (w *localWorkflow) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return w.in.Request.ChangedFiles, nil
}

func (w *localWorkflow) FileAtRef(ctx context.Context, _ int64, _, _, path, ref string) ([]byte, bool, error) {
	return gitFileAt(ctx, w.in.Dir, ref, path)
}

func (w *localWorkflow) DocsAtRef(ctx context.Context, _ int64, _, _, ref string) (fs.FS, error) {
	names, err := runEvalGit(ctx, w.in.Dir, nil, "ls-tree", "-r", "-z", "--name-only", ref, "--", "docs")
	if err != nil {
		return nil, err
	}
	fsys := fstest.MapFS{}
	for name := range strings.SplitSeq(names, "\x00") {
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		content, ok, err := gitFileAt(ctx, w.in.Dir, ref, name)
		if err != nil {
			return nil, err
		}
		if ok {
			fsys[name] = &fstest.MapFile{Data: content}
		}
	}
	return fsys, nil
}

// gitFileAt reads path at ref from the repo in dir; ok is false when it is not
// a file there or exceeds docs.MaxDocBytes.
func gitFileAt(ctx context.Context, dir, ref, path string) (content []byte, ok bool, err error) {
	entry, err := runEvalGit(ctx, dir, nil, "ls-tree", "-l", "-z", ref, "--", path)
	if err != nil {
		return nil, false, err
	}
	// "<mode> <type> <sha> <size>\t<path>"
	meta, _, _ := strings.Cut(entry, "\t")
	fields := strings.Fields(meta)
	if len(fields) != 4 || fields[1] != "blob" {
		return nil, false, nil
	}
	size, err := strconv.Atoi(fields[3])
	if err != nil {
		return nil, false, fmt.Errorf("size of %s at %s: %w", path, ref, err)
	}
	if size > docs.MaxDocBytes {
		return nil, false, nil
	}
	blob, err := runEvalGit(ctx, dir, nil, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, false, err
	}
	return []byte(blob), true, nil
}

// models lists the models the run's artifact reports.
func (w *localWorkflow) models() []string {
	data, err := os.ReadFile(w.artifactPath())
	if err != nil {
		return nil
	}
	var art actions.Artifact[json.RawMessage]
	if err := json.Unmarshal(data, &art); err != nil {
		return nil
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(art.Claude.ModelUsage, &usage); err != nil {
		return nil
	}
	var models []string
	for m := range usage {
		models = append(models, m)
	}
	slices.Sort(models)
	return models
}

// actionsAttempt runs the Actions runner on a case through localWorkflow and
// keeps each run's transcript and script output under logDir.
func actionsAttempt(root, logDir string, sandbox claudeSandbox) attemptFunc {
	return func(ctx context.Context, c evalCase, in evalInput, runNo int) (out attemptOutcome, err error) {
		work, err := os.MkdirTemp("", "pollux-eval-actions-")
		if err != nil {
			return attemptOutcome{}, fmt.Errorf("case %s run %d: create work dir: %w", c.ID, runNo, err)
		}
		defer func() {
			if rerr := os.RemoveAll(work); rerr != nil && err == nil {
				err = fmt.Errorf("case %s run %d: remove work dir %s: %w", c.ID, runNo, work, rerr)
			}
		}()
		// Claude Code compares paths literally; macOS temp dirs are symlinks.
		resolved, err := filepath.EvalSymlinks(work)
		if err != nil {
			return attemptOutcome{}, fmt.Errorf("case %s run %d: resolve work dir: %w", c.ID, runNo, err)
		}

		api := &localWorkflow{
			root: root, work: resolved, in: in, sandbox: sandbox, runID: int64(runNo),
			logPrefix: filepath.Join(logDir, fmt.Sprintf("%s-%d", c.ID, runNo)),
		}
		runner := actions.New(api, actionsRunTimeout, actionsRunTimeout)

		started, runErr := runner.Start(ctx, in.Request)
		switch s := started.(type) {
		case review.Result:
			out.Result = s
		case review.Pending:
			req := in.Request
			out.Result, runErr = runner.Collect(ctx, review.Completion{
				InstallationID: req.InstallationID, Owner: req.Owner, Repo: req.Repo, Number: req.Number,
				HeadSHA: req.HeadSHA, BaseSHA: req.BaseSHA, RunID: s.RunID, Nonce: s.Nonce,
			})
			out.Models = api.models()
		default:
			if runErr == nil {
				runErr = fmt.Errorf("runner returned %T, want review.Result or review.Pending", started)
			}
		}
		out.Err = runErr
		return out, nil
	}
}

// stubClaude writes a `claude` that ignores its arguments and prints a stream
// with one result event, then exits with status.
func stubClaude(t *testing.T, result map[string]any, status int) string {
	t.Helper()

	event, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal stub result: %v", err)
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n{\"type\":\"system\",\"subtype\":\"init\"}\n%s\nEOF\nexit %d\n", event, status)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // the stub must be executable
		t.Fatalf("write stub claude: %v", err)
	}
	return dir
}

// stubCase builds a repo with a base commit and a head commit that changes
// src/thing.go, which docs/features/thing.md covers, and returns the eval input.
func stubCase(t *testing.T) (evalCase, evalInput) {
	t.Helper()
	ctx := t.Context()

	src := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := runEvalGit(ctx, src, evalCommitEnv(), args...)
		if err != nil {
			t.Fatalf("stub repo: %v", err)
		}
		return strings.TrimSpace(out)
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(src, name)
		if err := mkdirs(filepath.Dir(p)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	git("init", "-q")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	write("docs/features/thing.md", "---\ncovers:\n  - src/*.go\n---\n# Thing\n\nIntro.\n\n## Retention\n\nKeeps 7 days.\n")
	write("src/thing.go", "package src\n\nconst Retention = 7\n")
	git("add", "-A")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")
	write("src/thing.go", "package src\n\nconst Retention = 30\n")
	git("add", "-A")
	git("-c", "commit.gpgsign=false", "commit", "-q", "-m", "head")
	head := git("rev-parse", "HEAD")

	c := evalCase{ID: "stub", Base: base, Head: head}
	in, err := buildEvalInput(ctx, src, t.TempDir(), c)
	if err != nil {
		t.Fatalf("buildEvalInput() error = %v", err)
	}
	return c, in
}

func TestEvalActionsLocalRun(t *testing.T) {
	if err := missingTools("bash", "jq", "git"); err != nil {
		t.Fatalf("local action run needs: %v", err)
	}
	t.Setenv("EVAL_RUNNER", "actions")
	t.Setenv("ANTHROPIC_API_KEY", "stub")
	cfg, err := config.LoadEval()
	if err != nil {
		t.Fatalf("load eval config: %v", err)
	}
	ctx := t.Context()
	root, err := evalRepoRoot(ctx)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	c, in := stubCase(t)

	proposal := map[string]any{
		"doc_path": "docs/features/thing.md", "section": "Retention",
		"anchor": map[string]any{"file": "src/thing.go", "line": 3},
		"reason": "retention changed", "content": "## Retention\n\nKeeps 30 days.\n",
	}
	success := func(output map[string]any) map[string]any {
		return map[string]any{
			"type": "result", "subtype": "success", "is_error": false,
			"modelUsage": map[string]any{"stub-model": map[string]any{}}, "structured_output": output,
		}
	}
	proposalOpt := cmpopts.IgnoreFields(review.Proposal{}, "Original", "Lines")

	tests := []struct {
		name        string
		result      map[string]any
		status      int
		want        review.Result
		wantOrig    string
		wantInvalid bool
		wantModels  []string
	}{
		{
			name:       "no impact",
			result:     success(map[string]any{"no_impact_reason": "constant only", "proposals": []any{}}),
			want:       review.Result{Model: "stub-model", Verdict: review.NoImpact{Reason: "constant only"}},
			wantModels: []string{"stub-model"},
		},
		{
			name:   "proposal anchored in the diff",
			result: success(map[string]any{"no_impact_reason": "", "proposals": []any{proposal}}),
			want: review.Result{Model: "stub-model", Verdict: review.Proposals{{
				DocPath: "docs/features/thing.md", Section: "Retention",
				Anchor: review.Anchor{File: "src/thing.go", Line: 3}, Reason: "retention changed", Content: "## Retention\n\nKeeps 30 days.\n",
			}}},
			wantOrig:   "Keeps 7 days.",
			wantModels: []string{"stub-model"},
		},
		{
			name:        "claude error with a failing script",
			result:      map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "api_error_status": 529, "modelUsage": map[string]any{}},
			status:      1,
			wantInvalid: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := claudeSandbox{path: stubClaude(t, tc.result, tc.status) + string(os.PathListSeparator) + cfg.Path}
			logDir := t.TempDir()

			got, err := actionsAttempt(root, logDir, sandbox)(ctx, c, in, 1)
			if err != nil {
				t.Fatalf("attempt error = %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(logDir, "stub-1.transcript.jsonl")); statErr != nil {
				t.Errorf("transcript not copied to the logs dir: %v", statErr)
			}
			if d := cmp.Diff(tc.wantModels, got.Models); d != "" {
				t.Errorf("models mismatch (-want +got):\n%s", d)
			}

			if tc.wantInvalid {
				var invalid *review.InvalidResultError
				if !errors.As(got.Err, &invalid) {
					t.Fatalf("attempt Err = %v, want *review.InvalidResultError", got.Err)
				}
				return
			}
			if got.Err != nil {
				t.Fatalf("attempt Err = %v", got.Err)
			}
			if d := cmp.Diff(tc.want, got.Result, proposalOpt); d != "" {
				t.Errorf("result mismatch (-want +got):\n%s", d)
			}
			if props, ok := got.Result.Verdict.(review.Proposals); ok && tc.wantOrig != "" {
				if !strings.Contains(props[0].Original, tc.wantOrig) || props[0].Lines.Start == 0 {
					t.Errorf("proposal Original = %q at %+v, want the head section containing %q", props[0].Original, props[0].Lines, tc.wantOrig)
				}
			}
		})
	}
}
