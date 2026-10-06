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
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
)

const (
	runnerName   = "actions"
	defaultModel = "claude-code"

	// maxCauseText bounds model-controlled text in an InvalidResultError, which
	// becomes a public check-run summary.
	maxCauseText = 200
)

// DispatchInputs are the workflow_dispatch inputs of the pollux-agent workflow.
type DispatchInputs struct {
	HeadSHA  string
	PRNumber int
	Nonce    string
	// Docs are the candidate doc paths the run must review.
	Docs []string
}

// WorkflowAPI is the GitHub Actions surface the runner needs.
type WorkflowAPI interface {
	// Dispatch starts the pollux-agent workflow on the repo's default branch and
	// returns the ID of the run it created.
	Dispatch(ctx context.Context, installationID int64, owner, repo string, in DispatchInputs) (runID int64, err error)
	// ResultArtifact returns the result.json bytes of the run's result artifact.
	ResultArtifact(ctx context.Context, installationID int64, owner, repo string, runID int64) ([]byte, error)
	// ListChangedFiles returns the pull request's files with their head-side hunk ranges.
	ListChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error)
	// FileAtRef returns the file's content at ref, or ok=false when the file
	// does not exist there. A file over docs.MaxDocBytes is an error wrapping
	// review.ErrFileTooLarge.
	FileAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (content []byte, ok bool, err error)
	// DocsAtRef returns the .md files under docs/ at ref, rooted at the repo root.
	DocsAtRef(ctx context.Context, installationID int64, owner, repo, ref string) (fs.FS, error)
}

// Artifact is the JSON document the workflow uploads as result.json.
type Artifact[T any] struct {
	HeadSHA string          `json:"head_sha"`
	Nonce   string          `json:"nonce"`
	Claude  ClaudeOutput[T] `json:"claude"`
}

// ClaudeOutput is the subset of `claude -p --output-format json` stdout the
// runner reads.
type ClaudeOutput[T any] struct {
	IsError          bool                       `json:"is_error"`
	Subtype          string                     `json:"subtype"`
	TerminalReason   string                     `json:"terminal_reason"`
	APIErrorStatus   *int                       `json:"api_error_status"`
	ModelUsage       map[string]json.RawMessage `json:"modelUsage"`
	StructuredOutput *T                         `json:"structured_output"`
}

// Runner dispatches the repo's pollux-agent workflow and collects its result.
type Runner struct {
	api     WorkflowAPI
	timeout time.Duration
}

var (
	_ review.AsyncRunner     = (*Runner)(nil)
	_ review.AsyncScaffolder = (*Runner)(nil)
)

// New returns a Runner that dispatches through api and gives each run timeout
// to complete.
func New(api WorkflowAPI, timeout time.Duration) *Runner {
	return &Runner{api: api, timeout: timeout}
}

// Start computes the candidate docs from the PR's base commit and dispatches
// the workflow to review them, returning review.Pending. When the PR deletes a
// candidate and a restore can be proposed, it returns the finished
// review.Result of restore proposals without dispatching.
func (r *Runner) Start(ctx context.Context, req review.Request) (review.Started, error) {
	where := fmt.Sprintf("%s/%s#%d", req.Owner, req.Repo, req.Number)

	baseFS, err := r.api.DocsAtRef(ctx, req.InstallationID, req.Owner, req.Repo, req.BaseSHA)
	if err != nil {
		return nil, fmt.Errorf("start actions run %s: %w", where, err)
	}
	selection, err := basedocs.Select(baseFS, req.ChangedFiles)
	if err != nil {
		return nil, fmt.Errorf("start actions run %s: base %s: %w", where, req.BaseSHA, err)
	}
	if len(selection.Restores) > 0 {
		return review.Result{Runner: runnerName, Verdict: review.Proposals(selection.Restores)}, nil
	}
	if len(selection.Candidates) > basedocs.MaxCandidates {
		return nil, &review.FailedError{
			Cause: review.CauseTooManyCandidates,
			Err:   fmt.Errorf("start actions run %s: %d candidate docs exceed the cap of %d", where, len(selection.Candidates), basedocs.MaxCandidates),
		}
	}

	pending, err := r.dispatch(ctx, req.InstallationID, req.Owner, req.Repo, req.HeadSHA, req.Number, selection.Candidates)
	if err != nil {
		return nil, fmt.Errorf("start actions run %s: %w", where, err)
	}
	return pending, nil
}

// StartScaffold dispatches the workflow with pr_number 0 and no docs at
// req.BaseSHA and returns review.Pending.
func (r *Runner) StartScaffold(ctx context.Context, req review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	pending, err := r.dispatch(ctx, req.InstallationID, req.Owner, req.Repo, req.BaseSHA, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("start actions scaffold %s/%s: %w", req.Owner, req.Repo, err)
	}
	return pending, nil
}

func (r *Runner) dispatch(ctx context.Context, installationID int64, owner, repo, sha string, number int, docs []string) (review.Pending, error) {
	nonce, err := newNonce()
	if err != nil {
		return review.Pending{}, err
	}

	runID, err := r.api.Dispatch(ctx, installationID, owner, repo, DispatchInputs{HeadSHA: sha, PRNumber: number, Nonce: nonce, Docs: docs})
	if err != nil {
		return review.Pending{}, fmt.Errorf("dispatch: %w", err)
	}

	return review.Pending{RunID: runID, Nonce: nonce, Deadline: time.Now().Add(r.timeout)}, nil
}

// CollectScaffold decodes the completed run's result artifact. It returns
// *review.InvalidResultError when the artifact is for another commit or
// dispatch, reports an error, or holds docs that fail docs.CheckScaffold.
func (r *Runner) CollectScaffold(ctx context.Context, c review.Completion) (review.Scaffold, error) {
	out, model, err := collectOutput[review.ScaffoldDocs](ctx, r.api, c, "scaffold run")
	if err != nil {
		return review.Scaffold{}, err
	}

	files := out.Files()
	scaffold := make([]docs.ScaffoldFile, len(files))
	for i, f := range files {
		scaffold[i] = docs.ScaffoldFile(f)
	}
	if err := docs.CheckScaffold(scaffold, review.IndexPath, review.IndexHeading); err != nil {
		return review.Scaffold{}, &review.InvalidResultError{Cause: errors.New(review.Truncate(err.Error(), maxCauseText))}
	}

	return review.Scaffold{
		Runner:       runnerName,
		Model:        model,
		Index:        out.Index,
		Architecture: out.Architecture,
		Setup:        out.Setup,
	}, nil
}

// collectOutput reads the run's result artifact and returns its structured
// output and model. kind names the run in errors. It returns
// *review.InvalidResultError when the artifact is unusable; failing to read it
// is transient and returned as an ordinary error.
func collectOutput[T any](ctx context.Context, api WorkflowAPI, c review.Completion, kind string) (out *T, model string, err error) {
	raw, err := api.ResultArtifact(ctx, c.InstallationID, c.Owner, c.Repo, c.RunID)
	if err != nil {
		return nil, "", fmt.Errorf("collect actions %s %d of %s/%s: %w", kind, c.RunID, c.Owner, c.Repo, err)
	}

	var art Artifact[T]
	if err := json.Unmarshal(raw, &art); err != nil {
		return nil, "", &review.InvalidResultError{Cause: fmt.Errorf("decode result artifact: %w", err)}
	}

	out, err = art.output(c)
	if err != nil {
		return nil, "", &review.InvalidResultError{Cause: err}
	}
	return out, art.Claude.model(), nil
}

// Collect decodes the completed run's result artifact. It returns
// *review.InvalidResultError when the artifact is for another head or
// dispatch, reports an error, or holds a malformed result or a proposal
// outside the PR's docs or diff. Failing to list the PR's files is transient
// and returned as an ordinary error.
func (r *Runner) Collect(ctx context.Context, c review.Completion) (review.Result, error) {
	out, model, err := collectOutput[review.StructuredOutput](ctx, r.api, c, "run")
	if err != nil {
		return review.Result{}, err
	}

	result := review.Result{Runner: runnerName, Model: model}
	if len(out.Proposals) == 0 {
		if strings.TrimSpace(out.NoImpactReason) == "" {
			return review.Result{}, &review.InvalidResultError{Cause: errors.New("no proposals and an empty no_impact_reason")}
		}
		result.Verdict = review.NoImpact{Reason: out.NoImpactReason}
		return result, nil
	}

	changed, err := r.api.ListChangedFiles(ctx, c.InstallationID, c.Owner, c.Repo, c.Number)
	if err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	proposals := make(review.Proposals, len(out.Proposals))
	for i, p := range out.Proposals {
		p.Section = review.NormalizeSection(p.Section)
		if err := p.Validate(changed); err != nil {
			return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("proposal %d: %s", i, review.Truncate(err.Error(), maxCauseText))}
		}
		proposals[i] = p
	}
	if err := r.fillOriginals(ctx, c, proposals); err != nil {
		var invalid *review.InvalidResultError
		if errors.As(err, &invalid) {
			return review.Result{}, err
		}
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}
	result.Verdict = proposals
	return result, nil
}

// fillOriginals sets Original and Lines on each proposal that replaces a
// section, from the doc at the completion's head. It returns
// *review.InvalidResultError when the doc is missing there, does not parse, or
// has no single heading matching the section. A doc over docs.MaxDocBytes
// leaves both empty.
func (r *Runner) fillOriginals(ctx context.Context, c review.Completion, proposals review.Proposals) error {
	parsed := map[string]*docs.Doc{}
	for i := range proposals {
		p := &proposals[i]
		if p.Section == "" {
			continue
		}

		doc, seen := parsed[p.DocPath]
		if !seen {
			d, readable, err := r.headDoc(ctx, c, p.DocPath)
			if err != nil {
				return err
			}
			if readable {
				doc = &d
			}
			parsed[p.DocPath] = doc
		}
		if doc == nil {
			continue
		}
		if err := basedocs.FillOriginal(p, *doc); err != nil {
			return &review.InvalidResultError{Cause: fmt.Errorf("proposal for %s: %s", p.DocPath, review.Truncate(err.Error(), maxCauseText))}
		}
	}
	return nil
}

// headDoc parses docPath at the completion's head. readable is false when the
// doc is too large to read.
func (r *Runner) headDoc(ctx context.Context, c review.Completion, docPath string) (doc docs.Doc, readable bool, err error) {
	src, ok, err := r.api.FileAtRef(ctx, c.InstallationID, c.Owner, c.Repo, docPath, c.HeadSHA)
	switch {
	case errors.Is(err, review.ErrFileTooLarge):
		return docs.Doc{}, false, nil
	case err != nil:
		return docs.Doc{}, false, fmt.Errorf("read %s at %s: %w", docPath, c.HeadSHA, err)
	case !ok:
		return docs.Doc{}, false, &review.InvalidResultError{Cause: fmt.Errorf("proposal replaces a section of %s, which does not exist at head; leave section empty to create a new doc", docPath)}
	}

	doc, err = docs.ParseDoc(docPath, src)
	if err != nil {
		return docs.Doc{}, false, &review.InvalidResultError{Cause: fmt.Errorf("proposal replaces a section of %s, which does not parse: %s", docPath, review.Truncate(err.Error(), maxCauseText))}
	}
	return doc, true, nil
}

// output returns the structured output of an artifact that belongs to c's
// dispatch and reports no error.
func (a Artifact[T]) output(c review.Completion) (*T, error) {
	if a.HeadSHA != c.HeadSHA {
		return nil, fmt.Errorf("artifact head_sha %q, want %q", a.HeadSHA, c.HeadSHA)
	}
	if a.Nonce != c.Nonce {
		return nil, errors.New("artifact nonce does not match the dispatch")
	}
	if a.Claude.IsError {
		return nil, a.Claude.failure()
	}
	if a.Claude.StructuredOutput == nil {
		return nil, errors.New("claude output has no structured_output")
	}
	return a.Claude.StructuredOutput, nil
}

// failure describes an errored run from structured fields only; the free-form
// result text is attacker-influenced and must not reach a public check run.
func (o ClaudeOutput[T]) failure() error {
	status := "none"
	if o.APIErrorStatus != nil {
		status = strconv.Itoa(*o.APIErrorStatus)
	}
	return fmt.Errorf("claude code failed: api_error_status %s (terminal_reason %s, subtype %s)",
		status, review.Truncate(o.TerminalReason, maxCauseText), review.Truncate(o.Subtype, maxCauseText))
}

func (o ClaudeOutput[T]) model() string {
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
