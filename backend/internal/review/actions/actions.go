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
	// Uncovered are the changed files no base doc covers; the run decides
	// whether they need a new doc.
	Uncovered []string
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
	// does not exist there or exceeds docs.MaxDocBytes.
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
	api             WorkflowAPI
	timeout         time.Duration
	scaffoldTimeout time.Duration
}

var (
	_ review.AsyncRunner     = (*Runner)(nil)
	_ review.AsyncScaffolder = (*Runner)(nil)
)

// New returns a Runner that dispatches through api and gives each review run
// timeout and each scaffold run scaffoldTimeout to complete.
func New(api WorkflowAPI, timeout, scaffoldTimeout time.Duration) *Runner {
	return &Runner{api: api, timeout: timeout, scaffoldTimeout: scaffoldTimeout}
}

// Start computes the candidate docs from the PR's base commit and dispatches
// the workflow to review them, returning review.Pending. When the PR deletes a
// candidate and a restore can be proposed, it returns the finished
// review.Result of restore proposals without dispatching.
func (r *Runner) Start(ctx context.Context, req review.Request) (review.Started, error) {
	where := fmt.Sprintf("%s/%s#%d", req.Owner, req.Repo, req.Number)

	selection, err := r.selectAtBase(ctx, req.InstallationID, req.Owner, req.Repo, req.BaseSHA, req.ChangedFiles)
	if err != nil {
		return nil, fmt.Errorf("start actions run %s: %w", where, err)
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

	if selection.Empty() {
		return review.Result{Runner: runnerName, Verdict: review.NoImpact{Reason: basedocs.NothingToReview}}, nil
	}
	pending, err := r.dispatch(ctx, r.timeout, req.InstallationID, req.Owner, req.Repo, req.HeadSHA, req.Number, selection.Candidates, selection.Uncovered)
	if err != nil {
		return nil, fmt.Errorf("start actions run %s: %w", where, err)
	}
	return pending, nil
}

// selectAtBase picks the candidate, uncovered, and restorable docs from the
// docs tree at sha.
func (r *Runner) selectAtBase(ctx context.Context, installationID int64, owner, repo, sha string, changed []review.ChangedFile) (basedocs.Selection, error) {
	baseFS, err := r.api.DocsAtRef(ctx, installationID, owner, repo, sha)
	if err != nil {
		return basedocs.Selection{}, fmt.Errorf("base %s: %w", sha, err)
	}
	selection, err := basedocs.Select(baseFS, changed)
	if err != nil {
		return basedocs.Selection{}, fmt.Errorf("base %s: %w", sha, err)
	}
	return selection, nil
}

// StartScaffold dispatches the workflow with pr_number 0 and no docs at
// req.BaseSHA and returns review.Pending.
func (r *Runner) StartScaffold(ctx context.Context, req review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	pending, err := r.dispatch(ctx, r.scaffoldTimeout, req.InstallationID, req.Owner, req.Repo, req.BaseSHA, 0, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("start actions scaffold %s/%s: %w", req.Owner, req.Repo, err)
	}
	return pending, nil
}

func (r *Runner) dispatch(ctx context.Context, timeout time.Duration, installationID int64, owner, repo, sha string, number int, docs, uncovered []string) (review.Pending, error) {
	nonce, err := newNonce()
	if err != nil {
		return review.Pending{}, err
	}

	runID, err := r.api.Dispatch(ctx, installationID, owner, repo, DispatchInputs{HeadSHA: sha, PRNumber: number, Nonce: nonce, Docs: docs, Uncovered: uncovered})
	if err != nil {
		return review.Pending{}, fmt.Errorf("dispatch: %w", err)
	}

	return review.Pending{RunID: runID, Nonce: nonce, Deadline: time.Now().Add(timeout)}, nil
}

// CollectScaffold decodes the completed run's result artifact. It returns
// *review.InvalidResultError when the artifact is for another commit or
// dispatch, reports an error, or holds docs that fail docs.CheckScaffold.
func (r *Runner) CollectScaffold(ctx context.Context, c review.Completion) (review.Scaffold, error) {
	raw, err := r.api.ResultArtifact(ctx, c.InstallationID, c.Owner, c.Repo, c.RunID)
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("collect actions scaffold run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	var art Artifact[review.ScaffoldDocs]
	if err := json.Unmarshal(raw, &art); err != nil {
		return review.Scaffold{}, &review.InvalidResultError{Cause: fmt.Errorf("decode result artifact: %w", err)}
	}

	out, err := art.output(c)
	if err != nil {
		return review.Scaffold{}, &review.InvalidResultError{Cause: err}
	}
	if err := docs.CheckScaffold(out.Index, out.Architecture, out.Setup, c.Owner+"/"+c.Repo); err != nil {
		return review.Scaffold{}, &review.InvalidResultError{Cause: errors.New(capText(err.Error()))}
	}

	return review.Scaffold{
		Runner:       runnerName,
		Model:        art.Claude.model(),
		Index:        out.Index,
		Architecture: out.Architecture,
		Setup:        out.Setup,
	}, nil
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

	var art Artifact[review.StructuredOutput]
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

	changed, err := r.api.ListChangedFiles(ctx, c.InstallationID, c.Owner, c.Repo, c.Number)
	if err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	// A run started before its merge base was stored can't have a new doc's
	// covers checked, so new docs from it are refused rather than trusted.
	validate := func(p review.Proposal) error {
		if p.Section == "" {
			return errors.New("new doc: the run has no stored merge base to check its covers against")
		}
		return p.Validate(changed)
	}
	if c.BaseSHA != "" {
		selection, err := r.selectAtBase(ctx, c.InstallationID, c.Owner, c.Repo, c.BaseSHA, changed)
		if err != nil {
			return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
		}
		validate = func(p review.Proposal) error { return selection.ValidateProposal(p, changed, c.Owner+"/"+c.Repo) }
	}

	proposals := make(review.Proposals, len(out.Proposals))
	for i, p := range out.Proposals {
		if err := validate(p); err != nil {
			return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("proposal %d: %s", i, capText(err.Error()))}
		}
		if p.Section == "" {
			_, exists, err := r.api.FileAtRef(ctx, c.InstallationID, c.Owner, c.Repo, p.DocPath, c.HeadSHA)
			if err != nil {
				return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: read %s at %s: %w", c.RunID, c.Owner, c.Repo, p.DocPath, c.HeadSHA, err)
			}
			if exists {
				return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("proposal %d: new doc %s already exists at head", i, p.DocPath)}
			}
		}
		proposals[i] = p
	}
	if err := r.fillOriginals(ctx, c, proposals); err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}
	result.Verdict = proposals
	return result, nil
}

// fillOriginals sets Original and Lines on each proposal that replaces a
// section, from the doc at the completion's head. A doc or section missing
// there leaves both empty.
func (r *Runner) fillOriginals(ctx context.Context, c review.Completion, proposals review.Proposals) error {
	parsed := map[string]*docs.Doc{}
	for i, p := range proposals {
		if p.Section == "" {
			continue
		}
		doc, seen := parsed[p.DocPath]
		if !seen {
			src, ok, err := r.api.FileAtRef(ctx, c.InstallationID, c.Owner, c.Repo, p.DocPath, c.HeadSHA)
			if err != nil {
				return fmt.Errorf("read %s at %s: %w", p.DocPath, c.HeadSHA, err)
			}
			if ok {
				if d, err := docs.ParseDoc(p.DocPath, src); err == nil {
					doc = &d
				}
			}
			parsed[p.DocPath] = doc
		}
		if doc == nil {
			continue
		}
		if text, start, end, ok := doc.SectionSpan(p.Section); ok {
			proposals[i].Original, proposals[i].Lines = text, review.LineRange{Start: start, End: end}
		}
	}
	return nil
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
		status, capText(o.TerminalReason), capText(o.Subtype))
}

func capText(s string) string {
	if len(s) <= maxCauseText {
		return s
	}
	return strings.ToValidUTF8(s[:maxCauseText], "") + "..."
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
