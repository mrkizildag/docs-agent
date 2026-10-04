// Package gate is the domain: deciding what check run a pull request gets.
package gate

import (
	"context"
	"errors"
	"fmt"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

// errAsyncUnsupported marks runners that return review.Pending; no runner
// wired today does (#12 adds one).
var errAsyncUnsupported = errors.New("asynchronous runners are not supported yet")

// setupGuideURL is linked from the neutral check when no runner is available.
const setupGuideURL = "https://github.com/mrkizildag/docs-agent/blob/main/docs/guides/setup.md"

// PullRequest is the subset of a GitHub pull request the gate needs.
type PullRequest struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	BaseSHA        string
	HeadSHA        string
}

// Conclusion is a GitHub check run conclusion.
type Conclusion string

const (
	ConclusionSuccess        Conclusion = "success"
	ConclusionNeutral        Conclusion = "neutral"
	ConclusionActionRequired Conclusion = "action_required"
)

// CheckRun is a completed GitHub check run.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Conclusion Conclusion
	Title      string
	Summary    string
}

// GitHub creates check runs and inspects repository state on behalf of an
// installation.
type GitHub interface {
	CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run CheckRun) error
	// WorkflowExists reports whether the repo's default branch has the
	// docs-agent Actions workflow.
	WorkflowExists(ctx context.Context, installationID int64, owner, repo string) (bool, error)
	// ListChangedFiles returns the files in the pull request's diff with their head-side hunk ranges.
	ListChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error)
}

const checkName = "docs-agent"

// PRState is what the gate remembers about one pull request between events.
type PRState struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string // head commit the gate last reported a check run for; "" if never
}

// Store persists PRState.
type Store interface {
	// LoadPR returns the zero-HeadSHA state (identity fields filled from the args) for a PR never saved.
	LoadPR(ctx context.Context, owner, repo string, number int) (PRState, error)
	SavePR(ctx context.Context, state PRState) error
}

// OnPush is the state transition for a new head commit: pure, no I/O.
func OnPush(_ PRState, pr PullRequest) PRState {
	return PRState{
		InstallationID: pr.InstallationID,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
		Number:         pr.Number,
		HeadSHA:        pr.HeadSHA,
	}
}

// Runners are the analysis runners a repo may use. A nil Runner means that
// runner is unavailable.
type Runners struct {
	Actions review.Runner
	Server  review.Runner
}

// Service decides and reports the docs-agent check run for a pull request.
type Service struct {
	gh      GitHub
	store   Store
	runners Runners
}

// NewService returns a Service that reports check runs through gh, persists
// state through store, and selects among runners for analysis.
func NewService(gh GitHub, store Store, runners Runners) *Service {
	return &Service{gh: gh, store: store, runners: runners}
}

// HandlePullRequest selects an analysis runner for pr, runs it, and reports
// the result as the docs-agent check run.
func (s *Service) HandlePullRequest(ctx context.Context, pr PullRequest) error {
	state, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: load state: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	var hasWorkflow bool
	if s.runners.Actions != nil || s.runners.Server != nil {
		hasWorkflow, err = s.gh.WorkflowExists(ctx, pr.InstallationID, pr.Owner, pr.Repo)
		if err != nil {
			return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
		}
	}

	var run CheckRun
	switch selectRunner(hasWorkflow, s.runners) {
	case runnerNone:
		run = CheckRun{
			Name:       checkName,
			HeadSHA:    pr.HeadSHA,
			Conclusion: ConclusionNeutral,
			Title:      "No analysis runner configured",
			Summary:    "Set up an analysis runner: " + setupGuideURL,
		}
	case runnerActions:
		run, err = s.runCheck(ctx, s.runners.Actions, pr)
	case runnerServer:
		run, err = s.runCheck(ctx, s.runners.Server, pr)
	}
	if err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	if err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run); err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	if err := s.store.SavePR(ctx, OnPush(state, pr)); err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: save state: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	return nil
}

// runnerSelection names which runner HandlePullRequest uses.
type runnerSelection int

const (
	runnerNone runnerSelection = iota
	runnerActions
	runnerServer
)

// selectRunner picks the runner a repo uses: the Actions runner when the
// workflow is present, else the server runner, else none. A nil runner in
// the chosen slot counts as none; it never falls back to the other runner.
func selectRunner(hasWorkflow bool, runners Runners) runnerSelection {
	if hasWorkflow {
		if runners.Actions == nil {
			return runnerNone
		}
		return runnerActions
	}
	if runners.Server == nil {
		return runnerNone
	}
	return runnerServer
}

func (s *Service) runCheck(ctx context.Context, runner review.Runner, pr PullRequest) (CheckRun, error) {
	changed, err := s.gh.ListChangedFiles(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return CheckRun{}, fmt.Errorf("list changed files: %w", err)
	}

	req := review.Request{
		InstallationID: pr.InstallationID,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
		Number:         pr.Number,
		BaseSHA:        pr.BaseSHA,
		HeadSHA:        pr.HeadSHA,
		ChangedFiles:   changed,
	}

	started, err := runner.Start(ctx, req)
	if err != nil {
		return CheckRun{}, fmt.Errorf("start analysis: %w", err)
	}

	switch res := started.(type) {
	case review.Pending:
		return CheckRun{}, errAsyncUnsupported
	case review.Result:
		return checkRunForResult(pr.HeadSHA, res)
	default:
		return CheckRun{}, fmt.Errorf("unknown review.Started %T", started)
	}
}

func checkRunForResult(headSHA string, result review.Result) (CheckRun, error) {
	switch v := result.Verdict.(type) {
	case review.NoImpact:
		return CheckRun{
			Name:       checkName,
			HeadSHA:    headSHA,
			Conclusion: ConclusionSuccess,
			Title:      "No doc impact",
			Summary:    v.Reason,
		}, nil
	case review.Proposals:
		if len(v) == 0 {
			return CheckRun{}, errors.New("runner returned an empty proposal list; no impact must be NoImpact")
		}
		return CheckRun{
			Name:       checkName,
			HeadSHA:    headSHA,
			Conclusion: ConclusionActionRequired,
			Title:      "Docs need updating",
			Summary:    proposalsSummary(v),
		}, nil
	default:
		return CheckRun{}, fmt.Errorf("unknown review.Verdict %T", result.Verdict)
	}
}

func proposalsSummary(proposals review.Proposals) string {
	summary := ""
	for _, p := range proposals {
		if summary != "" {
			summary += "\n"
		}
		summary += fmt.Sprintf("- %s: %s", p.DocPath, p.Reason)
	}
	return summary
}
