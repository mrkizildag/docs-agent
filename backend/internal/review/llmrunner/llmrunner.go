// Package llmrunner implements review.Runner for the server: a small-model
// triage call per candidate doc, then an agent loop that drafts proposals
// over a depth-1 clone of the pull request's head commit.
package llmrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/agent"
	"github.com/mrkizildag/docs-agent/backend/internal/llm"
	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

const runnerName = "llmrunner"

// Caps on one analysis run: thinnest-path placeholders, narrowed once the
// spike (#11) reports real step and token counts on a free-tier model.
const (
	stepCap         = 25
	tokenBudget     = 200_000
	analysisTimeout = 150 * time.Second
)

// Runner implements review.Runner by triaging candidate docs with a small
// model and drafting proposals with an agent loop.
type Runner struct {
	m           llm.Model
	token       func(ctx context.Context, installationID int64) (string, error)
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
func New(m llm.Model, token func(ctx context.Context, installationID int64) (string, error), triageModel, model string) *Runner {
	return &Runner{m: m, token: token, triageModel: triageModel, model: model, timeout: analysisTimeout, budget: tokenBudget}
}

// Start implements review.Runner. Errors from a hit limit satisfy
// errors.Is with agent.ErrStepLimit, agent.ErrTokenBudget or agent.ErrDeadline.
func (r *Runner) Start(ctx context.Context, req review.Request) (review.Started, error) {
	if len(req.CandidateDocs) == 0 {
		return r.noImpact("no candidate docs"), nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	res, err := r.analyze(ctx, req)
	if err != nil {
		err = fmt.Errorf("start analysis %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err)
		if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, agent.ErrDeadline) {
			err = fmt.Errorf("%w: %w", agent.ErrDeadline, err)
		}
		return nil, err
	}
	return res, nil
}

func (r *Runner) noImpact(reason string) review.Result {
	return review.Result{Runner: runnerName, Model: r.model, Verdict: review.NoImpact{Reason: reason}}
}

func (r *Runner) analyze(ctx context.Context, req review.Request) (review.Result, error) {
	token, err := r.token(ctx, req.InstallationID)
	if err != nil {
		return review.Result{}, fmt.Errorf("get installation token: %w", err)
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
		return review.Result{}, err
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return review.Result{}, fmt.Errorf("open clone root: %w", err)
	}
	defer func() { _ = root.Close() }()

	budget := agent.NewBudget(r.budget)
	patch := combinedPatch(req.ChangedFiles)

	var impacted, reasons []string
	for _, docPath := range req.CandidateDocs {
		v, err := r.triage(ctx, root, budget, docPath, patch)
		if err != nil {
			return review.Result{}, fmt.Errorf("triage %s: %w", docPath, err)
		}
		if v.Impacted {
			impacted = append(impacted, docPath)
		} else {
			reasons = append(reasons, docPath+": "+v.Reason)
		}
	}

	if len(impacted) == 0 {
		return r.noImpact(oneLine("no candidate doc is affected: "+strings.Join(reasons, "; "), maxReasonLen)), nil
	}

	proposals, err := r.draft(ctx, root, budget, req, impacted, patch)
	if err != nil {
		return review.Result{}, err
	}
	if len(proposals) == 0 {
		return r.noImpact("model proposed no doc changes"), nil
	}

	var kept []review.Proposal
	var rejected []string
	for _, p := range proposals {
		v, err := r.verify(ctx, root, budget, p, patch)
		if err != nil {
			return review.Result{}, fmt.Errorf("verify proposal %s: %w", p.DocPath, err)
		}
		if v.Supported {
			kept = append(kept, p)
		} else {
			rejected = append(rejected, p.DocPath+": "+v.Reason)
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

type triageVerdict struct {
	Impacted bool   `json:"impacted"`
	Reason   string `json:"reason"`
}

type verifyVerdict struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

// decodeVerdict decodes the first JSON object in reply into v, tolerating
// code fences and surrounding prose.
func decodeVerdict(reply string, v any) error {
	start := strings.IndexByte(reply, '{')
	if start < 0 {
		return fmt.Errorf("decode verdict from reply %q: no JSON object", oneLine(reply, 200))
	}
	if err := json.NewDecoder(bytes.NewReader([]byte(reply[start:]))).Decode(v); err != nil {
		return fmt.Errorf("decode verdict from reply %q: %w", oneLine(reply, 200), err)
	}
	return nil
}

// ask sends one tool-less prompt to model, charges its usage to budget, and
// decodes the JSON verdict in the reply into v.
func (r *Runner) ask(ctx context.Context, budget *agent.Budget, model, system, prompt string, v any) error {
	resp, err := r.m.Complete(ctx, llm.Request{
		Model:    model,
		System:   system,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: prompt}},
	})
	if err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	if err := budget.Charge(resp.Usage); err != nil {
		return fmt.Errorf("charge token budget: %w", err)
	}
	return decodeVerdict(resp.Text, v)
}

// triage runs one small-model call for docPath, returning whether the PR's
// diff makes it stale.
func (r *Runner) triage(ctx context.Context, root *os.Root, budget *agent.Budget, docPath, patch string) (triageVerdict, error) {
	content, err := readDoc(root, docPath)
	if err != nil {
		return triageVerdict{}, fmt.Errorf("read doc %s: %w", docPath, err)
	}

	var v triageVerdict
	if err := r.ask(ctx, budget, r.triageModel, triageSystemPrompt, triageUserPrompt(docPath, content, patch), &v); err != nil {
		return triageVerdict{}, err
	}
	return v, nil
}

// verify asks the triage model whether p is supported by the patch, given the
// doc section p replaces.
func (r *Runner) verify(ctx context.Context, root *os.Root, budget *agent.Budget, p review.Proposal, patch string) (verifyVerdict, error) {
	section := "(new doc)"
	if p.Section != "" {
		content, err := readDoc(root, p.DocPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			section = "(doc does not exist)"
		case err != nil:
			return verifyVerdict{}, fmt.Errorf("read doc %s: %w", p.DocPath, err)
		default:
			section = docSection(content, p.Section)
		}
	}

	var v verifyVerdict
	if err := r.ask(ctx, budget, r.triageModel, verifySystemPrompt, verifyUserPrompt(p, section, patch), &v); err != nil {
		return verifyVerdict{}, err
	}
	return v, nil
}

// docSection returns the markdown section under the heading titled heading,
// up to the next heading of the same or a higher level, or the whole doc if
// no heading matches.
func docSection(content, heading string) string {
	lines := strings.Split(content, "\n")
	level := 0
	start := -1
	for i, line := range lines {
		l, title := headingOf(line)
		if start < 0 {
			if l > 0 && title == strings.TrimSpace(strings.TrimLeft(heading, "#")) {
				start, level = i, l
			}
			continue
		}
		if l > 0 && l <= level {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		return content
	}
	return strings.Join(lines[start:], "\n")
}

func headingOf(line string) (int, string) {
	trimmed := strings.TrimLeft(line, "#")
	level := len(line) - len(trimmed)
	if level == 0 || level > 6 || !strings.HasPrefix(trimmed, " ") {
		return 0, ""
	}
	return level, strings.TrimSpace(trimmed)
}

// submitProposalsArgs is the argument shape of the submit_proposals finishing
// tool: an object wrapping the array, since function-calling parameters must
// be a JSON Schema object.
type submitProposalsArgs struct {
	Proposals []review.Proposal `json:"proposals"`
}

// draft runs the agent loop that drafts proposals for the impacted docs.
func (r *Runner) draft(ctx context.Context, root *os.Root, budget *agent.Budget, req review.Request, impacted []string, patch string) ([]review.Proposal, error) {
	finish, err := submitProposalsTool()
	if err != nil {
		return nil, err
	}

	docsContent, err := readDocs(root, impacted)
	if err != nil {
		return nil, fmt.Errorf("read impacted docs: %w", err)
	}

	task := agent.Task{
		Model:  r.model,
		System: draftSystemPrompt,
		Prompt: draftUserPrompt(impacted, docsContent, patch),
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
			}
			return nil
		},
		MaxSteps: stepCap,
	}

	raw, _, err := agent.Run(ctx, r.m, task, budget)
	if err != nil {
		return nil, fmt.Errorf("draft proposals: %w", err)
	}

	var parsed submitProposalsArgs
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode accepted submit_proposals arguments: %w", err)
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

func readDoc(root *os.Root, docPath string) (string, error) {
	f, err := root.Open(docPath)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", docPath, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", docPath, err)
	}
	return string(data), nil
}

func readDocs(root *os.Root, paths []string) (string, error) {
	var b strings.Builder
	for _, p := range paths {
		content, err := readDoc(root, p)
		if err != nil {
			return "", fmt.Errorf("read doc %s: %w", p, err)
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", p, content)
	}
	return b.String(), nil
}
