// Package actions is the review runner that analyzes a pull request inside
// the target repo's own GitHub Actions workflow and reads the result back
// from the run's artifact.
package actions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

const (
	runnerName   = "actions"
	defaultModel = "claude-code"
)

// DispatchInputs are the workflow_dispatch inputs of the docs-agent workflow.
type DispatchInputs struct {
	HeadSHA  string
	PRNumber int
	Nonce    string
}

// WorkflowAPI is the GitHub Actions surface the runner needs.
type WorkflowAPI interface {
	// Dispatch starts the docs-agent workflow on the repo's default branch and
	// returns the ID of the run it created.
	Dispatch(ctx context.Context, installationID int64, owner, repo string, in DispatchInputs) (runID int64, err error)
	// ResultArtifact returns the result.json bytes of the run's result artifact.
	ResultArtifact(ctx context.Context, installationID int64, owner, repo string, runID int64) ([]byte, error)
	// ChangedFiles returns the pull request's files with their head-side hunk ranges.
	ChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error)
}

// Artifact is the JSON document the workflow uploads as result.json.
type Artifact struct {
	HeadSHA string       `json:"head_sha"`
	Nonce   string       `json:"nonce"`
	Claude  ClaudeOutput `json:"claude"`
}

// ClaudeOutput is the subset of `claude -p --output-format json` stdout the
// runner reads.
type ClaudeOutput struct {
	IsError          bool                       `json:"is_error"`
	Result           string                     `json:"result"`
	ModelUsage       map[string]json.RawMessage `json:"modelUsage"`
	StructuredOutput *StructuredOutput          `json:"structured_output"`
}

// StructuredOutput is the schema-enforced analysis: no impact (with a reason)
// or proposals.
type StructuredOutput struct {
	NoImpactReason string            `json:"no_impact_reason"`
	Proposals      []review.Proposal `json:"proposals"`
}

// Runner dispatches the repo's docs-agent workflow and collects its result.
type Runner struct {
	api     WorkflowAPI
	timeout time.Duration
}

var _ review.AsyncRunner = (*Runner)(nil)

// New returns a Runner that dispatches through api and gives each run timeout
// to complete.
func New(api WorkflowAPI, timeout time.Duration) *Runner {
	return &Runner{api: api, timeout: timeout}
}

// Start dispatches the workflow for req and returns review.Pending.
func (r *Runner) Start(ctx context.Context, req review.Request) (review.Started, error) {
	nonce, err := newNonce()
	if err != nil {
		return nil, fmt.Errorf("start actions run %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err)
	}

	runID, err := r.api.Dispatch(ctx, req.InstallationID, req.Owner, req.Repo, DispatchInputs{
		HeadSHA:  req.HeadSHA,
		PRNumber: req.Number,
		Nonce:    nonce,
	})
	if err != nil {
		return nil, fmt.Errorf("start actions run %s/%s#%d: dispatch: %w", req.Owner, req.Repo, req.Number, err)
	}

	return review.Pending{RunID: runID, Nonce: nonce, Deadline: time.Now().Add(r.timeout)}, nil
}

// Collect decodes the completed run's result artifact. It returns
// *review.InvalidResultError when the artifact is for another head or
// dispatch, reports an error, or holds a malformed result or a proposal
// outside the PR's docs or diff. Failing to list the PR's files is transient
// and returned as an ordinary error.
func (r *Runner) Collect(ctx context.Context, c review.Completion) (review.Result, error) {
	raw, err := r.api.ResultArtifact(ctx, c.InstallationID, c.Owner, c.Repo, c.RunID)
	if err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	var art Artifact
	if err := json.Unmarshal(raw, &art); err != nil {
		return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("decode result artifact: %w", err)}
	}

	out, err := art.output(c)
	if err != nil {
		return review.Result{}, &review.InvalidResultError{Cause: err}
	}

	result := review.Result{Runner: runnerName, Model: art.Claude.model()}
	if len(out.Proposals) == 0 {
		if strings.TrimSpace(out.NoImpactReason) == "" {
			return review.Result{}, &review.InvalidResultError{Cause: errors.New("no proposals and an empty no_impact_reason")}
		}
		result.Verdict = review.NoImpact{Reason: out.NoImpactReason}
		return result, nil
	}

	changed, err := r.api.ChangedFiles(ctx, c.InstallationID, c.Owner, c.Repo, c.Number)
	if err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	proposals := make(review.Proposals, len(out.Proposals))
	for i, p := range out.Proposals {
		if err := p.Validate(changed); err != nil {
			return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("proposal %d: %w", i, err)}
		}
		proposals[i] = p
	}
	result.Verdict = proposals
	return result, nil
}

func (a Artifact) output(c review.Completion) (*StructuredOutput, error) {
	if a.HeadSHA != c.HeadSHA {
		return nil, fmt.Errorf("artifact head_sha %q, want %q", a.HeadSHA, c.HeadSHA)
	}
	if a.Nonce != c.Nonce {
		return nil, errors.New("artifact nonce does not match the dispatch")
	}
	if a.Claude.IsError {
		return nil, fmt.Errorf("claude reported an error: %s", a.Claude.Result)
	}
	if a.Claude.StructuredOutput == nil {
		return nil, errors.New("claude output has no structured_output")
	}
	return a.Claude.StructuredOutput, nil
}

func (o ClaudeOutput) model() string {
	if models := slices.Sorted(maps.Keys(o.ModelUsage)); len(models) > 0 {
		return models[0]
	}
	return defaultModel
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
