// Package llmrunner implements review.Runner for the server: a small-model
// triage call per candidate doc, then an agent loop that drafts proposals
// over a depth-1 clone of the pull request's head commit. Candidates are the
// docs whose covers at the base commit match the changed files, so a PR can't
// opt a doc out by editing its own covers.
package llmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
)

const runnerName = "llmrunner"

// Caps on one analysis run, sized from measured runs on the Gemini free tier
// (1-3 steps, at most 46k tokens).
const (
	stepCap         = 12
	tokenBudget     = 120_000
	analysisTimeout = 150 * time.Second
)

// limits caps one agent run: its steps, total tokens, and wall-clock time.
type limits struct {
	steps   int
	tokens  int
	timeout time.Duration
}

// Runner implements review.Runner by triaging candidate docs with a small
// model and drafting proposals with an agent loop.
type Runner struct {
	m           llm.Model
	token       func(ctx context.Context, installationID int64, repo string) (string, error)
	triageModel string
	model       string

	analysisLimits limits
	scaffoldLimits limits
	log            *slog.Logger

	// Test override; see export_test.go.
	remote string
}

var (
	_ review.Runner     = (*Runner)(nil)
	_ review.Scaffolder = (*Runner)(nil)
)

// New returns a Runner that triages with triageModel, drafts with model, both
// served by m, and authenticates clones with a token from token.
func New(m llm.Model, token func(ctx context.Context, installationID int64, repo string) (string, error), triageModel, model string) *Runner {
	return &Runner{
		m: m, token: token, triageModel: triageModel, model: model,
		analysisLimits: limits{steps: stepCap, tokens: tokenBudget, timeout: analysisTimeout},
		scaffoldLimits: limits{steps: scaffoldStepCap, tokens: scaffoldTokenBudget, timeout: scaffoldTimeout},
		log:            slog.Default(),
	}
}

var (
	errProvider          = errors.New("model call failed or returned an unusable reply")
	errClone             = errors.New("clone failed")
	errTooManyCandidates = errors.New("too many candidate docs")
)

// Start implements review.Runner. Every error it returns is a
// *review.FailedError whose Err keeps the original chain.
func (r *Runner) Start(ctx context.Context, req review.Request) (review.Started, error) {
	if len(req.ChangedFiles) == 0 {
		return r.noImpact("no changed files"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.analysisLimits.timeout)
	defer cancel()

	res, err := r.analyze(ctx, req)
	if err != nil {
		return nil, failed(fmt.Errorf("start analysis %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err))
	}
	return res, nil
}

// failed classifies err into the *review.FailedError a runner returns.
func failed(err error) *review.FailedError {
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, agent.ErrDeadline) {
		err = fmt.Errorf("%w: %w", agent.ErrDeadline, err)
	}
	return &review.FailedError{Cause: classify(err), Err: err}
}

func classify(err error) review.FailureCause {
	switch {
	case errors.Is(err, agent.ErrStepLimit), errors.Is(err, agent.ErrTokenBudget):
		return review.CauseLimit
	case errors.Is(err, agent.ErrDeadline):
		return review.CauseTimeout
	case errors.Is(err, errTooManyCandidates):
		return review.CauseTooManyCandidates
	case errors.Is(err, errClone):
		return review.CauseClone
	case errors.Is(err, errProvider), errors.Is(err, agent.ErrModel):
		return review.CauseProvider
	default:
		return review.CauseInternal
	}
}

func (r *Runner) noImpact(reason string) review.Result {
	return review.Result{Runner: runnerName, Model: r.model, Verdict: review.NoImpact{Reason: reason}}
}

func (r *Runner) analyze(ctx context.Context, req review.Request) (review.Result, error) {
	c, cleanup, err := r.openClone(ctx, req.InstallationID, req.Owner, req.Repo, req.HeadSHA, req.BaseSHA)
	if err != nil {
		return review.Result{}, err
	}
	defer cleanup()
	root := c.root

	baseFS, err := c.docsAt(ctx, req.BaseSHA)
	if err != nil {
		return review.Result{}, fmt.Errorf("%w: %w", errClone, err)
	}

	selection, err := basedocs.Select(baseFS, req.ChangedFiles)
	if err != nil {
		return review.Result{}, fmt.Errorf("base docs of %s: %w", req.BaseSHA, err)
	}
	r.logProblems(ctx, "base", req.BaseSHA, selection.Problems)
	if len(selection.Restores) > 0 {
		return review.Result{Runner: runnerName, Model: "", Verdict: review.Proposals(selection.Restores)}, nil
	}
	candidates := selection.Candidates
	if len(candidates) == 0 {
		return r.noImpact("no doc covers the changed files"), nil
	}
	if len(candidates) > basedocs.MaxCandidates {
		return review.Result{}, fmt.Errorf("%w: %d candidate docs exceed the cap of %d", errTooManyCandidates, len(candidates), basedocs.MaxCandidates)
	}

	headTree, err := docs.Parse(root.FS())
	if err != nil {
		return review.Result{}, fmt.Errorf("parse docs of %s: %w", req.HeadSHA, err)
	}
	r.logProblems(ctx, "head", req.HeadSHA, headTree.Problems)
	index := make(docIndex, len(headTree.Docs))
	for _, d := range headTree.Docs {
		index[d.Path] = d
	}
	for _, docPath := range candidates {
		if _, ok := index[docPath]; ok {
			continue
		}
		d, err := headDoc(root, docPath)
		if err != nil {
			return review.Result{}, err
		}
		index[docPath] = d
	}

	fence, err := newFence()
	if err != nil {
		return review.Result{}, err
	}
	budget := agent.NewBudget(r.analysisLimits.tokens)
	patch := combinedPatch(req.ChangedFiles)

	var impacted, reasons []string
	for _, docPath := range candidates {
		isImpacted, why, err := r.triage(ctx, index, budget, fence, docPath, patch)
		if err != nil {
			return review.Result{}, fmt.Errorf("triage %s: %w", docPath, err)
		}
		if isImpacted {
			impacted = append(impacted, docPath)
		} else {
			reasons = append(reasons, docPath+": "+why)
		}
	}

	if len(impacted) == 0 {
		return r.noImpact(oneLine("no candidate doc is affected: "+strings.Join(reasons, "; "), maxReasonLen)), nil
	}

	proposals, err := r.draft(ctx, root, index, budget, fence, req, impacted, patch)
	if err != nil {
		return review.Result{}, err
	}
	if len(proposals) == 0 {
		return r.noImpact("model proposed no doc changes"), nil
	}

	var kept []review.Proposal
	var rejected []string
	for _, p := range proposals {
		supported, why, err := r.verify(ctx, budget, fence, p, patch)
		if err != nil {
			return review.Result{}, fmt.Errorf("verify proposal %s: %w", p.DocPath, err)
		}
		if supported {
			kept = append(kept, p)
		} else {
			rejected = append(rejected, p.DocPath+": "+why)
		}
	}
	if len(kept) == 0 {
		return r.noImpact(oneLine("verification rejected every proposal: "+strings.Join(rejected, "; "), maxReasonLen)), nil
	}

	return review.Result{Runner: runnerName, Model: r.model, Verdict: review.Proposals(kept)}, nil
}

// logProblems records docs that failed to parse: they silently drop out of
// covers matching, so someone must be able to see why a doc was not reviewed.
func (r *Runner) logProblems(ctx context.Context, side, sha string, problems []docs.Problem) {
	for _, p := range problems {
		r.log.WarnContext(ctx, "doc failed to parse and is skipped", "commit", side, "sha", sha, "path", p.Path, "err", p.Err)
	}
}

// headDoc reads docPath from the head clone for a candidate that docs.Parse
// left out, such as one with broken frontmatter, so a PR can't opt a doc out of
// triage by breaking it. It never reads through a symlink.
func headDoc(root *os.Root, docPath string) (docs.Doc, error) {
	info, err := root.Lstat(docPath)
	if err != nil {
		return docs.Doc{}, fmt.Errorf("candidate doc %s is missing at head: %w", docPath, err)
	}
	if !info.Mode().IsRegular() {
		return docs.Doc{}, fmt.Errorf("candidate doc %s at head is not a regular file (mode %s)", docPath, info.Mode())
	}

	f, err := root.Open(docPath)
	if err != nil {
		return docs.Doc{}, fmt.Errorf("open %s at head: %w", docPath, err)
	}
	defer func() { _ = f.Close() }() // read-only handle

	src, err := io.ReadAll(io.LimitReader(f, docs.MaxDocBytes+1))
	if err != nil {
		return docs.Doc{}, fmt.Errorf("read %s at head: %w", docPath, err)
	}
	if len(src) > docs.MaxDocBytes {
		return docs.Doc{}, fmt.Errorf("candidate doc %s at head exceeds %d bytes", docPath, docs.MaxDocBytes)
	}
	return docs.ParseBody(docPath, src), nil
}

const maxReasonLen = 300

// oneLine collapses s onto a single line and truncates it to max bytes.
func oneLine(s string, max int) string {
	return review.Truncate(strings.Join(strings.Fields(s), " "), max)
}

// docIndex maps a repo-relative doc path to its parsed doc at the head commit;
// candidates are chosen from the base commit's covers, but docs are read here.
type docIndex map[string]docs.Doc

// The verdict flags are pointers so a reply that omits them is an error, not
// a silent "no".
type triageVerdict struct {
	Impacted *bool  `json:"impacted"`
	Reason   string `json:"reason"`
}

type verifyVerdict struct {
	Supported *bool  `json:"supported"`
	Reason    string `json:"reason"`
}

// decodeVerdict decodes the first JSON object in reply into v, tolerating
// code fences and surrounding prose.
func decodeVerdict(reply string, v any) error {
	start := strings.IndexByte(reply, '{')
	if start < 0 {
		return fmt.Errorf("decode verdict from reply %q: no JSON object", oneLine(reply, 200))
	}
	if err := json.NewDecoder(strings.NewReader(reply[start:])).Decode(v); err != nil {
		return fmt.Errorf("decode verdict from reply %q: %w", oneLine(reply, 200), err)
	}
	return nil
}

// ask sends one tool-less prompt to model, charges its usage to budget, and
// decodes the JSON verdict in the reply into v. It returns the reply text for
// error messages.
func (r *Runner) ask(ctx context.Context, budget *agent.Budget, model, system, prompt string, v any) (string, error) {
	resp, err := r.m.Complete(ctx, llm.Request{
		Model:    model,
		System:   system,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: prompt}},
	})
	if err != nil {
		return "", fmt.Errorf("complete: %w: %w", errProvider, err)
	}
	if err := budget.Charge(resp.Usage); err != nil {
		return "", fmt.Errorf("charge token budget: %w", err)
	}
	if err := decodeVerdict(resp.Text, v); err != nil {
		return resp.Text, fmt.Errorf("%w: %w", errProvider, err)
	}
	return resp.Text, nil
}

// triage runs one small-model call for docPath, returning whether the PR's
// diff makes it stale and why.
func (r *Runner) triage(ctx context.Context, index docIndex, budget *agent.Budget, f fence, docPath, patch string) (impacted bool, reason string, err error) {
	var v triageVerdict
	reply, err := r.ask(ctx, budget, r.triageModel, triageSystemPrompt, triageUserPrompt(f, index[docPath], patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Impacted == nil {
		return false, "", fmt.Errorf("%w: triage verdict has no \"impacted\" field in reply %q", errProvider, oneLine(reply, 200))
	}
	return *v.Impacted, v.Reason, nil
}

// verify asks the triage model whether p is supported by the patch, given the
// doc section p replaces.
func (r *Runner) verify(ctx context.Context, budget *agent.Budget, f fence, p review.Proposal, patch string) (supported bool, reason string, err error) {
	section := p.Original
	if section == "" {
		section = "(new doc)"
	}

	var v verifyVerdict
	reply, err := r.ask(ctx, budget, r.triageModel, verifySystemPrompt, verifyUserPrompt(f, p, section, patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Supported == nil {
		return false, "", fmt.Errorf("%w: verify verdict has no \"supported\" field in reply %q", errProvider, oneLine(reply, 200))
	}
	return *v.Supported, v.Reason, nil
}

// checkSection reports why p, whose Section is normalized, cannot replace a
// section of its doc: the doc is not in the index, or Section does not name
// exactly one heading of it.
func checkSection(index docIndex, p review.Proposal) error {
	doc, ok := index[p.DocPath]
	if !ok {
		return fmt.Errorf("section %q: %s is not an existing doc; leave section empty to create a new doc", p.Section, p.DocPath)
	}
	if err := basedocs.FillOriginal(&p, doc); err != nil {
		return fmt.Errorf("check section of %s: %w", p.DocPath, err)
	}
	return nil
}

// draft runs the agent loop that drafts proposals for the impacted docs.
func (r *Runner) draft(ctx context.Context, root *os.Root, index docIndex, budget *agent.Budget, f fence, req review.Request, impacted []string, patch string) ([]review.Proposal, error) {
	finish, err := submitProposalsTool()
	if err != nil {
		return nil, err
	}

	impactedDocs := make([]docs.Doc, len(impacted))
	for i, p := range impacted {
		impactedDocs[i] = index[p]
	}

	task := agent.Task{
		Model:  r.model,
		System: draftSystemPrompt,
		Prompt: draftUserPrompt(f, impactedDocs, req.ChangedFiles, patch),
		Root:   root,
		Finish: finish,
		Accept: func(args json.RawMessage) error {
			var parsed review.ProposalsArgs
			if err := json.Unmarshal(args, &parsed); err != nil {
				return fmt.Errorf("decode submit_proposals arguments: %w", err)
			}
			for _, p := range parsed.Proposals {
				p.Section = review.NormalizeSection(p.Section)
				if err := p.Validate(req.ChangedFiles); err != nil {
					return fmt.Errorf("proposal %s: %w", p.DocPath, err)
				}
				if p.Section == "" {
					continue
				}
				if err := checkSection(index, p); err != nil {
					return fmt.Errorf("proposal %s: %w", p.DocPath, err)
				}
			}
			return nil
		},
		MaxSteps: r.analysisLimits.steps,
	}

	raw, stats, err := agent.Run(ctx, r.m, task, budget)
	r.logStats(ctx, "draft", stats)
	if err != nil {
		return nil, fmt.Errorf("draft proposals: %w", err)
	}

	var parsed review.ProposalsArgs
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode accepted submit_proposals arguments: %w", err)
	}

	for i := range parsed.Proposals {
		p := &parsed.Proposals[i]
		p.Section = review.NormalizeSection(p.Section)
		if err := basedocs.FillOriginal(p, index[p.DocPath]); err != nil {
			return nil, fmt.Errorf("accepted proposal %s: %w", p.DocPath, err)
		}
	}
	return parsed.Proposals, nil
}

func (r *Runner) logStats(ctx context.Context, what string, stats agent.Stats) {
	r.log.InfoContext(ctx, "agent run finished", "run", what, "steps", stats.Steps, "input_tokens", stats.InputTokens, "output_tokens", stats.OutputTokens)
}

func submitProposalsTool() (llm.Tool, error) {
	schema, err := review.ProposalsArgsSchema()
	if err != nil {
		return llm.Tool{}, fmt.Errorf("build submit_proposals schema: %w", err)
	}

	return llm.Tool{
		Name:        "submit_proposals",
		Description: "Submit the final list of doc proposals for this PR. An empty list means no doc needs to change.",
		Schema:      schema,
	}, nil
}
