// Package gate is the domain: deciding what check run a pull request gets.
package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

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

// Status is a GitHub check run status.
type Status string

const (
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// CheckRun is a GitHub check run; Conclusion is empty unless Status is completed.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Status     Status
	Conclusion Conclusion
	Title      string
	Summary    string
}

// GitHub creates check runs and inspects repository state on behalf of an
// installation.
type GitHub interface {
	// CreateCheckRun returns the ID of the check run it created.
	CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run CheckRun) (int64, error)
	UpdateCheckRun(ctx context.Context, installationID int64, owner, repo string, id int64, run CheckRun) error
	// WorkflowExists reports whether the repo's default branch has the
	// docs-agent Actions workflow.
	WorkflowExists(ctx context.Context, installationID int64, owner, repo string) (bool, error)
}

const checkName = "docs-agent"

// PRState is what the gate remembers about one pull request between events.
type PRState struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string // head commit the gate last reported a check run for; "" if never
	CheckRunID     int64  // check run reported for HeadSHA; 0 if none
	Run            *AwaitingRun
}

// AwaitingRun is the external analysis run whose result will conclude the
// check run; PRState.Run is nil when none is awaited.
type AwaitingRun struct {
	RunID    int64
	Nonce    string
	Deadline time.Time
}

// RunCompleted reports that an external analysis run finished.
type RunCompleted struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	RunID          int64
	Conclusion     string
}

// PRRef identifies a pull request.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// OverdueRun is an awaited run whose deadline has passed, as found by a sweep.
type OverdueRun struct {
	PRRef
	Nonce string
}

// Store persists PRState.
type Store interface {
	// LoadPR returns the zero-HeadSHA state (identity fields filled from the args) for a PR never saved.
	LoadPR(ctx context.Context, owner, repo string, number int) (PRState, error)
	SavePR(ctx context.Context, state PRState) error
	// PRForRun returns the pull request an external run was dispatched for.
	PRForRun(ctx context.Context, owner, repo string, runID int64) (number int, ok bool, err error)
}

// OnPush is the state transition for a new head commit: pure, no I/O. It
// drops any awaited run, so that run's result is ignored.
func OnPush(_ PRState, pr PullRequest) PRState {
	return PRState{
		InstallationID: pr.InstallationID,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
		Number:         pr.Number,
		HeadSHA:        pr.HeadSHA,
	}
}

// Superseded returns the neutral check run that closes the check run of an
// awaited run when a new analysis of pr replaces it, on any head; ok is false
// when there is nothing to close. Pure, no I/O.
func Superseded(state PRState, pr PullRequest) (run CheckRun, ok bool) {
	if state.Run == nil || state.CheckRunID == 0 {
		return CheckRun{}, false
	}
	short := pr.HeadSHA
	if len(short) > 7 {
		short = short[:7]
	}
	run = CheckRun{Name: checkName, HeadSHA: state.HeadSHA, Status: StatusCompleted}
	return neutral(run, "Superseded", "Superseded by "+short), true
}

// Overdue reports whether state awaits a run whose deadline has passed at now.
func Overdue(state PRState, now time.Time) bool {
	return state.Run != nil && now.After(state.Run.Deadline)
}

// OnStarted is the state transition for an analysis that runs elsewhere: pure, no I/O.
func OnStarted(state PRState, pending review.Pending, checkRunID int64) PRState {
	state.CheckRunID = checkRunID
	state.Run = &AwaitingRun{RunID: pending.RunID, Nonce: pending.Nonce, Deadline: pending.Deadline}
	return state
}

// MatchesRun reports whether rc is the completion of the run state awaits.
func MatchesRun(state PRState, rc RunCompleted) bool {
	return state.Run != nil && rc.RunID != 0 && state.Run.RunID == rc.RunID
}

// Outcome is how an analysis ended: exactly one of Result or Failed is set.
type Outcome struct {
	Result *review.Result
	Failed *AnalysisFailed
}

func resultOutcome(r review.Result) Outcome { return Outcome{Result: &r} }

func failedOutcome(cause string) Outcome { return Outcome{Failed: &AnalysisFailed{Cause: cause}} }

// AnalysisFailed is an analysis that ended without a usable Result.
type AnalysisFailed struct {
	Cause string
}

// conclude is the state transition for an analysis that ended: pure, no I/O.
// It clears the awaited run and returns the completed check run to report.
func conclude(state PRState, outcome Outcome) (PRState, CheckRun) {
	state.Run = nil
	run := CheckRun{Name: checkName, HeadSHA: state.HeadSHA, Status: StatusCompleted}

	switch {
	case outcome.Result != nil:
		switch v := outcome.Result.Verdict.(type) {
		case review.NoImpact:
			run.Conclusion, run.Title, run.Summary = ConclusionSuccess, "No doc impact", v.Reason
			return state, run
		case review.Proposals:
			if len(v) > 0 {
				run.Conclusion, run.Title, run.Summary = ConclusionActionRequired, "Docs need updating", proposalsSummary(v)
				return state, run
			}
			return state, neutral(run, "Analysis failed", "runner returned an empty proposal list; no impact must be NoImpact")
		default:
			return state, neutral(run, "Analysis failed", fmt.Sprintf("unknown review.Verdict %T", outcome.Result.Verdict))
		}
	case outcome.Failed != nil:
		return state, neutral(run, "Analysis failed", outcome.Failed.Cause)
	default:
		return state, neutral(run, "Analysis failed", "analysis ended without an outcome")
	}
}

func neutral(run CheckRun, title, summary string) CheckRun {
	run.Conclusion, run.Title, run.Summary = ConclusionNeutral, title, summary
	return run
}

// Runners are the analysis runners a repo may use. A nil Runner means that
// runner is unavailable.
type Runners struct {
	Actions review.AsyncRunner
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
// the result as the docs-agent check run. A runner that finishes later leaves
// the check run in progress until HandleRunCompleted concludes it.
func (s *Service) HandlePullRequest(ctx context.Context, pr PullRequest) error {
	state, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: load state: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	if old, ok := Superseded(state, pr); ok {
		if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, old); err != nil {
			return fmt.Errorf("handle pull request %s/%s#%d: supersede check run %d: %w", pr.Owner, pr.Repo, pr.Number, state.CheckRunID, err)
		}
	}

	var hasWorkflow bool
	if s.runners.Actions != nil || s.runners.Server != nil {
		hasWorkflow, err = s.gh.WorkflowExists(ctx, pr.InstallationID, pr.Owner, pr.Repo)
		if err != nil {
			return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
		}
	}

	var started review.Started
	switch selectRunner(hasWorkflow, s.runners) {
	case runnerNone:
	case runnerActions:
		started, err = start(ctx, s.runners.Actions, pr)
	case runnerServer:
		started, err = start(ctx, s.runners.Server, pr)
	}
	if err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	next := OnPush(state, pr)
	switch res := started.(type) {
	case nil:
		run := CheckRun{
			Name:       checkName,
			HeadSHA:    pr.HeadSHA,
			Status:     StatusCompleted,
			Conclusion: ConclusionNeutral,
			Title:      "No analysis runner configured",
			Summary:    "Set up an analysis runner: " + setupGuideURL,
		}
		_, err = s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run)
	case review.Pending:
		var id int64
		id, err = s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, CheckRun{
			Name:    checkName,
			HeadSHA: pr.HeadSHA,
			Status:  StatusInProgress,
			Title:   "Analyzing docs impact",
			Summary: "Waiting for the docs-agent workflow run to finish.",
		})
		next = OnStarted(next, res, id)
	case review.Result:
		var run CheckRun
		next, run = conclude(next, resultOutcome(res))
		_, err = s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run)
	default:
		err = fmt.Errorf("unknown review.Started %T", started)
	}
	if err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: save state: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	return nil
}

// HandleRunCompleted concludes the check run of the analysis run rc reports,
// if it is the one the pull request awaits; any other completion is ignored.
func (s *Service) HandleRunCompleted(ctx context.Context, rc RunCompleted) error {
	state, err := s.store.LoadPR(ctx, rc.Owner, rc.Repo, rc.Number)
	if err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: load state: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}
	if !MatchesRun(state, rc) {
		return nil
	}

	outcome, err := s.collect(ctx, state, rc)
	if err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}

	next, run := conclude(state, outcome)
	if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}
	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: save state: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}

	return nil
}

// HandleDeadline concludes the check run neutral if the run identified by
// nonce is still awaited for ref and overdue at now; otherwise it does nothing.
func (s *Service) HandleDeadline(ctx context.Context, ref PRRef, nonce string, now time.Time) error {
	state, err := s.store.LoadPR(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return fmt.Errorf("handle deadline of %s/%s#%d: load state: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	if !Overdue(state, now) || state.Run.Nonce != nonce {
		return nil
	}

	next, run := conclude(state, failedOutcome("no result from the docs-agent workflow run before the deadline"))
	if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("handle deadline of %s/%s#%d: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("handle deadline of %s/%s#%d: save state: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	return nil
}

func (s *Service) collect(ctx context.Context, state PRState, rc RunCompleted) (Outcome, error) {
	if s.runners.Actions == nil {
		return Outcome{}, errors.New("collect result: no Actions runner configured")
	}

	result, err := s.runners.Actions.Collect(ctx, review.Completion{
		InstallationID: state.InstallationID,
		Owner:          state.Owner,
		Repo:           state.Repo,
		Number:         state.Number,
		HeadSHA:        state.HeadSHA,
		RunID:          state.Run.RunID,
		Nonce:          state.Run.Nonce,
	})
	var invalid *review.InvalidResultError
	failed := rc.Conclusion != "success"
	switch {
	case errors.As(err, &invalid) && failed:
		return failedOutcome("workflow run " + rc.Conclusion + ": " + invalid.Cause.Error()), nil
	case errors.As(err, &invalid):
		return failedOutcome(invalid.Error()), nil
	case failed:
		return failedOutcome("workflow run " + rc.Conclusion), nil
	case err != nil:
		return Outcome{}, fmt.Errorf("collect result: %w", err)
	default:
		return resultOutcome(result), nil
	}
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

func start(ctx context.Context, runner review.Runner, pr PullRequest) (review.Started, error) {
	started, err := runner.Start(ctx, review.Request{
		InstallationID: pr.InstallationID,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
		Number:         pr.Number,
		BaseSHA:        pr.BaseSHA,
		HeadSHA:        pr.HeadSHA,
	})
	if err != nil {
		return nil, fmt.Errorf("start analysis: %w", err)
	}
	return started, nil
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
