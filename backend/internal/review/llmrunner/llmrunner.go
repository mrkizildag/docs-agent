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
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/finalize"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
)

const runnerName = "llmrunner"

// Caps on one analysis run, sized from measured runs on the Gemini free tier
// (1-3 steps, at most 46k tokens).
const (
	stepCap         = 12
	tokenBudget     = 120_000
	analysisTimeout = 150 * time.Second
)

// Runner implements review.Runner by triaging candidate docs with a small
// model and drafting proposals with an agent loop.
type Runner struct {
	m           llm.Model
	token       func(ctx context.Context, installationID int64, repo string) (string, error)
	triageModel string
	model       string
	log         *slog.Logger

	// Test overrides; see export_test.go.
	remote  string
	timeout time.Duration
	budget  int
}

var (
	_ review.Runner     = (*Runner)(nil)
	_ review.Scaffolder = (*Runner)(nil)
)

// New returns a Runner that triages with triageModel, drafts with model, both
// served by m, and authenticates clones with a token from token.
func New(m llm.Model, token func(ctx context.Context, installationID int64, repo string) (string, error), triageModel, model string, log *slog.Logger) *Runner {
	return &Runner{m: m, token: token, triageModel: triageModel, model: model, log: log, timeout: analysisTimeout, budget: tokenBudget}
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
		return noImpact("no changed files", "", nil), nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
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
	case errors.Is(err, errProvider):
		return review.CauseProvider
	default:
		return review.CauseInternal
	}
}

// noImpact is the Result for a PR that needs no doc change, decided by model;
// pass "" when no model was called.
func noImpact(reason, model string, budget *agent.Budget) review.Result {
	return review.Result{Model: model, Verdict: review.NoImpact{Reason: reason}, Usage: usageOf(budget)}
}

// usageOf reports what budget was charged; nil when no model call was made.
func usageOf(budget *agent.Budget) *review.Usage {
	if budget == nil {
		return nil
	}
	u := budget.Usage()
	if u == (llm.Usage{}) {
		return nil
	}
	return &review.Usage{Tokens: &review.Tokens{
		Input:      int64(u.InputTokens),
		Output:     int64(u.OutputTokens),
		CacheRead:  int64(u.CacheReadTokens),
		CacheWrite: int64(u.CacheWriteTokens),
	}}
}

func (r *Runner) analyze(ctx context.Context, req review.Request) (review.Result, error) {
	c, cleanup, err := r.openClone(ctx, req.InstallationID, req.Owner, req.Repo, req.HeadSHA, req.BaseSHA)
	if err != nil {
		return review.Result{}, err
	}
	defer cleanup()
	root := c.root
	log := r.log.With("repo", req.Owner+"/"+req.Repo, "pr", req.Number, "head_sha", req.HeadSHA)

	baseFS, err := c.docsAt(ctx, req.BaseSHA)
	if err != nil {
		return review.Result{}, fmt.Errorf("%w: %w", errClone, err)
	}

	selection, err := basedocs.Select(baseFS, req.ChangedFiles)
	if err != nil {
		return review.Result{}, fmt.Errorf("base docs of %s: %w", req.BaseSHA, err)
	}
	if len(selection.Restores) > 0 {
		return review.Result{Verdict: review.Proposals(selection.Restores)}, nil
	}
	in := input.New(req, selection)
	if len(in.Candidates) == 0 && len(in.Uncovered) == 0 {
		return noImpact(basedocs.NothingToReview, "", nil), nil
	}
	if len(in.Candidates) > basedocs.MaxCandidates {
		return review.Result{}, fmt.Errorf("%w: %d candidate docs exceed the cap of %d", errTooManyCandidates, len(in.Candidates), basedocs.MaxCandidates)
	}

	headTree, err := docs.Parse(root.FS())
	if err != nil {
		return review.Result{}, fmt.Errorf("parse docs of %s: %w", req.HeadSHA, err)
	}
	index := make(docIndex, len(headTree.Docs))
	for _, d := range headTree.Docs {
		index[d.Path] = d
	}
	for _, docPath := range in.Candidates {
		if _, ok := index[docPath]; ok {
			continue
		}
		d, err := headDoc(ctx, root, docPath)
		if err != nil {
			return review.Result{}, err
		}
		index[docPath] = d
	}

	fence, err := newFence()
	if err != nil {
		return review.Result{}, err
	}
	budget := agent.NewBudget(r.budget)
	patch := combinedPatch(req.ChangedFiles)

	var impacted, reasons []string
	for _, docPath := range in.Candidates {
		isImpacted, why, err := r.triage(ctx, log, index, budget, fence, docPath, patch)
		if err != nil {
			return review.Result{}, fmt.Errorf("triage %s: %w", docPath, err)
		}
		if isImpacted {
			impacted = append(impacted, docPath)
		} else {
			reasons = append(reasons, docPath+": "+why)
		}
	}

	allowNewDoc := false
	if len(in.Uncovered) > 0 {
		readme, err := headReadme(root)
		if err != nil {
			return review.Result{}, fmt.Errorf("read docs/README.md of %s: %w", req.HeadSHA, err)
		}
		needed, why, err := r.decideNewDoc(ctx, log, budget, fence, string(readme), in.Uncovered, patch)
		if err != nil {
			return review.Result{}, fmt.Errorf("decide new doc: %w", err)
		}
		if needed {
			allowNewDoc = true
		} else {
			reasons = append(reasons, "no doc covers "+strings.Join(in.Uncovered, ", ")+"; no new doc needed: "+why)
		}
	}

	if len(impacted) == 0 && !allowNewDoc {
		prefix := ""
		if len(in.Candidates) > 0 {
			prefix = "no candidate doc is affected: "
		}
		return noImpact(finalize.NoImpactReason(prefix+strings.Join(reasons, "; ")), r.triageModel, budget), nil
	}

	prompt := draftPrompt{fence: fence, impacted: make([]docs.Doc, len(impacted)), files: in.Files, patch: patch}
	for i, p := range impacted {
		prompt.impacted[i] = index[p]
	}
	if allowNewDoc {
		prompt.newDocFiles = in.Uncovered
	}
	rules := finalize.Rules{Changed: req.ChangedFiles, Selection: &selection, Repo: req.Owner + "/" + req.Repo, AllowNewDoc: allowNewDoc}
	proposals, err := r.draft(ctx, log, budget, cloneHead{root: root, gitlink: c.isGitlink}, rules, prompt)
	if err != nil {
		return review.Result{}, err
	}
	if len(proposals) == 0 {
		return noImpact("model proposed no doc changes", r.model, budget), nil
	}

	var kept []review.Proposal
	var rejected []string
	for _, p := range proposals {
		supported, why, err := r.verify(ctx, log, budget, fence, p, patch)
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
		return noImpact(finalize.NoImpactReason("verification rejected every proposal: "+strings.Join(rejected, "; ")), r.triageModel, budget), nil
	}

	return review.Result{Model: r.model, Verdict: review.Proposals(kept), Usage: usageOf(budget)}, nil
}

// headDoc reads docPath from the head clone for a candidate that docs.Parse
// left out, such as one with broken frontmatter, so a PR can't opt a doc out of
// triage by breaking it.
func headDoc(ctx context.Context, root *os.Root, docPath string) (docs.Doc, error) {
	src, ok, err := finalize.ReadDoc(ctx, cloneHead{root: root}, docPath)
	if err != nil {
		return docs.Doc{}, fmt.Errorf("candidate doc %s: %w", docPath, err)
	}
	if !ok {
		return docs.Doc{}, fmt.Errorf("candidate doc %s at head is missing, not a regular file, or over %d bytes", docPath, docs.MaxDocBytes)
	}
	return docs.ParseBody(docPath, src), nil
}

// cloneHead is the finalize.Head over the head clone. It never follows a
// symlink at the path it is asked about. gitlink, when set, reports whether a
// path is a submodule entry.
type cloneHead struct {
	root    *os.Root
	gitlink func(ctx context.Context, path string) (bool, error)
}

var _ finalize.Head = cloneHead{}

func (h cloneHead) Stat(ctx context.Context, path string) (finalize.Kind, error) {
	info, err := h.root.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return finalize.Missing, nil
	case err != nil:
		return finalize.Missing, fmt.Errorf("lstat %s at head: %w", path, err)
	case !info.IsDir():
		return finalize.Other, nil
	}
	// A clone checks a submodule out as an empty directory.
	if h.gitlink == nil || !h.emptyDir(path) {
		return finalize.Dir, nil
	}
	isLink, err := h.gitlink(ctx, path)
	if err != nil {
		return finalize.Missing, err
	}
	if isLink {
		return finalize.Other, nil
	}
	return finalize.Dir, nil
}

// emptyDir reports whether dir has no entries.
func (h cloneHead) emptyDir(dir string) bool {
	f, err := h.root.Open(dir)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }() // read-only handle
	entries, err := f.ReadDir(1)
	return errors.Is(err, io.EOF) && len(entries) == 0
}

func (h cloneHead) ReadFile(_ context.Context, path string) ([]byte, bool, error) {
	info, err := h.root.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("lstat %s at head: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > docs.MaxDocBytes {
		return nil, false, nil
	}

	f, err := h.root.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open %s at head: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only handle

	src, err := io.ReadAll(io.LimitReader(f, docs.MaxDocBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s at head: %w", path, err)
	}
	if len(src) > docs.MaxDocBytes {
		return nil, false, nil
	}
	return src, true, nil
}

// headReadme reads docs/README.md from the head clone, empty when it is
// unreachable or not a regular file. It never reads through a symlink.
func headReadme(root *os.Root) (string, error) {
	const readmePath = "docs/README.md"
	info, err := root.Lstat(readmePath)
	if err != nil {
		return "", nil //nolint:nilerr // the README only feeds the prompt; any unreachable path counts as absent
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}

	f, err := root.Open(readmePath)
	if err != nil {
		return "", fmt.Errorf("open %s at head: %w", readmePath, err)
	}
	defer func() { _ = f.Close() }() // read-only handle

	src, err := io.ReadAll(io.LimitReader(f, maxDocBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s at head: %w", readmePath, err)
	}
	return string(src), nil
}

// oneLine collapses s onto a single line and truncates it to max bytes. It
// caps log and error text; no-impact reasons use finalize.NoImpactReason.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "..."
	}
	return s
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

type newDocVerdict struct {
	Needed *bool  `json:"needed"`
	Reason string `json:"reason"`
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
func (r *Runner) ask(ctx context.Context, log *slog.Logger, kind string, budget *agent.Budget, model, system, prompt string, v any) (string, error) {
	resp, err := r.m.Complete(ctx, llm.Request{
		Model:    model,
		System:   system,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: prompt}},
	})
	if err != nil {
		return "", fmt.Errorf("complete: %w: %w", errProvider, err)
	}
	log.Info("agent call", "kind", kind, "model", model,
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens)
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
func (r *Runner) triage(ctx context.Context, log *slog.Logger, index docIndex, budget *agent.Budget, f fence, docPath, patch string) (impacted bool, reason string, err error) {
	var v triageVerdict
	reply, err := r.ask(ctx, log, "triage", budget, r.triageModel, triageSystemPrompt, triageUserPrompt(f, index[docPath], patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Impacted == nil {
		return false, "", fmt.Errorf("%w: triage verdict has no \"impacted\" field in reply %q", errProvider, oneLine(reply, 200))
	}
	return *v.Impacted, v.Reason, nil
}

// decideNewDoc runs one small-model call on whether the PR's diff adds
// behavior that needs a new doc because no existing doc can hold it.
func (r *Runner) decideNewDoc(ctx context.Context, log *slog.Logger, budget *agent.Budget, f fence, readme string, uncovered []string, patch string) (needed bool, reason string, err error) {
	var v newDocVerdict
	reply, err := r.ask(ctx, log, "new_doc", budget, r.triageModel, newDocSystemPrompt, newDocUserPrompt(f, readme, uncovered, patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Needed == nil {
		return false, "", fmt.Errorf("%w: new-doc verdict has no \"needed\" field in reply %q", errProvider, oneLine(reply, 200))
	}
	return *v.Needed, v.Reason, nil
}

// verify asks the triage model whether p is supported by the patch, given the
// doc section p replaces.
func (r *Runner) verify(ctx context.Context, log *slog.Logger, budget *agent.Budget, f fence, p review.Proposal, patch string) (supported bool, reason string, err error) {
	section := p.Original
	if p.Section == "" {
		section = "(new doc)"
	}

	var v verifyVerdict
	reply, err := r.ask(ctx, log, "verify", budget, r.triageModel, verifySystemPrompt, verifyUserPrompt(f, p, section, patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Supported == nil {
		return false, "", fmt.Errorf("%w: verify verdict has no \"supported\" field in reply %q", errProvider, oneLine(reply, 200))
	}
	return *v.Supported, v.Reason, nil
}

// submitProposalsArgs is the argument shape of the submit_proposals finishing
// tool: an object wrapping the array, since function-calling parameters must
// be a JSON Schema object.
type submitProposalsArgs struct {
	Proposals []review.Proposal `json:"proposals"`
}

// draft runs the agent loop that drafts proposals and finalizes each
// submission against the clone's head under rules.
func (r *Runner) draft(ctx context.Context, log *slog.Logger, budget *agent.Budget, head cloneHead, rules finalize.Rules, prompt draftPrompt) ([]review.Proposal, error) {
	finish, err := submitProposalsTool()
	if err != nil {
		return nil, err
	}

	var finalized []review.Proposal
	var headErr error
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	task := agent.Task{
		Model:  r.model,
		System: draftSystemPrompt,
		Prompt: prompt.user(),
		Root:   head.root,
		Finish: finish,
		Accept: func(args json.RawMessage) error {
			var parsed submitProposalsArgs
			if err := json.Unmarshal(args, &parsed); err != nil {
				return fmt.Errorf("decode submit_proposals arguments: %w", err)
			}
			out, problems, err := finalize.Proposals(runCtx, head, rules, parsed.Proposals)
			if err != nil {
				// The model cannot fix a failed read of the clone: end the loop.
				headErr = fmt.Errorf("finalize proposals: %w", err)
				cancel()
				return errors.New("the server could not read the repository; submission not accepted")
			}
			if len(problems) > 0 {
				return problems
			}
			finalized = out
			return nil
		},
		MaxSteps: stepCap,
		Log:      log,
	}

	_, _, err = agent.Run(runCtx, r.m, task, budget)
	if headErr != nil {
		return nil, fmt.Errorf("draft proposals: %w", headErr)
	}
	if err != nil {
		return nil, fmt.Errorf("draft proposals: %w: %w", errProvider, err)
	}
	return finalized, nil
}

func submitProposalsTool() (llm.Tool, error) {
	proposalSchema, err := review.ProposalSchema()
	if err != nil {
		return llm.Tool{}, fmt.Errorf("build submit_proposals schema: %w", err)
	}

	schema, err := json.Marshal(struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}{
		Type: "object",
		Properties: map[string]any{
			"proposals": map[string]any{
				"type":  "array",
				"items": json.RawMessage(proposalSchema),
			},
		},
		Required: []string{"proposals"},
	})
	if err != nil {
		return llm.Tool{}, fmt.Errorf("marshal submit_proposals schema: %w", err)
	}

	return llm.Tool{
		Name:        "submit_proposals",
		Description: "Submit the final list of doc proposals for this PR. An empty list means no doc needs to change.",
		Schema:      schema,
	}, nil
}
