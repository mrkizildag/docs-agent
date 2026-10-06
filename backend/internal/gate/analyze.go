package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// HandlePullRequest selects an analysis runner for pr, runs it, and reports
// the result as the pollux-agent check run. A runner that finishes later leaves
// the check run in progress until HandleRunCompleted concludes it. A push of the
// commit a crashed Apply made keeps that Apply's proposals applied.
func (s *Service) HandlePullRequest(ctx context.Context, pr PullRequest) error {
	if err := s.handlePullRequest(ctx, pr); err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
	}
	return nil
}

func (s *Service) handlePullRequest(ctx context.Context, pr PullRequest) error {
	state, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	return s.analyzeHead(ctx, state, pr)
}

// analyzeHead analyzes pr's head against the stored state, as a push or a
// re-run does: the push of a crashed Apply's commit keeps its proposals
// applied, and a pending skip ask the new head cancels gets a note.
func (s *Service) analyzeHead(ctx context.Context, loaded PRState, pr PullRequest) error {
	state, adopted, err := s.adoptPendingApply(ctx, loaded, pr)
	if err != nil {
		return fmt.Errorf("adopt pending apply: %w", err)
	}
	// Once the adopted state is saved, a failed comment write must not stop the
	// analysis of the new head.
	var commentErr error
	if adopted {
		writeCtx, cancel := writeContext(ctx)
		_, commentErr = s.finishApply(writeCtx, state, loaded.PendingApply.IDs, "")
		cancel()
	}
	analyzeErr := errors.Join(commentErr, s.analyze(ctx, state, pr))
	noteCtx, cancel := writeContext(ctx)
	defer cancel()

	// The ask is cancelled once the new head is saved, so a retried job finds no
	// pending skip and the note is posted once; an analysis that failed before
	// saving leaves the ask for the retry.
	if ask := pendingSkipCancelled(loaded, pr); ask != nil && (analyzeErr == nil || s.headSaved(noteCtx, pr)) {
		label := skipCommitLabel
		if ask.Scope == SkipPR {
			label = skipPRLabel
		}
		body := fmt.Sprintf("@%s, a new push arrived before your reason, so the skip for `%s` was cancelled. Tick **%s** again to skip the new head.", ask.User, shortSHA(loaded.HeadSHA), label)
		if _, err := s.gh.CreateIssueComment(noteCtx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, body); err != nil {
			return errors.Join(analyzeErr, fmt.Errorf("post skip cancellation: %w", err))
		}
	}
	return analyzeErr
}

// headSaved reports whether the stored state is already at pr's head; a failed
// load counts as not saved.
func (s *Service) headSaved(ctx context.Context, pr PullRequest) bool {
	latest, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	return err == nil && latest.HeadSHA == pr.HeadSHA
}

// HandleRerun starts a fresh analysis of the pull request's current head, as a
// push would, unless r names a summary comment that is not the one state holds,
// the pull request is not open, or its current head is already being analyzed
// within its deadline.
func (s *Service) HandleRerun(ctx context.Context, r RerunRequest) error {
	if err := s.handleRerun(ctx, r); err != nil {
		return fmt.Errorf("handle rerun of %s/%s#%d: %w", r.PRRef.Owner, r.PRRef.Repo, r.PRRef.Number, err)
	}
	return nil
}

func (s *Service) handleRerun(ctx context.Context, r RerunRequest) error {
	ref := r.PRRef
	state, err := s.store.LoadPR(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if r.SummaryCommentID != 0 && r.SummaryCommentID != state.SummaryCommentID {
		return nil
	}
	pr, err := s.gh.GetPullRequest(ctx, r.InstallationID, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return fmt.Errorf("get pull request: %w", err)
	}
	if !pr.Open || (state.Run != nil && state.HeadSHA == pr.HeadSHA && !overdue(state, time.Now())) {
		return nil
	}
	return s.analyzeHead(ctx, state, pr)
}

// analyze closes the check run of any awaited analysis, then starts a new one
// on the runner the repo uses.
func (s *Service) analyze(ctx context.Context, state PRState, pr PullRequest) error {
	if old, ok := superseded(state, pr); ok {
		if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, old); err != nil {
			return fmt.Errorf("supersede check run %d: %w", state.CheckRunID, err)
		}
	}

	if next := OnPush(state, pr); next.Skip != nil && next.Skip.Scope == SkipPR {
		return s.concludeSkipped(ctx, next, pr)
	}

	docsExist, err := s.gh.DocsExist(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.HeadSHA)
	if err != nil {
		return fmt.Errorf("look for docs/ at %s: %w", shortSHA(pr.HeadSHA), err)
	}

	var hasWorkflow bool
	if s.runners.Actions != nil || s.runners.Server != nil {
		hasWorkflow, err = s.gh.WorkflowExists(ctx, pr.InstallationID, pr.Owner, pr.Repo)
		if err != nil {
			return fmt.Errorf("find workflow: %w", err)
		}
	}
	selected := selectRunner(hasWorkflow, s.runners)

	if !docsExist {
		if selected == runnerNone {
			return s.concludeNoDocs(ctx, state, pr)
		}
		return s.requestScaffold(ctx, state, pr)
	}

	switch selected {
	case runnerActions:
		return s.startRun(ctx, state, pr, s.runners.Actions)
	case runnerServer:
		return s.startRun(ctx, state, pr, s.runners.Server)
	case runnerNone:
	}

	run := CheckRun{
		Name:       CheckName,
		HeadSHA:    pr.HeadSHA,
		Status:     StatusCompleted,
		Conclusion: ConclusionNeutral,
		Title:      "No analysis runner configured",
		Summary:    "Set up an analysis runner: " + setupGuideURL,
	}
	if _, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run); err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	if err := s.store.SavePR(ctx, OnPush(state, pr)); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// concludeSkipped reports the active PR skip as the check run for the new head
// without starting any analysis.
func (s *Service) concludeSkipped(ctx context.Context, next PRState, pr PullRequest) error {
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, skipRun(next))
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	next.CheckRunID = id
	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// startRun reports an in-progress check run, arms its deadline in saved state,
// then runs the analysis. The check run and the armed state come first so a
// failed or cancelled start can still close the check run, and the deadline
// sweep can if nothing else does; once started, the state writes outlive a
// cancelled ctx so the next job can find and close the check run.
func (s *Service) startRun(ctx context.Context, state PRState, pr PullRequest, runner review.Runner) error {
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, CheckRun{
		Name:    CheckName,
		HeadSHA: pr.HeadSHA,
		Status:  StatusInProgress,
		Title:   "Analyzing docs impact",
		Summary: "Waiting for the analysis to finish.",
	})
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}

	next := OnPush(state, pr)
	next.CheckRunID = id
	next.Run = &AwaitingRun{Nonce: fmt.Sprintf("check-%d", id), Deadline: time.Now().Add(analysisDeadline)}

	var started review.Started
	var changed []review.ChangedFile
	armCtx, cancelArm := writeContext(ctx)
	err = s.store.SavePR(armCtx, next)
	cancelArm()
	if err != nil {
		err = fmt.Errorf("save state: %w", err)
	} else {
		started, changed, err = s.start(ctx, runner, pr)
		if err != nil && ctx.Err() != nil {
			// Superseded or shutting down: the armed state stays so the next job
			// closes this check run as superseded, or the deadline sweep does.
			return fmt.Errorf("analysis of %s/%s#%d interrupted: %w", pr.Owner, pr.Repo, pr.Number, errors.Join(err, context.Cause(ctx)))
		}
	}

	// The write budget starts once the analysis returns; a server analysis can
	// take longer than writeTimeout on its own.
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err == nil {
		switch res := started.(type) {
		case review.Pending:
			next = onStarted(next, res, id)
		case review.Result:
			if out := resultOutcome(res); out.Failed != nil {
				err = &unusableResultError{cause: out.Failed.Cause}
				break
			}
			if next, err = s.concludeResult(writeCtx, next, pr, res, changed); err != nil {
				return err
			}
		default:
			err = fmt.Errorf("unknown review.Started %T", started)
		}
	}
	if err != nil {
		return s.failRun(writeCtx, next, pr, err)
	}

	if err := s.store.SavePR(writeCtx, next); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// failRun concludes the check run neutral for cause, reports it in the summary
// comment, and saves the concluded state. It returns cause, as a
// *reportedFailure when those steps succeeded, else joined with their error.
func (s *Service) failRun(ctx context.Context, state PRState, pr PullRequest, cause error) error {
	outcome := failedOutcome(failureCause(cause))
	var large *tooLargeError
	var unusable *unusableResultError
	switch {
	case errors.As(cause, &large):
		outcome = Outcome{Failed: &AnalysisFailed{Title: titleTooLarge, Cause: large.limit}}
	case errors.As(cause, &unusable):
		outcome = failedOutcome(unusable.cause)
	}
	if err := s.concludeFailed(ctx, state, pr, outcome); err != nil {
		return errors.Join(cause, err)
	}
	return &reportedFailure{cause}
}

// reportedFailure is an analysis failure that the check run and the summary
// already report, so a caller that only wants the analysis to have run is done.
type reportedFailure struct{ error }

func (r *reportedFailure) Unwrap() error { return r.error }

// concludeFailed concludes the check run neutral for a failed outcome, writes
// the summary comment with its cause, and saves the concluded state. If the
// check run cannot be concluded the armed state stays so the deadline sweep retries.
func (s *Service) concludeFailed(ctx context.Context, state PRState, pr PullRequest, outcome Outcome) error {
	next, run := conclude(state, outcome)
	if err := s.gh.UpdateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("conclude check run %d: %w", state.CheckRunID, err)
	}
	var errs []error
	if withSummary, err := s.postFailureSummary(ctx, next, pr); err != nil {
		errs = append(errs, err)
	} else {
		next = withSummary
	}
	if err := s.store.SavePR(ctx, next); err != nil {
		errs = append(errs, fmt.Errorf("save state: %w", err))
	}
	return errors.Join(errs...)
}

// concludeResult concludes the check run for a result whose verdict is usable,
// saves state and posts its comments, retrying the posts with backoff. If every
// post fails the run is re-armed so the deadline sweep ends the check neutral.
// Callers save the returned state.
func (s *Service) concludeResult(ctx context.Context, state PRState, pr PullRequest, res review.Result, changed []review.ChangedFile) (PRState, error) {
	next, run := conclude(state, resultOutcome(res))
	if err := s.gh.UpdateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, state.CheckRunID, run); err != nil {
		return PRState{}, fmt.Errorf("conclude check run %d: %w", state.CheckRunID, err)
	}
	if err := s.store.SavePR(ctx, next); err != nil {
		return PRState{}, fmt.Errorf("save state: %w", err)
	}
	posted, err := retry(ctx, postAttempts, s.retryBackoff, func(ctx context.Context) (PRState, error) {
		return s.postComments(ctx, next, pr, res.Verdict, changed)
	})
	if err != nil {
		// A check claiming proposals that were never posted is worse than a neutral
		// one the user can re-run, so re-arm the run for the deadline sweep.
		if rerr := s.rearm(ctx, pr, state.Run); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}
	return posted, err
}

// rearm restores run on the stored state, which keeps whatever comment IDs the
// failed posts saved.
func (s *Service) rearm(ctx context.Context, pr PullRequest, run *AwaitingRun) error {
	latest, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("load state to re-arm run: %w", err)
	}
	latest.Run = run
	if err := s.store.SavePR(ctx, latest); err != nil {
		return fmt.Errorf("save re-armed state: %w", err)
	}
	return nil
}

// failureCause is the fixed one-line text for err; error text from a model or
// provider never reaches GitHub.
func failureCause(err error) string {
	var failed *review.FailedError
	if errors.As(err, &failed) {
		switch failed.Cause {
		case review.CauseProvider:
			return "The model provider returned an error."
		case review.CauseTimeout:
			return "The analysis timed out."
		case review.CauseLimit:
			return "The analysis hit its step or token limit."
		case review.CauseTooManyCandidates:
			return "Too many docs cover the changed files."
		case review.CauseClone:
			return "Cloning the repository failed."
		case review.CauseInternal:
			return "The analysis failed unexpectedly."
		}
	}
	return "The analysis failed unexpectedly."
}

// HandleRunCompleted concludes the check run of the analysis run rc reports,
// if it is the one the pull request awaits; any other completion is ignored.
func (s *Service) HandleRunCompleted(ctx context.Context, rc RunCompleted) error {
	if err := s.handleRunCompleted(ctx, rc); err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}
	return nil
}

func (s *Service) handleRunCompleted(ctx context.Context, rc RunCompleted) error {
	state, err := s.store.LoadPR(ctx, rc.Owner, rc.Repo, rc.Number)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !MatchesRun(state, rc) {
		return nil
	}

	outcome, err := s.collect(ctx, state, rc)
	if err != nil && outcome.Failed == nil {
		return err
	}

	pr := state.pullRequest()
	if outcome.Failed != nil {
		// err is the detail behind the fixed cause; it goes to the job log only.
		return errors.Join(err, s.concludeFailed(ctx, state, pr, outcome))
	}

	changed, err := s.gh.ListChangedFiles(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("list changed files: %w", err)
	}
	next, err := s.concludeResult(ctx, state, pr, *outcome.Result, changed)
	if err != nil {
		return err
	}
	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// HandleDeadline concludes the check run neutral if the run identified by
// nonce is still awaited for ref and overdue at now; otherwise it does nothing.
func (s *Service) HandleDeadline(ctx context.Context, ref PRRef, nonce string, now time.Time) error {
	if err := s.handleDeadline(ctx, ref, nonce, now); err != nil {
		return fmt.Errorf("handle deadline of %s/%s#%d: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	return nil
}

func (s *Service) handleDeadline(ctx context.Context, ref PRRef, nonce string, now time.Time) error {
	state, err := s.store.LoadPR(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !overdue(state, now) || state.Run.Nonce != nonce {
		return nil
	}

	cause := "The analysis did not report a result before the deadline."
	if state.Run.RunID != 0 {
		cause = "The pollux-agent workflow run did not report a result before the deadline."
	}
	return s.concludeFailed(ctx, state, state.pullRequest(), failedOutcome(cause))
}

// runFailureCause is the fixed one-line text for a workflow run that did not succeed.
func runFailureCause(conclusion string) string {
	switch conclusion {
	case "failure":
		return "The pollux-agent workflow run failed."
	case "cancelled":
		return "The pollux-agent workflow run was cancelled."
	case "timed_out":
		return "The pollux-agent workflow run timed out."
	default:
		return "The pollux-agent workflow run did not succeed."
	}
}

// collect returns the outcome of the run rc reports. When the run failed or its
// result is unusable it returns a failed outcome with a fixed cause together
// with the detail error, which must not reach GitHub.
func (s *Service) collect(ctx context.Context, state PRState, rc RunCompleted) (Outcome, error) {
	if s.runners.Actions == nil {
		return Outcome{}, errors.New("collect result: no Actions runner configured")
	}

	completion := review.Completion{
		InstallationID: state.InstallationID,
		Owner:          state.Owner,
		Repo:           state.Repo,
		Number:         state.Number,
		HeadSHA:        state.HeadSHA,
		RunID:          state.Run.RunID,
		Nonce:          state.Run.Nonce,
	}
	var invalid *review.InvalidResultError
	failed := rc.Conclusion != "success"

	var result review.Result
	var err error
	if failed {
		result, err = s.runners.Actions.Collect(ctx, completion)
	} else {
		result, err = retry(ctx, collectAttempts, s.retryBackoff, func(ctx context.Context) (review.Result, error) {
			return s.runners.Actions.Collect(ctx, completion)
		})
	}
	if err != nil && !failed && ctx.Err() != nil {
		return Outcome{}, fmt.Errorf("collect result: %w", err)
	}
	switch {
	case failed:
		return failedOutcome(runFailureCause(rc.Conclusion)), err
	case errors.As(err, &invalid):
		return failedOutcome("The pollux-agent workflow run returned an invalid result."), err
	case err != nil:
		return failedOutcome("Pollux could not read the workflow run's result."), err
	default:
		return resultOutcome(result), nil
	}
}

// retry returns fn's result, retrying an error with a doubling backoff up to
// attempts tries. It stops at once on *review.InvalidResultError, which a retry
// cannot fix, or when ctx ends.
func retry[T any](ctx context.Context, attempts int, backoff time.Duration, fn func(context.Context) (T, error)) (T, error) {
	var invalid *review.InvalidResultError
	result, err := fn(ctx)
	for attempt := 1; attempt < attempts && err != nil && !errors.As(err, &invalid); attempt++ {
		select {
		case <-ctx.Done():
			var zero T
			return zero, errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		result, err = fn(ctx)
	}
	return result, err
}

func (s *Service) start(ctx context.Context, runner review.Runner, pr PullRequest) (review.Started, []review.ChangedFile, error) {
	changed, err := s.gh.ListChangedFiles(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return nil, nil, fmt.Errorf("list changed files: %w", err)
	}
	if limit, ok := oversized(changed); ok {
		return nil, nil, &tooLargeError{limit: limit}
	}

	mergeBase, err := s.gh.MergeBase(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		return nil, nil, fmt.Errorf("find merge base: %w", err)
	}

	started, err := runner.Start(ctx, review.Request{
		InstallationID: pr.InstallationID,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
		Number:         pr.Number,
		BaseSHA:        mergeBase,
		HeadSHA:        pr.HeadSHA,
		ChangedFiles:   changed,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start analysis: %w", err)
	}
	return started, changed, nil
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
