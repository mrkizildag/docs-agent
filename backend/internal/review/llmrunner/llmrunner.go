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

	selection, err := selectAtBase(ctx, c, req)
	if err != nil {
		return review.Result{}, err
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
	candidates, err := candidateDocs(ctx, c.root, in.Candidates)
	if err != nil {
		return review.Result{}, fmt.Errorf("docs of %s: %w", req.HeadSHA, err)
	}

	fence, err := newFence()
	if err != nil {
		return review.Result{}, err
	}
	s := session{
		r:      r,
		log:    r.log.With("repo", req.Owner+"/"+req.Repo, "pr", req.Number, "head_sha", req.HeadSHA),
		budget: agent.NewBudget(r.budget),
		fence:  fence,
		patch:  combinedPatch(req.ChangedFiles),
	}

	impacted, reasons, err := s.triageAll(ctx, candidates)
	if err != nil {
		return review.Result{}, err
	}
	allowNewDoc := false
	if len(in.Uncovered) > 0 {
		var why string
		allowNewDoc, why, err = s.newDocAllowed(ctx, c.root, in.Uncovered)
		if err != nil {
			return review.Result{}, err
		}
		if !allowNewDoc {
			reasons = append(reasons, "no doc covers "+strings.Join(in.Uncovered, ", ")+"; no new doc needed: "+why)
		}
	}
	if len(impacted) == 0 && !allowNewDoc {
		prefix := ""
		if len(in.Candidates) > 0 {
			prefix = "no candidate doc is affected: "
		}
		return noImpact(finalize.NoImpactReason(prefix+strings.Join(reasons, "; ")), r.triageModel, s.budget), nil
	}

	prompt := draftPrompt{impacted: impacted, files: in.Files}
	if allowNewDoc {
		prompt.newDocFiles = in.Uncovered
	}
	rules := finalize.Rules{Changed: req.ChangedFiles, Selection: &selection, Repo: req.Owner + "/" + req.Repo, AllowNewDoc: allowNewDoc}
	proposals, err := s.draft(ctx, cloneHead{root: c.root, gitlink: c.isGitlink}, rules, prompt)
	if err != nil {
		return review.Result{}, err
	}
	if len(proposals) == 0 {
		return noImpact("model proposed no doc changes", r.model, s.budget), nil
	}

	kept, rejected, err := s.verifyAll(ctx, proposals)
	if err != nil {
		return review.Result{}, err
	}
	if len(kept) == 0 {
		return noImpact(finalize.NoImpactReason("verification rejected every proposal: "+strings.Join(rejected, "; ")), r.triageModel, s.budget), nil
	}
	return review.Result{Model: r.model, Verdict: review.Proposals(kept), Usage: usageOf(s.budget)}, nil
}

// selectAtBase picks the candidate docs and uncovered files from the docs at
// the PR's merge base.
func selectAtBase(ctx context.Context, c *clone, req review.Request) (basedocs.Selection, error) {
	baseFS, err := c.docsAt(ctx, req.BaseSHA)
	if err != nil {
		return basedocs.Selection{}, fmt.Errorf("%w: %w", errClone, err)
	}
	selection, err := basedocs.Select(baseFS, req.ChangedFiles)
	if err != nil {
		return basedocs.Selection{}, fmt.Errorf("base docs of %s: %w", req.BaseSHA, err)
	}
	return selection, nil
}

// candidateDocs reads each candidate doc at the head clone, in order.
func candidateDocs(ctx context.Context, root *os.Root, paths []string) ([]docs.Doc, error) {
	tree, err := docs.Parse(root.FS())
	if err != nil {
		return nil, fmt.Errorf("parse docs: %w", err)
	}
	parsed := make(map[string]docs.Doc, len(tree.Docs))
	for _, d := range tree.Docs {
		parsed[d.Path] = d
	}
	out := make([]docs.Doc, len(paths))
	for i, p := range paths {
		d, ok := parsed[p]
		if !ok {
			if d, err = headDoc(ctx, root, p); err != nil {
				return nil, err
			}
		}
		out[i] = d
	}
	return out, nil
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

// session is one analysis's shared model state: the logger, the token
// budget, the prompt fence, and the PR's combined diff every prompt carries.
type session struct {
	r      *Runner
	log    *slog.Logger
	budget *agent.Budget
	fence  fence
	patch  string
}

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

// ask sends one tool-less prompt to the triage model, charges its usage to
// the budget, and decodes the JSON verdict in the reply into v. It returns the
// reply text for error messages.
func (s session) ask(ctx context.Context, kind, system, prompt string, v any) (string, error) {
	resp, err := s.r.m.Complete(ctx, llm.Request{
		Model:    s.r.triageModel,
		System:   system,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: prompt}},
	})
	if err != nil {
		return "", fmt.Errorf("complete: %w: %w", errProvider, err)
	}
	s.log.Info("agent call", "kind", kind, "model", s.r.triageModel,
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens)
	if err := s.budget.Charge(resp.Usage); err != nil {
		return "", fmt.Errorf("charge token budget: %w", err)
	}
	if err := decodeVerdict(resp.Text, v); err != nil {
		return resp.Text, fmt.Errorf("%w: %w", errProvider, err)
	}
	return resp.Text, nil
}

// triageAll triages every candidate, returning the impacted docs and a
// "path: reason" line for each doc left out.
func (s session) triageAll(ctx context.Context, candidates []docs.Doc) (impacted []docs.Doc, reasons []string, err error) {
	for _, d := range candidates {
		isImpacted, why, err := s.triage(ctx, d)
		if err != nil {
			return nil, nil, fmt.Errorf("triage %s: %w", d.Path, err)
		}
		if isImpacted {
			impacted = append(impacted, d)
		} else {
			reasons = append(reasons, d.Path+": "+why)
		}
	}
	return impacted, reasons, nil
}

// triage runs one small-model call for doc, returning whether the PR's diff
// makes it stale and why.
func (s session) triage(ctx context.Context, doc docs.Doc) (impacted bool, reason string, err error) {
	var v triageVerdict
	reply, err := s.ask(ctx, "triage", triageSystemPrompt, triageUserPrompt(s.fence, doc, s.patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Impacted == nil {
		return false, "", fmt.Errorf("%w: triage verdict has no \"impacted\" field in reply %q", errProvider, oneLine(reply, 200))
	}
	return *v.Impacted, v.Reason, nil
}

// newDocAllowed decides, against the head's docs index, whether the uncovered
// files need a new doc, and why not when they don't.
func (s session) newDocAllowed(ctx context.Context, root *os.Root, uncovered []string) (needed bool, reason string, err error) {
	readme, err := headReadme(root)
	if err != nil {
		return false, "", fmt.Errorf("read docs/README.md: %w", err)
	}
	var v newDocVerdict
	reply, err := s.ask(ctx, "new_doc", newDocSystemPrompt, newDocUserPrompt(s.fence, readme, uncovered, s.patch), &v)
	if err != nil {
		return false, "", fmt.Errorf("decide new doc: %w", err)
	}
	if v.Needed == nil {
		return false, "", fmt.Errorf("decide new doc: %w: new-doc verdict has no \"needed\" field in reply %q", errProvider, oneLine(reply, 200))
	}
	return *v.Needed, v.Reason, nil
}

// verifyAll verifies every proposal, returning the supported ones and a
// "path: reason" line for each rejected one.
func (s session) verifyAll(ctx context.Context, proposals []review.Proposal) (kept []review.Proposal, rejected []string, err error) {
	for _, p := range proposals {
		supported, why, err := s.verify(ctx, p)
		if err != nil {
			return nil, nil, fmt.Errorf("verify proposal %s: %w", p.DocPath, err)
		}
		if supported {
			kept = append(kept, p)
		} else {
			rejected = append(rejected, p.DocPath+": "+why)
		}
	}
	return kept, rejected, nil
}

// verify asks the triage model whether p is supported by the patch, given the
// doc section p replaces.
func (s session) verify(ctx context.Context, p review.Proposal) (supported bool, reason string, err error) {
	section := p.Original
	if p.Section == "" {
		section = "(new doc)"
	}

	var v verifyVerdict
	reply, err := s.ask(ctx, "verify", verifySystemPrompt, verifyUserPrompt(s.fence, p, section, s.patch), &v)
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
func (s session) draft(ctx context.Context, head cloneHead, rules finalize.Rules, prompt draftPrompt) ([]review.Proposal, error) {
	finish, err := submitProposalsTool()
	if err != nil {
		return nil, err
	}

	var finalized []review.Proposal
	var headErr error
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	task := agent.Task{
		Model:  s.r.model,
		System: draftSystemPrompt(),
		Prompt: prompt.user(s.fence, s.patch),
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
		Log:      s.log,
	}

	_, _, err = agent.Run(runCtx, s.r.m, task, s.budget)
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
