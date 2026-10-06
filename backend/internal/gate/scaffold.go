package gate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const (
	noDocsTitle = "No docs/ folder"

	scaffoldBranch = "pollux-agent/docs-scaffold"

	// maxScaffoldAttempts is how many failed attempts a repo's scaffold gets
	// before the gate stops trying.
	maxScaffoldAttempts = 3

	scaffoldCommitMessage = "docs: add a starting docs/ folder"
	scaffoldPRTitle       = "Add a starting docs/ folder"
	scaffoldPRBody        = "Pollux found no `docs/` folder in this repository and wrote a starting structure from its code: " +
		"an index, an architecture overview and a setup guide.\n\n" +
		"Edit them freely; Pollux will keep them current in later pull requests. It opens this pull request once and does not update it."
)

// ErrBranchExists is returned by ScaffoldGitHub.CreateBranch when the branch already exists.
var ErrBranchExists = errors.New("gate: branch exists")

// RepoRef identifies a repository.
type RepoRef struct {
	Owner string
	Repo  string
}

// ScaffoldPhase is how far the repo's scaffold has come.
type ScaffoldPhase string

const (
	ScaffoldIdle     ScaffoldPhase = "idle"     // nothing written; the next PR event may start a scaffold
	ScaffoldWriting  ScaffoldPhase = "writing"  // a job is starting the runner; a crashed job leaves it here
	ScaffoldAwaiting ScaffoldPhase = "awaiting" // an external run is writing the files; Run says which
	ScaffoldWritten  ScaffoldPhase = "written"  // Files are final; branch, commit and PR remain
	ScaffoldOpened   ScaffoldPhase = "opened"   // the scaffold PR exists; terminal
	ScaffoldGaveUp   ScaffoldPhase = "gave_up"  // the last allowed attempt failed; terminal
)

// ScaffoldState is what the gate remembers about one repo's scaffold. A repo
// has at most one scaffold PR, ever: Opened is never left, and neither is GaveUp.
type ScaffoldState struct {
	Owner          string
	Repo           string
	InstallationID int64
	Phase          ScaffoldPhase
	Attempt        int          // job identity: bumped by every failure and docs-present conclusion, so a retry is a new job
	Failures       int          // failed attempts only; the scaffold gives up at maxScaffoldAttempts
	BaseSHA        string       // default-branch tip the files were written from and the branch starts at
	Run            *AwaitingRun // the external run writing the files; set only while Awaiting
	Files          *review.Scaffold
	CommitSHA      string // the commit of Files on the scaffold branch; empty until it succeeds, so a retry never commits twice
	PRNumber       int
	PRURL          string
}

// ScaffoldWaiter is a PR check run that waits for the scaffold PR's link.
type ScaffoldWaiter struct {
	CheckRunID int64
}

// ScaffoldPR is a pull request the gate opened or adopted.
type ScaffoldPR struct {
	Number int
	URL    string
	ByBot  bool // the author is this App's bot user
	Open   bool // the pull request is open
}

// NewPullRequest is a pull request to open from branch Head into Base.
type NewPullRequest struct {
	Title string
	Body  string
	Head  string
	Base  string
}

// ScaffoldGitHub is what writing and proposing a scaffold needs from GitHub.
type ScaffoldGitHub interface {
	// DefaultBranch returns the default branch's name and tip commit.
	DefaultBranch(ctx context.Context, installationID int64, owner, repo string) (name, sha string, err error)
	// CreateBranch creates branch at sha; it returns ErrBranchExists when branch exists.
	CreateBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error
	// ResetBranch force-moves an existing branch to sha.
	ResetBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error
	BranchSHA(ctx context.Context, installationID int64, owner, repo, branch string) (string, error)
	CreatePullRequest(ctx context.Context, installationID int64, owner, repo string, pr NewPullRequest) (ScaffoldPR, error)
	// FindPullRequest returns the pull request opened from branch, preferring the bot's (open first), then an open one, over the rest.
	FindPullRequest(ctx context.Context, installationID int64, owner, repo, branch string) (pr ScaffoldPR, ok bool, err error)
}

// ScaffoldQueue schedules the repo-keyed job that writes the scaffold; one
// attempt is one job, so a repeated request for the same attempt is a no-op.
type ScaffoldQueue interface {
	EnqueueScaffold(ctx context.Context, ref RepoRef, attempt int) error
}

// OnScaffoldStart is the state transition for a job that begins writing from
// baseSHA: pure, no I/O.
func OnScaffoldStart(s ScaffoldState, baseSHA string) ScaffoldState {
	s.Phase, s.BaseSHA, s.Run, s.Files, s.CommitSHA = ScaffoldWriting, baseSHA, nil, nil, ""
	return s
}

// OnScaffoldStarted is the state transition for a scaffold that is being written
// by an external run: pure, no I/O.
func OnScaffoldStarted(s ScaffoldState, pending review.Pending) ScaffoldState {
	s.Phase, s.Run = ScaffoldAwaiting, &AwaitingRun{RunID: pending.RunID, Nonce: pending.Nonce, Deadline: pending.Deadline}
	return s
}

// OnScaffoldWritten is the state transition for finished files: pure, no I/O.
func OnScaffoldWritten(s ScaffoldState, files review.Scaffold) ScaffoldState {
	s.Phase, s.Files, s.Run = ScaffoldWritten, &files, nil
	return s
}

// OnScaffoldOpened is the state transition for an existing scaffold PR: pure,
// no I/O.
func OnScaffoldOpened(s ScaffoldState, pr ScaffoldPR) ScaffoldState {
	s.Phase, s.PRNumber, s.PRURL, s.Run = ScaffoldOpened, pr.Number, pr.URL, nil
	return s
}

// OnScaffoldFailed is the state transition for a failure before the PR exists:
// pure, no I/O. The next PR event tries again; finished files are kept, so a
// failed commit or PR never asks the runner to write them again. The
// maxScaffoldAttempts-th failure ends in GaveUp. An Opened state is unchanged:
// the PR exists, so nothing failed.
func OnScaffoldFailed(s ScaffoldState) ScaffoldState {
	if s.Phase == ScaffoldOpened {
		return s
	}
	s.Attempt++
	s.Failures++
	s.Run = nil
	switch {
	case s.Failures >= maxScaffoldAttempts:
		s.Phase = ScaffoldGaveUp
	case s.Files == nil:
		s.Phase = ScaffoldIdle
	default:
		s.Phase = ScaffoldWritten
	}
	return s
}

// OnScaffoldDocsPresent is the state transition for a default branch that has
// docs/ after all: pure, no I/O. It bumps Attempt but not Failures, because the
// job for the finished attempt must not swallow the next request and the
// conclusion is not a failure.
func OnScaffoldDocsPresent(s ScaffoldState) ScaffoldState {
	s.Attempt++
	s.Phase, s.Run, s.Files, s.CommitSHA = ScaffoldIdle, nil, nil, ""
	return s
}

// onScaffoldNoRunner is the state transition for a job that finds no runner to
// write the files: pure, no I/O. It bumps Attempt but not Failures, because the
// job for the finished attempt must not swallow the next request and a missing
// runner is not a failed attempt.
func onScaffoldNoRunner(s ScaffoldState) ScaffoldState {
	s.Attempt++
	s.Phase, s.Run = ScaffoldIdle, nil
	return s
}

// MatchesScaffoldRun reports whether rc is the completion of the run state awaits.
func MatchesScaffoldRun(state ScaffoldState, rc RunCompleted) bool {
	return state.Phase == ScaffoldAwaiting && state.Run != nil && rc.RunID != 0 && state.Run.RunID == rc.RunID
}

// ScaffoldOverdue reports whether state awaits a run whose deadline has passed at now.
func ScaffoldOverdue(state ScaffoldState, now time.Time) bool {
	return state.Phase == ScaffoldAwaiting && state.Run != nil && now.After(state.Run.Deadline)
}

// noRunnerRun is the neutral check run of a PR whose head has no docs/ folder
// when no runner could write a scaffold.
func noRunnerRun(headSHA string) CheckRun {
	run := CheckRun{Name: CheckName, HeadSHA: headSHA, Status: StatusCompleted}
	return neutral(run, noDocsTitle, "No scaffold can be written until an analysis runner is set up: "+setupGuideURL)
}

// noDocsRun is the neutral check run of a PR whose head has no docs/ folder
// while a runner is available. Its summary links the scaffold PR once s is
// Opened and says to add docs/ by hand once s has GaveUp.
func noDocsRun(headSHA string, s ScaffoldState) CheckRun {
	run := CheckRun{Name: CheckName, HeadSHA: headSHA, Status: StatusCompleted}
	switch s.Phase {
	case ScaffoldOpened:
		return neutral(run, noDocsTitle, "Pollux opened a pull request that adds a starting docs/ folder: "+s.PRURL)
	case ScaffoldGaveUp:
		return neutral(run, noDocsTitle, fmt.Sprintf("Pollux could not write a starting docs/ folder after %d attempts; add docs/ by hand: %s", maxScaffoldAttempts, setupGuideURL))
	case ScaffoldIdle, ScaffoldWriting, ScaffoldAwaiting, ScaffoldWritten:
	}
	return neutral(run, noDocsTitle, "Pollux is writing a starting docs/ folder and will link the pull request here.")
}

// scaffoldFailedRun is the neutral check run of a PR waiting for a scaffold
// whose attempt just failed; s is the state after the failure.
func scaffoldFailedRun(s ScaffoldState) CheckRun {
	if s.Phase == ScaffoldGaveUp {
		return noDocsRun("", s)
	}
	run := CheckRun{Name: CheckName, Status: StatusCompleted}
	return neutral(run, noDocsTitle, "Pollux could not write a starting docs/ folder this time; the next pull request event in this repository tries again.")
}

// docsPresentRun is the neutral check run of a PR waiting for a scaffold that
// is not needed: the default branch has docs/ now.
func docsPresentRun(headSHA string) CheckRun {
	run := CheckRun{Name: CheckName, HeadSHA: headSHA, Status: StatusCompleted}
	return neutral(run, noDocsTitle, "The default branch already has a docs/ folder, so Pollux wrote no scaffold. Merge or rebase the default branch into this pull request to have its docs analyzed.")
}

// concludeNoDocs reports the neutral check run of a PR without docs/ when no
// runner could write a scaffold.
func (s *Service) concludeNoDocs(ctx context.Context, state PRState, pr PullRequest) error {
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, noRunnerRun(pr.HeadSHA))
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	next := OnPush(state, pr)
	next.CheckRunID = id
	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// requestScaffold reports the neutral check run of a PR without docs/ and asks
// for the repo's scaffold. The check run comes first: the scaffold job edits the
// check runs recorded as waiters. The PR's state is saved before any waiter is
// edited, so a failing waiter never loses it. A repeat of any step is harmless.
func (s *Service) requestScaffold(ctx context.Context, state PRState, pr PullRequest) error {
	loaded, err := s.store.LoadScaffold(ctx, pr.Owner, pr.Repo)
	if err != nil {
		return fmt.Errorf("load scaffold: %w", err)
	}
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, noDocsRun(pr.HeadSHA, loaded))
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	scaffold, err := s.store.RequestScaffold(ctx, pr.InstallationID, pr.Owner, pr.Repo, ScaffoldWaiter{CheckRunID: id})
	if err != nil {
		return fmt.Errorf("request scaffold: %w", err)
	}

	next := OnPush(state, pr)
	next.CheckRunID = id
	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	switch scaffold.Phase {
	case ScaffoldOpened, ScaffoldGaveUp:
		return s.concludeWaiters(ctx, scaffold, noDocsRun("", scaffold))
	case ScaffoldIdle, ScaffoldWriting, ScaffoldAwaiting, ScaffoldWritten:
	}
	if err := s.scaffoldQueue.EnqueueScaffold(ctx, RepoRef{Owner: pr.Owner, Repo: pr.Repo}, scaffold.Attempt); err != nil {
		return fmt.Errorf("enqueue scaffold: %w", err)
	}
	return nil
}

// HandleScaffold writes and proposes the scaffold of ref's repo unless one was
// already proposed or an external run is already writing it. A failure before
// the PR exists leaves the scaffold to be requested again by the next PR event
// and is returned.
func (s *Service) HandleScaffold(ctx context.Context, ref RepoRef) error {
	if err := s.handleScaffold(ctx, ref); err != nil {
		return fmt.Errorf("handle scaffold of %s/%s: %w", ref.Owner, ref.Repo, err)
	}
	return nil
}

func (s *Service) handleScaffold(ctx context.Context, ref RepoRef) error {
	state, err := s.store.LoadScaffold(ctx, ref.Owner, ref.Repo)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if state.Phase == ScaffoldAwaiting || state.Phase == ScaffoldGaveUp {
		return nil
	}
	return s.advanceScaffold(ctx, state)
}

// HandleScaffoldRun continues the scaffold from the external run rc reports, if
// it is the one the repo awaits; any other completion is ignored. A run that
// failed or whose result is unusable fails the attempt; a result that could not
// be read is returned for retry.
func (s *Service) HandleScaffoldRun(ctx context.Context, rc RunCompleted) error {
	if err := s.handleScaffoldRun(ctx, rc); err != nil {
		return fmt.Errorf("handle scaffold run %d of %s/%s: %w", rc.RunID, rc.Owner, rc.Repo, err)
	}
	return nil
}

func (s *Service) handleScaffoldRun(ctx context.Context, rc RunCompleted) error {
	state, err := s.store.LoadScaffold(ctx, rc.Owner, rc.Repo)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !MatchesScaffoldRun(state, rc) {
		return nil
	}

	var files review.Scaffold
	if rc.Conclusion == "success" {
		var invalid *review.InvalidResultError
		files, err = s.collectScaffold(ctx, state)
		if err != nil && !errors.As(err, &invalid) {
			return err
		}
	} else {
		err = fmt.Errorf("workflow run concluded %q", rc.Conclusion)
	}
	if err != nil {
		if serr := s.saveScaffoldFailed(ctx, state); serr != nil {
			err = errors.Join(err, serr)
		}
		return err
	}

	writeCtx, cancel := writeContext(ctx)
	state = OnScaffoldWritten(state, files)
	err = s.store.SaveScaffold(writeCtx, state)
	cancel()
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := s.advanceScaffold(ctx, state); err != nil {
		return s.healWaiters(ctx, RepoRef{Owner: rc.Owner, Repo: rc.Repo}, err)
	}
	return nil
}

// HandleScaffoldDeadline fails the scaffold attempt if the run identified by
// nonce is still awaited for ref and overdue at now; otherwise it does nothing.
func (s *Service) HandleScaffoldDeadline(ctx context.Context, ref RepoRef, nonce string, now time.Time) error {
	if err := s.handleScaffoldDeadline(ctx, ref, nonce, now); err != nil {
		return fmt.Errorf("handle scaffold deadline of %s/%s: %w", ref.Owner, ref.Repo, err)
	}
	return nil
}

func (s *Service) handleScaffoldDeadline(ctx context.Context, ref RepoRef, nonce string, now time.Time) error {
	state, err := s.store.LoadScaffold(ctx, ref.Owner, ref.Repo)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if !ScaffoldOverdue(state, now) || state.Run.Nonce != nonce {
		return nil
	}
	return s.saveScaffoldFailed(ctx, state)
}

// healWaiters returns err, after enqueueing a scaffold job for the saved
// state's attempt when err only left waiting check runs unedited: that job
// finds the waiters still unlinked and edits them. A failure to enqueue is
// joined to err.
func (s *Service) healWaiters(ctx context.Context, ref RepoRef, err error) error {
	var unedited *waitersError
	if !errors.As(err, &unedited) {
		return err
	}
	state, lerr := s.store.LoadScaffold(ctx, ref.Owner, ref.Repo)
	if lerr != nil {
		return errors.Join(err, fmt.Errorf("load scaffold: %w", lerr))
	}
	if qerr := s.scaffoldQueue.EnqueueScaffold(ctx, ref, state.Attempt); qerr != nil {
		return errors.Join(err, fmt.Errorf("enqueue scaffold: %w", qerr))
	}
	return err
}

// advanceScaffold takes state as far as it goes, then links the waiting check
// runs if the PR exists. A failure marks the attempt failed, except one after
// the PR exists and one that only left waiting check runs unedited, which a
// scaffold job for the saved state heals; a caller that is not such a job
// enqueues it with healWaiters.
func (s *Service) advanceScaffold(ctx context.Context, state ScaffoldState) error {
	next := state
	if state.Phase != ScaffoldOpened {
		// A job retried after the PR was saved still owes the waiters their link.
		var err error
		next, err = s.proposeScaffold(ctx, state)
		var unedited *waitersError
		if err != nil {
			if errors.As(err, &unedited) || next.Phase == ScaffoldOpened {
				return err
			}
			if serr := s.saveScaffoldFailed(ctx, next); serr != nil {
				err = errors.Join(err, serr)
			}
			return err
		}
	}
	if next.Phase != ScaffoldOpened {
		return nil
	}
	if err := s.concludeWaiters(ctx, next, noDocsRun("", next)); err != nil {
		return &waitersError{err}
	}
	return nil
}

// saveScaffoldFailed saves state as a failed attempt, surviving a cancelled
// ctx, then tells the waiting check runs. A failure to tell them is returned
// with the save's, never in place of the attempt's own error.
func (s *Service) saveScaffoldFailed(ctx context.Context, state ScaffoldState) error {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	failed := OnScaffoldFailed(state)
	if err := s.store.SaveScaffold(writeCtx, failed); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return s.updateWaiters(writeCtx, failed, scaffoldFailedRun(failed), false)
}

// collectScaffold reads the files of the run state awaits, retrying a transient
// error with backoff. It returns *review.InvalidResultError at once.
func (s *Service) collectScaffold(ctx context.Context, state ScaffoldState) (review.Scaffold, error) {
	if s.runners.Actions == nil {
		return review.Scaffold{}, errors.New("collect scaffold: no Actions runner configured")
	}
	completion := review.Completion{
		InstallationID: state.InstallationID,
		Owner:          state.Owner,
		Repo:           state.Repo,
		HeadSHA:        state.BaseSHA,
		RunID:          state.Run.RunID,
		Nonce:          state.Run.Nonce,
	}
	files, err := retry(ctx, collectAttempts, s.retryBackoff, func(ctx context.Context) (review.Scaffold, error) {
		return s.runners.Actions.CollectScaffold(ctx, completion)
	})
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("collect scaffold: %w", err)
	}
	return files, nil
}

// proposeScaffold takes state as far as an Opened scaffold, or an Awaiting one
// when an external run writes the files. On error it returns the state reached,
// which holds the files once they are written.
func (s *Service) proposeScaffold(ctx context.Context, state ScaffoldState) (ScaffoldState, error) {
	inst, owner, repo := state.InstallationID, state.Owner, state.Repo
	base, tip, err := s.scaffoldGH.DefaultBranch(ctx, inst, owner, repo)
	if err != nil {
		return state, fmt.Errorf("find default branch: %w", err)
	}
	present, err := s.gh.DocsExist(ctx, inst, owner, repo, tip)
	if err != nil {
		return state, fmt.Errorf("look for docs/ at %s: %w", shortSHA(tip), err)
	}
	if present {
		return s.concludeDocsPresent(ctx, state)
	}

	if state.Phase != ScaffoldWritten {
		state, err = s.startScaffold(ctx, state, tip)
		if err != nil || state.Phase != ScaffoldWritten {
			return state, err
		}
	}

	state, pr, err := s.openScaffoldPR(ctx, state, base)
	if err != nil {
		return state, err
	}
	state = OnScaffoldOpened(state, pr)
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := s.store.SaveScaffold(writeCtx, state); err != nil {
		return state, fmt.Errorf("save state: %w", err)
	}
	return state, nil
}

// concludeDocsPresent frees the scaffold for a later request, then tells the
// waiting check runs that the default branch has docs/. The state is saved
// first so a failing check run edit cannot lose it; the edit fails with a
// *waitersError, and a job for the saved state finds the waiters still unlinked.
func (s *Service) concludeDocsPresent(ctx context.Context, state ScaffoldState) (ScaffoldState, error) {
	done := OnScaffoldDocsPresent(state)
	writeCtx, cancel := writeContext(ctx)
	err := s.store.SaveScaffold(writeCtx, done)
	cancel()
	if err != nil {
		return state, fmt.Errorf("save state: %w", err)
	}
	if err := s.concludeWaiters(ctx, done, docsPresentRun("")); err != nil {
		return done, &waitersError{err}
	}
	return done, nil
}

// waitersError is a failure to edit the check runs waiting for a scaffold,
// which a later request or job retries; it is not a failed scaffold attempt.
type waitersError struct{ error }

func (e *waitersError) Unwrap() error { return e.error }

// startScaffold has the runner for the repo write the files from tip. It returns
// the state Writing's successor: Written for files, Awaiting for a pending run, Idle when no runner is available.
func (s *Service) startScaffold(ctx context.Context, state ScaffoldState, tip string) (ScaffoldState, error) {
	inst, owner, repo := state.InstallationID, state.Owner, state.Repo
	hasWorkflow, err := s.gh.WorkflowExists(ctx, inst, owner, repo)
	if err != nil {
		return state, fmt.Errorf("find workflow: %w", err)
	}
	var runner review.Scaffolder
	switch selectRunner(hasWorkflow, s.runners) {
	case runnerServer:
		runner = s.runners.Server
	case runnerActions:
		runner = s.runners.Actions
	case runnerNone:
		return s.concludeNoRunner(ctx, state)
	}

	state = OnScaffoldStart(state, tip)
	if err := s.store.SaveScaffold(ctx, state); err != nil {
		return state, fmt.Errorf("save state: %w", err)
	}
	started, err := runner.StartScaffold(ctx, review.ScaffoldRequest{InstallationID: inst, Owner: owner, Repo: repo, BaseSHA: state.BaseSHA})
	if err != nil {
		return state, fmt.Errorf("write scaffold: %w", err)
	}
	switch res := started.(type) {
	case review.Scaffold:
		state = OnScaffoldWritten(state, res)
	case review.Pending:
		state = OnScaffoldStarted(state, res)
	default:
		return state, fmt.Errorf("write scaffold: unexpected review.ScaffoldStarted %T", started)
	}
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := s.store.SaveScaffold(writeCtx, state); err != nil {
		return state, fmt.Errorf("save state: %w", err)
	}
	return state, nil
}

// concludeNoRunner frees the scaffold for a later request when no runner can
// write it, then tells the unlinked waiting check runs. It is not a failed
// attempt; the waiters stay unlinked for the request that finds a runner.
func (s *Service) concludeNoRunner(ctx context.Context, state ScaffoldState) (ScaffoldState, error) {
	idle := onScaffoldNoRunner(state)
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := s.store.SaveScaffold(writeCtx, idle); err != nil {
		return state, fmt.Errorf("save state: %w", err)
	}
	if err := s.updateWaiters(writeCtx, idle, noRunnerRun(""), false); err != nil {
		return idle, &waitersError{err}
	}
	return idle, nil
}

// openScaffoldPR adopts the pull request from the scaffold branch when Pollux
// opened it, in any state, so a scaffold PR is never opened twice. An open pull
// request from anyone else fails the attempt before anything is touched.
// Otherwise it creates the branch at the state's base commit; an existing branch
// at state's CommitSHA is Pollux's own earlier commit and is kept, one at the
// base commit is committed to, and any other tip is reset to the base commit. It
// commits the files unless that commit exists, then creates, or adopts, the pull
// request into base. The returned state records the commit, also on error.
func (s *Service) openScaffoldPR(ctx context.Context, state ScaffoldState, base string) (ScaffoldState, ScaffoldPR, error) {
	inst, owner, repo := state.InstallationID, state.Owner, state.Repo
	existing, ok, err := s.scaffoldGH.FindPullRequest(ctx, inst, owner, repo, scaffoldBranch)
	if err != nil {
		return state, ScaffoldPR{}, fmt.Errorf("find pull request from %s: %w", scaffoldBranch, err)
	}
	if ok && existing.ByBot {
		return state, existing, nil
	}
	if ok && existing.Open {
		return state, ScaffoldPR{}, fmt.Errorf("branch %s has an open pull request not opened by pollux: %s", scaffoldBranch, existing.URL)
	}

	committed := false
	err = s.scaffoldGH.CreateBranch(ctx, inst, owner, repo, scaffoldBranch, state.BaseSHA)
	switch {
	case err == nil:
	case errors.Is(err, ErrBranchExists):
		tip, err := s.scaffoldGH.BranchSHA(ctx, inst, owner, repo, scaffoldBranch)
		if err != nil {
			return state, ScaffoldPR{}, fmt.Errorf("read branch %s: %w", scaffoldBranch, err)
		}
		committed = state.CommitSHA != "" && tip == state.CommitSHA
		if !committed && tip != state.BaseSHA {
			if err := s.scaffoldGH.ResetBranch(ctx, inst, owner, repo, scaffoldBranch, state.BaseSHA); err != nil {
				return state, ScaffoldPR{}, fmt.Errorf("reset branch %s to %s: %w", scaffoldBranch, shortSHA(state.BaseSHA), err)
			}
		}
	default:
		return state, ScaffoldPR{}, fmt.Errorf("create branch %s: %w", scaffoldBranch, err)
	}

	if !committed {
		var files []FileChange
		for _, f := range state.Files.Files() {
			files = append(files, FileChange{Path: f.Path, Content: f.Content})
		}
		sha, err := s.comments.CommitFiles(ctx, inst, owner, repo, scaffoldBranch, state.BaseSHA, files, scaffoldCommitMessage)
		if err != nil {
			return state, ScaffoldPR{}, fmt.Errorf("commit scaffold to %s: %w", scaffoldBranch, err)
		}
		state.CommitSHA = sha
		writeCtx, cancel := writeContext(ctx)
		defer cancel()
		if err := s.store.SaveScaffold(writeCtx, state); err != nil {
			return state, ScaffoldPR{}, fmt.Errorf("save state: %w", err)
		}
	}

	pr, err := s.scaffoldGH.CreatePullRequest(ctx, inst, owner, repo, NewPullRequest{Title: scaffoldPRTitle, Body: scaffoldPRBody, Head: scaffoldBranch, Base: base})
	if err != nil {
		found, ok, ferr := s.scaffoldGH.FindPullRequest(ctx, inst, owner, repo, scaffoldBranch)
		if ferr != nil || !ok || !found.ByBot {
			return state, ScaffoldPR{}, errors.Join(fmt.Errorf("create pull request from %s: %w", scaffoldBranch, err), ferr)
		}
		return state, found, nil
	}
	return state, pr, nil
}

// concludeWaiters edits every unlinked waiting check run to run and marks each
// done once edited. A failure on one does not stop the others; it stays
// unlinked for the next request or job.
func (s *Service) concludeWaiters(ctx context.Context, state ScaffoldState, run CheckRun) error {
	return s.updateWaiters(ctx, state, run, true)
}

// updateWaiters edits every unlinked waiting check run to run, marking each
// linked when mark is set, and joins the failures.
func (s *Service) updateWaiters(ctx context.Context, state ScaffoldState, run CheckRun, mark bool) error {
	waiters, err := s.store.UnlinkedScaffoldWaiters(ctx, state.Owner, state.Repo)
	if err != nil {
		return fmt.Errorf("list waiting check runs: %w", err)
	}
	var errs []error
	for _, w := range waiters {
		if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, w.CheckRunID, run); err != nil {
			errs = append(errs, fmt.Errorf("update waiting check run %d: %w", w.CheckRunID, err))
			continue
		}
		if !mark {
			continue
		}
		if err := s.store.MarkScaffoldWaiterLinked(ctx, state.Owner, state.Repo, w.CheckRunID); err != nil {
			errs = append(errs, fmt.Errorf("mark check run %d done: %w", w.CheckRunID, err))
		}
	}
	return errors.Join(errs...)
}
