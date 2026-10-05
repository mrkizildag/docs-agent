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
)

// ScaffoldState is what the gate remembers about one repo's scaffold. A repo
// has at most one scaffold PR, ever: Opened is never left.
type ScaffoldState struct {
	Owner          string
	Repo           string
	InstallationID int64
	Phase          ScaffoldPhase
	Attempt        int          // bumped by every failure, so a retry is a new job
	BaseSHA        string       // default-branch tip the files were written from and the branch starts at
	Run            *AwaitingRun // the external run writing the files; set only while Awaiting
	Files          *review.Scaffold
	Branch         string
	PRNumber       int
	PRURL          string
}

// ScaffoldWaiter is a PR check run that waits for the scaffold PR's link.
type ScaffoldWaiter struct {
	CheckRunID int64
	PRNumber   int
}

// ScaffoldPR is a pull request the gate opened or adopted.
type ScaffoldPR struct {
	Number int
	URL    string
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
	BranchSHA(ctx context.Context, installationID int64, owner, repo, branch string) (string, error)
	CreatePullRequest(ctx context.Context, installationID int64, owner, repo string, pr NewPullRequest) (ScaffoldPR, error)
	// FindPullRequest returns the pull request, of any state, opened from branch.
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
	s.Phase, s.BaseSHA, s.Run, s.Files = ScaffoldWriting, baseSHA, nil, nil
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
func OnScaffoldOpened(s ScaffoldState, branch string, pr ScaffoldPR) ScaffoldState {
	s.Phase, s.Branch, s.PRNumber, s.PRURL, s.Run = ScaffoldOpened, branch, pr.Number, pr.URL, nil
	return s
}

// OnScaffoldFailed is the state transition for a failure before the PR exists:
// pure, no I/O. The next PR event tries again; finished files are kept, so a
// failed commit or PR never asks the runner to write them again.
func OnScaffoldFailed(s ScaffoldState) ScaffoldState {
	s.Attempt++
	s.Run = nil
	if s.Files == nil {
		s.Phase = ScaffoldIdle
	} else {
		s.Phase = ScaffoldWritten
	}
	return s
}

// OnScaffoldDocsPresent is the state transition for a default branch that has
// docs/ after all: pure, no I/O. It bumps Attempt like a failure does, because
// the job for the finished attempt must not swallow the next request.
func OnScaffoldDocsPresent(s ScaffoldState) ScaffoldState {
	s.Attempt++
	s.Phase, s.Run, s.Files = ScaffoldIdle, nil, nil
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

// noDocsRun is the neutral check run of a PR whose head has no docs/ folder. Its
// summary links the scaffold PR once s is Opened; with no runner it points at
// the setup guide instead.
func noDocsRun(headSHA string, s ScaffoldState, haveRunner bool) CheckRun {
	run := CheckRun{Name: CheckName, HeadSHA: headSHA, Status: StatusCompleted}
	switch {
	case !haveRunner:
		return neutral(run, noDocsTitle, "No scaffold can be written until an analysis runner is set up: "+setupGuideURL)
	case s.Phase == ScaffoldOpened:
		return neutral(run, noDocsTitle, "Pollux opened a pull request that adds a starting docs/ folder: "+s.PRURL)
	default:
		return neutral(run, noDocsTitle, "Pollux is writing a starting docs/ folder and will link the pull request here.")
	}
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
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, noDocsRun(pr.HeadSHA, ScaffoldState{}, false))
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
// check runs recorded as waiters. A repeat of any step is harmless.
func (s *Service) requestScaffold(ctx context.Context, state PRState, pr PullRequest) error {
	loaded, err := s.store.LoadScaffold(ctx, pr.Owner, pr.Repo)
	if err != nil {
		return fmt.Errorf("load scaffold: %w", err)
	}
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, noDocsRun(pr.HeadSHA, loaded, true))
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	scaffold, err := s.store.RequestScaffold(ctx, pr.InstallationID, pr.Owner, pr.Repo, ScaffoldWaiter{CheckRunID: id, PRNumber: pr.Number})
	if err != nil {
		return fmt.Errorf("request scaffold: %w", err)
	}
	if scaffold.Phase == ScaffoldOpened {
		if err := s.linkWaiters(ctx, scaffold); err != nil {
			return err
		}
	} else if err := s.scaffoldQueue.EnqueueScaffold(ctx, RepoRef{Owner: pr.Owner, Repo: pr.Repo}, scaffold.Attempt); err != nil {
		return fmt.Errorf("enqueue scaffold: %w", err)
	}

	next := OnPush(state, pr)
	next.CheckRunID = id
	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// HandleScaffold writes and proposes the scaffold of ref's repo unless one was
// already proposed or an external run is already writing it. A failure before
// the PR exists leaves the scaffold to be requested again by the next PR event
// and is returned.
func (s *Service) HandleScaffold(ctx context.Context, ref RepoRef) error {
	op := fmt.Sprintf("handle scaffold of %s/%s", ref.Owner, ref.Repo)
	state, err := s.store.LoadScaffold(ctx, ref.Owner, ref.Repo)
	if err != nil {
		return fmt.Errorf("%s: load state: %w", op, err)
	}
	if state.Phase == ScaffoldAwaiting {
		return nil
	}
	if err := s.advanceScaffold(ctx, state); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// HandleScaffoldRun continues the scaffold from the external run rc reports, if
// it is the one the repo awaits; any other completion is ignored. A run that
// failed or whose result is unusable fails the attempt; a result that could not
// be read is returned for retry.
func (s *Service) HandleScaffoldRun(ctx context.Context, rc RunCompleted) error {
	op := fmt.Sprintf("handle scaffold run %d of %s/%s", rc.RunID, rc.Owner, rc.Repo)
	state, err := s.store.LoadScaffold(ctx, rc.Owner, rc.Repo)
	if err != nil {
		return fmt.Errorf("%s: load state: %w", op, err)
	}
	if !MatchesScaffoldRun(state, rc) {
		return nil
	}

	var files review.Scaffold
	if rc.Conclusion == "success" {
		var invalid *review.InvalidResultError
		files, err = s.collectScaffold(ctx, state)
		if err != nil && !errors.As(err, &invalid) {
			return fmt.Errorf("%s: %w", op, err)
		}
	} else {
		err = fmt.Errorf("workflow run concluded %q", rc.Conclusion)
	}
	if err != nil {
		if serr := s.saveScaffoldFailed(ctx, state); serr != nil {
			err = errors.Join(err, serr)
		}
		return fmt.Errorf("%s: %w", op, err)
	}

	writeCtx, cancel := writeContext(ctx)
	state = OnScaffoldWritten(state, files)
	err = s.store.SaveScaffold(writeCtx, state)
	cancel()
	if err != nil {
		return fmt.Errorf("%s: save state: %w", op, err)
	}
	if err := s.advanceScaffold(ctx, state); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// HandleScaffoldDeadline fails the scaffold attempt if the run identified by
// nonce is still awaited for ref and overdue at now; otherwise it does nothing.
func (s *Service) HandleScaffoldDeadline(ctx context.Context, ref RepoRef, nonce string, now time.Time) error {
	state, err := s.store.LoadScaffold(ctx, ref.Owner, ref.Repo)
	if err != nil {
		return fmt.Errorf("handle scaffold deadline of %s/%s: load state: %w", ref.Owner, ref.Repo, err)
	}
	if !ScaffoldOverdue(state, now) || state.Run.Nonce != nonce {
		return nil
	}
	if err := s.saveScaffoldFailed(ctx, state); err != nil {
		return fmt.Errorf("handle scaffold deadline of %s/%s: %w", ref.Owner, ref.Repo, err)
	}
	return nil
}

// advanceScaffold takes state as far as it goes, then links the waiting check
// runs if the PR exists. A failure marks the attempt failed.
func (s *Service) advanceScaffold(ctx context.Context, state ScaffoldState) error {
	if state.Phase == ScaffoldOpened {
		// A job retried after the PR was saved still owes the waiters their link.
		return s.linkWaiters(ctx, state)
	}
	next, err := s.proposeScaffold(ctx, state)
	if err != nil {
		if serr := s.saveScaffoldFailed(ctx, next); serr != nil {
			err = errors.Join(err, serr)
		}
		return err
	}
	return s.linkWaiters(ctx, next)
}

// saveScaffoldFailed saves state as a failed attempt, surviving a cancelled ctx.
func (s *Service) saveScaffoldFailed(ctx context.Context, state ScaffoldState) error {
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := s.store.SaveScaffold(writeCtx, OnScaffoldFailed(state)); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
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
	var invalid *review.InvalidResultError
	files, err := s.runners.Actions.CollectScaffold(ctx, completion)
	backoff := s.collectBackoff
	for attempt := 1; attempt < collectAttempts && err != nil && !errors.As(err, &invalid); attempt++ {
		select {
		case <-ctx.Done():
			return review.Scaffold{}, fmt.Errorf("collect scaffold: %w", ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		files, err = s.runners.Actions.CollectScaffold(ctx, completion)
	}
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
		if err != nil || state.Phase == ScaffoldAwaiting {
			return state, err
		}
	}

	pr, err := s.openScaffoldPR(ctx, state, base)
	if err != nil {
		return state, err
	}
	state = OnScaffoldOpened(state, scaffoldBranch, pr)
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := s.store.SaveScaffold(writeCtx, state); err != nil {
		return state, fmt.Errorf("save state: %w", err)
	}
	return state, nil
}

// concludeDocsPresent tells the waiting check runs that the default branch has
// docs/ and frees the scaffold for a later request.
func (s *Service) concludeDocsPresent(ctx context.Context, state ScaffoldState) (ScaffoldState, error) {
	waiters, err := s.store.UnlinkedScaffoldWaiters(ctx, state.Owner, state.Repo)
	if err != nil {
		return state, fmt.Errorf("list waiting check runs: %w", err)
	}
	run := docsPresentRun("")
	var errs []error
	for _, w := range waiters {
		if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, w.CheckRunID, run); err != nil {
			errs = append(errs, fmt.Errorf("report docs/ in check run %d: %w", w.CheckRunID, err))
			continue
		}
		if err := s.store.MarkScaffoldWaiterLinked(ctx, state.Owner, state.Repo, w.CheckRunID); err != nil {
			errs = append(errs, fmt.Errorf("mark check run %d concluded: %w", w.CheckRunID, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return state, err
	}
	done := OnScaffoldDocsPresent(state)
	if err := s.store.SaveScaffold(ctx, done); err != nil {
		return state, fmt.Errorf("save state: %w", err)
	}
	return done, nil
}

// startScaffold has the runner for the repo write the files from tip. It returns
// the state Writing's successor: Written for files, Awaiting for a pending run.
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
		return state, errors.New("no analysis runner is configured")
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

// openScaffoldPR creates, or adopts, the branch at the state's base commit,
// commits the files when the branch is still there, and creates, or adopts, the
// pull request into base.
func (s *Service) openScaffoldPR(ctx context.Context, state ScaffoldState, base string) (ScaffoldPR, error) {
	inst, owner, repo := state.InstallationID, state.Owner, state.Repo
	if err := s.scaffoldGH.CreateBranch(ctx, inst, owner, repo, scaffoldBranch, state.BaseSHA); err != nil && !errors.Is(err, ErrBranchExists) {
		return ScaffoldPR{}, fmt.Errorf("create branch %s: %w", scaffoldBranch, err)
	}
	tip, err := s.scaffoldGH.BranchSHA(ctx, inst, owner, repo, scaffoldBranch)
	if err != nil {
		return ScaffoldPR{}, fmt.Errorf("read branch %s: %w", scaffoldBranch, err)
	}
	if tip == state.BaseSHA {
		files := []FileChange{
			{Path: indexPath, Content: state.Files.Index},
			{Path: "docs/architecture.md", Content: state.Files.Architecture},
			{Path: "docs/guides/setup.md", Content: state.Files.Setup},
		}
		if _, err := s.comments.CommitFiles(ctx, inst, owner, repo, scaffoldBranch, state.BaseSHA, files, scaffoldCommitMessage); err != nil {
			return ScaffoldPR{}, fmt.Errorf("commit scaffold to %s: %w", scaffoldBranch, err)
		}
	}

	pr, err := s.scaffoldGH.CreatePullRequest(ctx, inst, owner, repo, NewPullRequest{Title: scaffoldPRTitle, Body: scaffoldPRBody, Head: scaffoldBranch, Base: base})
	if err != nil {
		found, ok, ferr := s.scaffoldGH.FindPullRequest(ctx, inst, owner, repo, scaffoldBranch)
		if ferr != nil || !ok {
			return ScaffoldPR{}, errors.Join(fmt.Errorf("create pull request from %s: %w", scaffoldBranch, err), ferr)
		}
		return found, nil
	}
	return pr, nil
}

// linkWaiters edits every unlinked waiting check run to link the scaffold PR and
// marks each linked once edited. A failure on one does not stop the others; it
// stays unlinked for the next request or job.
func (s *Service) linkWaiters(ctx context.Context, state ScaffoldState) error {
	if state.Phase != ScaffoldOpened {
		return nil
	}
	waiters, err := s.store.UnlinkedScaffoldWaiters(ctx, state.Owner, state.Repo)
	if err != nil {
		return fmt.Errorf("list waiting check runs: %w", err)
	}
	run := noDocsRun("", state, true)
	var errs []error
	for _, w := range waiters {
		if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, w.CheckRunID, run); err != nil {
			errs = append(errs, fmt.Errorf("link scaffold pull request in check run %d: %w", w.CheckRunID, err))
			continue
		}
		if err := s.store.MarkScaffoldWaiterLinked(ctx, state.Owner, state.Repo, w.CheckRunID); err != nil {
			errs = append(errs, fmt.Errorf("mark check run %d linked: %w", w.CheckRunID, err))
		}
	}
	return errors.Join(errs...)
}
