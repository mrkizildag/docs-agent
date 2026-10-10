package llmrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// Caps on one scaffold run, which reads the whole repo rather than one diff.
const (
	scaffoldStepCap     = 40
	scaffoldTokenBudget = 600_000
	scaffoldTimeout     = 8 * time.Minute
)

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
	c, cleanup, err := r.openClone(ctx, req.InstallationID, req.Owner, req.Repo, req.BaseSHA)
	if err != nil {
		return review.Scaffold{}, err
	}
	defer cleanup()

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
		System:   scaffoldSystemPrompt(),
		Prompt:   scaffoldUserPrompt(f, req.Owner, req.Repo, req.BaseSHA),
		Root:     c.root,
		Finish:   finish,
		Accept:   checkSubmittedDocs(req.Owner + "/" + req.Repo),
		MaxSteps: scaffoldStepCap,
		Log:      r.log.With("repo", req.Owner+"/"+req.Repo, "scaffold_sha", req.BaseSHA),
	}, agent.NewBudget(scaffoldTokenBudget))
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("write docs: %w: %w", errProvider, err)
	}

	var submitted review.ScaffoldDocs
	if err := json.Unmarshal(raw, &submitted); err != nil {
		return review.Scaffold{}, fmt.Errorf("decode accepted submit_docs arguments: %w", err)
	}
	return review.Scaffold{Runner: runnerName, Model: r.model, Index: submitted.Index, Architecture: submitted.Architecture, Setup: submitted.Setup}, nil
}

// checkSubmittedDocs is the finishing tool's Accept for repo ("owner/repo").
func checkSubmittedDocs(repo string) func(json.RawMessage) error {
	return func(args json.RawMessage) error {
		var d review.ScaffoldDocs
		if err := json.Unmarshal(args, &d); err != nil {
			return fmt.Errorf("decode submit_docs arguments: %w", err)
		}
		if err := docs.CheckScaffold(d.Index, d.Architecture, d.Setup, repo); err != nil {
			return fmt.Errorf("submit_docs: %w", err)
		}
		return nil
	}
}

func submitDocsTool() (llm.Tool, error) {
	schema, err := review.ScaffoldSchema()
	if err != nil {
		return llm.Tool{}, fmt.Errorf("build submit_docs schema: %w", err)
	}
	return llm.Tool{
		Name:        "submit_docs",
		Description: "Submit the three starting docs for this repository. Call exactly once when they are final.",
		Schema:      schema,
	}, nil
}
