package llmrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// submitDocsArgs is the argument shape of the submit_docs finishing tool.
type submitDocsArgs struct {
	Index        string `json:"index"`
	Architecture string `json:"architecture"`
	Setup        string `json:"setup"`
}

// StartScaffold implements review.Scaffolder: an agent loop over a depth-1
// clone of req.BaseSHA writes the three scaffold docs. It always returns a
// review.Scaffold; every error is a *review.FailedError whose Err keeps the
// original chain.
func (r *Runner) StartScaffold(ctx context.Context, req review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	ctx, cancel := context.WithTimeout(ctx, scaffoldTimeout)
	defer cancel()

	res, err := r.scaffold(ctx, req)
	if err != nil {
		return nil, failed(fmt.Errorf("start scaffold %s/%s: %w", req.Owner, req.Repo, err))
	}
	return res, nil
}

func (r *Runner) scaffold(ctx context.Context, req review.ScaffoldRequest) (review.Scaffold, error) {
	token, err := r.token(ctx, req.InstallationID, req.Repo)
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("get installation token: %w: %w", errClone, err)
	}

	remoteURL := r.remote
	if remoteURL == "" {
		remoteURL = fmt.Sprintf("https://github.com/%s/%s.git", req.Owner, req.Repo)
	}

	dir, err := cloneHead(ctx, remoteURL, req.BaseSHA, token)
	if dir != "" {
		defer func() { _ = os.RemoveAll(dir) }() // best-effort cleanup of a temp dir; the runner has no logger
	}
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("%w: %w", errClone, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("open clone root: %w", err)
	}
	defer func() { _ = root.Close() }()

	finish, err := submitDocsTool()
	if err != nil {
		return review.Scaffold{}, err
	}
	f, err := newFence()
	if err != nil {
		return review.Scaffold{}, err
	}

	raw, _, err := agent.Run(ctx, r.m, agent.Task{
		Model:    r.model,
		System:   scaffoldSystemPrompt,
		Prompt:   scaffoldUserPrompt(f, req.Owner, req.Repo, req.BaseSHA),
		Root:     root,
		Finish:   finish,
		Accept:   checkSubmittedDocs,
		MaxSteps: scaffoldStepCap,
	}, agent.NewBudget(scaffoldTokenBudget))
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("write docs: %w: %w", errProvider, err)
	}

	var submitted submitDocsArgs
	if err := json.Unmarshal(raw, &submitted); err != nil {
		return review.Scaffold{}, fmt.Errorf("decode accepted submit_docs arguments: %w", err)
	}
	return review.Scaffold{Runner: runnerName, Model: r.model, Index: submitted.Index, Architecture: submitted.Architecture, Setup: submitted.Setup}, nil
}

// checkSubmittedDocs is the finishing tool's Accept.
func checkSubmittedDocs(args json.RawMessage) error {
	var d submitDocsArgs
	if err := json.Unmarshal(args, &d); err != nil {
		return fmt.Errorf("decode submit_docs arguments: %w", err)
	}
	if err := docs.CheckScaffold(d.Index, d.Architecture, d.Setup); err != nil {
		return fmt.Errorf("submit_docs: %w", err)
	}
	return nil
}

func submitDocsTool() (llm.Tool, error) {
	text := func(what string) map[string]string {
		return map[string]string{"type": "string", "description": what}
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"index":        text("Full markdown of docs/README.md, frontmatter included."),
			"architecture": text("Full markdown of docs/architecture.md, frontmatter included."),
			"setup":        text("Full markdown of docs/guides/setup.md, frontmatter included."),
		},
		"required": []string{"index", "architecture", "setup"},
	})
	if err != nil {
		return llm.Tool{}, fmt.Errorf("marshal submit_docs schema: %w", err)
	}
	return llm.Tool{
		Name:        "submit_docs",
		Description: "Submit the three starting docs for this repository. Call exactly once when they are final.",
		Schema:      schema,
	}, nil
}
