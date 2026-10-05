// Package llmrunner implements review.Runner for the server: a small-model
// triage call per candidate doc, then an agent loop that drafts proposals
// over a depth-1 clone of the pull request's head commit.
package llmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const runnerName = "llmrunner"

// Caps on one analysis run, sized from measured runs on the Gemini free tier
// (1-3 steps, at most 46k tokens).
const (
	stepCap         = 12
	tokenBudget     = 120_000
	analysisTimeout = 150 * time.Second

	// maxCandidateDocs bounds the triage calls one PR can trigger.
	maxCandidateDocs = 10
)

// Runner implements review.Runner by triaging candidate docs with a small
// model and drafting proposals with an agent loop.
type Runner struct {
	m           llm.Model
	token       func(ctx context.Context, installationID int64, repo string) (string, error)
	triageModel string
	model       string

	// Test overrides; see export_test.go.
	remote  string
	timeout time.Duration
	budget  int
}

var _ review.Runner = (*Runner)(nil)

// New returns a Runner that triages with triageModel, drafts with model, both
// served by m, and authenticates clones with a token from token.
func New(m llm.Model, token func(ctx context.Context, installationID int64, repo string) (string, error), triageModel, model string) *Runner {
	return &Runner{m: m, token: token, triageModel: triageModel, model: model, timeout: analysisTimeout, budget: tokenBudget}
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

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	res, err := r.analyze(ctx, req)
	if err != nil {
		err = fmt.Errorf("start analysis %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err)
		if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, agent.ErrDeadline) {
			err = fmt.Errorf("%w: %w", agent.ErrDeadline, err)
		}
		return nil, &review.FailedError{Cause: classify(err), Err: err}
	}
	return res, nil
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

func (r *Runner) noImpact(reason string) review.Result {
	return review.Result{Runner: runnerName, Model: r.model, Verdict: review.NoImpact{Reason: reason}}
}

func (r *Runner) analyze(ctx context.Context, req review.Request) (review.Result, error) {
	token, err := r.token(ctx, req.InstallationID, req.Repo)
	if err != nil {
		return review.Result{}, fmt.Errorf("get installation token: %w: %w", errClone, err)
	}

	remoteURL := r.remote
	if remoteURL == "" {
		remoteURL = fmt.Sprintf("https://github.com/%s/%s.git", req.Owner, req.Repo)
	}

	dir, err := cloneHead(ctx, remoteURL, req.HeadSHA, token)
	if dir != "" {
		defer func() { _ = os.RemoveAll(dir) }() // best-effort cleanup of a temp dir; the runner has no logger
	}
	if err != nil {
		return review.Result{}, fmt.Errorf("%w: %w", errClone, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return review.Result{}, fmt.Errorf("open clone root: %w", err)
	}
	defer func() { _ = root.Close() }()

	tree, err := docs.Parse(root.FS())
	if err != nil {
		return review.Result{}, fmt.Errorf("parse docs of %s: %w", req.HeadSHA, err)
	}
	changed := make([]string, 0, len(req.ChangedFiles))
	for _, f := range req.ChangedFiles {
		changed = append(changed, f.Path)
		if f.PreviousPath != "" {
			changed = append(changed, f.PreviousPath)
		}
	}
	candidates := tree.Match(changed)
	if len(candidates) == 0 {
		return r.noImpact("no doc covers the changed files"), nil
	}
	if len(candidates) > maxCandidateDocs {
		return review.Result{}, fmt.Errorf("%w: %d candidate docs exceed the cap of %d", errTooManyCandidates, len(candidates), maxCandidateDocs)
	}

	index := make(docIndex, len(tree.Docs))
	for _, d := range tree.Docs {
		index[d.Path] = d
	}
	fence, err := newFence()
	if err != nil {
		return review.Result{}, err
	}
	budget := agent.NewBudget(r.budget)
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
		supported, why, err := r.verify(ctx, index, budget, fence, p, patch)
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

const maxReasonLen = 300

// oneLine collapses s onto a single line and truncates it to max bytes.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "..."
	}
	return s
}

// docIndex maps a repo-relative doc path to its parsed doc at the head commit.
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
func (r *Runner) verify(ctx context.Context, index docIndex, budget *agent.Budget, f fence, p review.Proposal, patch string) (supported bool, reason string, err error) {
	section := "(new doc)"
	if p.Section != "" {
		doc, ok := index[p.DocPath]
		if ok {
			text, _, found := lookupSection(doc, p.Section)
			if !found {
				text = string(doc.Source)
			}
			section = text
		} else {
			section = "(doc does not exist)"
		}
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

// normalizeSection strips leading '#'s and surrounding spaces from a section
// heading as models write it.
func normalizeSection(section string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(section), "#"))
}

// lookupSection returns the markdown under the heading titled heading. When
// none matches it returns the quoted headings of the doc instead.
func lookupSection(doc docs.Doc, heading string) (text string, headings []string, found bool) {
	want := normalizeSection(heading)
	for _, s := range doc.Sections {
		if s.Level == 0 {
			continue
		}
		if s.Heading == want {
			return string(doc.Source[s.Start:s.End]), nil, true
		}
		headings = append(headings, fmt.Sprintf("%q", s.Heading))
	}
	return "", headings, false
}

// checkSection reports an error listing the doc's headings when section names
// none of them. A doc missing at the head commit has no headings to check.
func checkSection(index docIndex, docPath, section string) error {
	doc, ok := index[docPath]
	if !ok {
		return nil
	}
	_, headings, found := lookupSection(doc, section)
	if found {
		return nil
	}
	return fmt.Errorf("section %q: no such heading in %s; headings are: %s", section, docPath, strings.Join(headings, ", "))
}

// submitProposalsArgs is the argument shape of the submit_proposals finishing
// tool: an object wrapping the array, since function-calling parameters must
// be a JSON Schema object.
type submitProposalsArgs struct {
	Proposals []review.Proposal `json:"proposals"`
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
			var parsed submitProposalsArgs
			if err := json.Unmarshal(args, &parsed); err != nil {
				return fmt.Errorf("decode submit_proposals arguments: %w", err)
			}
			for _, p := range parsed.Proposals {
				if err := p.Validate(req.ChangedFiles); err != nil {
					return fmt.Errorf("proposal %s: %w", p.DocPath, err)
				}
				if p.Section == "" {
					continue
				}
				if err := checkSection(index, p.DocPath, normalizeSection(p.Section)); err != nil {
					return fmt.Errorf("proposal %s: %w", p.DocPath, err)
				}
			}
			return nil
		},
		MaxSteps: stepCap,
	}

	raw, _, err := agent.Run(ctx, r.m, task, budget)
	if err != nil {
		return nil, fmt.Errorf("draft proposals: %w: %w", errProvider, err)
	}

	var parsed submitProposalsArgs
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode accepted submit_proposals arguments: %w", err)
	}

	for i := range parsed.Proposals {
		p := &parsed.Proposals[i]
		p.Section = normalizeSection(p.Section)
		if p.Section == "" {
			continue
		}
		if text, start, end, ok := index[p.DocPath].SectionSpan(p.Section); ok {
			p.Original, p.Lines = text, review.LineRange{Start: start, End: end}
		}
	}
	return parsed.Proposals, nil
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
