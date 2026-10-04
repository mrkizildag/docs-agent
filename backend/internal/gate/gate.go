// Package gate is the domain: deciding what check run a pull request gets.
package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// setupGuideURL is linked from the neutral check when no runner is available.
const setupGuideURL = "https://github.com/mrkizildag/pollux-agent/blob/main/docs/guides/setup.md"

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
	// pollux-agent Actions workflow.
	WorkflowExists(ctx context.Context, installationID int64, owner, repo string) (bool, error)
	// ListChangedFiles returns the files in the pull request's diff with their head-side hunk ranges.
	ListChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error)
	// ListComments returns the pull request's review comments and issue comments.
	ListComments(ctx context.Context, installationID int64, owner, repo string, number int) ([]Comment, error)
	CreateReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, c ReviewComment) (Comment, error)
	EditReviewComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error
	CreateIssueComment(ctx context.Context, installationID int64, owner, repo string, number int, body string) (Comment, error)
	EditIssueComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error
}

// CommentKind says which GitHub comment API a Comment lives in; the two have
// separate ID spaces and edit endpoints.
type CommentKind string

const (
	CommentKindReview CommentKind = "review"
	CommentKindIssue  CommentKind = "issue"
)

// Comment is a comment on a pull request. Path, StartLine and Line locate a
// review comment and are zero for issue comments and for review comments GitHub
// no longer anchors. Mine is true when the App's bot user wrote the comment;
// only those are ever adopted or edited.
type Comment struct {
	ID        int64
	Mine      bool
	Kind      CommentKind
	URL       string
	Body      string
	Path      string
	StartLine int
	Line      int
}

// ReviewComment is a new review comment on the right side of a file in the
// head commit. StartLine 0 means a single-line comment on Line.
type ReviewComment struct {
	CommitSHA string
	Path      string
	StartLine int
	Line      int
	Body      string
}

const checkName = "pollux-agent"

// WorkflowPath is the target-repo workflow whose presence selects the Actions
// runner and whose completion carries its result.
const WorkflowPath = ".github/workflows/pollux-agent.yml"

// PRState is what the gate remembers about one pull request between events.
type PRState struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string // head commit the gate last reported a check run for; "" if never
	CheckRunID     int64  // check run reported for HeadSHA; 0 if none
	Run            *AwaitingRun

	SummaryCommentID int64 // 0 until the summary comment is created
	Proposals        []ProposalState
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

// ProposalStatus is whether a proposal still applies to the latest head.
type ProposalStatus string

const (
	ProposalOpen     ProposalStatus = "open"
	ProposalOutdated ProposalStatus = "outdated"
)

// ProposalState is one proposal's review comment as the gate remembers it.
type ProposalState struct {
	ID         string // see ProposalID
	DocPath    string
	Section    string
	CommentID  int64 // 0 until the review comment is created
	CommentURL string
	State      ProposalStatus
}

// ProposalID is the stable identity of a proposal across re-runs: a short hash
// of its doc path and normalized section heading (path alone for a new doc).
func ProposalID(docPath, section string) string {
	section = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(section), "#"))
	sum := sha256.Sum256([]byte(docPath + "\x00" + section))
	return hex.EncodeToString(sum[:6])
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
// drops any awaited run, so that run's result is ignored; the proposals and
// the summary comment carry over.
func OnPush(prev PRState, pr PullRequest) PRState {
	return PRState{
		InstallationID:   pr.InstallationID,
		Owner:            pr.Owner,
		Repo:             pr.Repo,
		Number:           pr.Number,
		HeadSHA:          pr.HeadSHA,
		SummaryCommentID: prev.SummaryCommentID,
		Proposals:        slices.Clone(prev.Proposals),
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

const (
	// maxSummaryBytes is GitHub's limit on a check run summary.
	maxSummaryBytes = 65535
	maxCauseBytes   = 1000
	truncatedMark   = "… (truncated)"
	// writeTimeout bounds the state writes that must survive a cancelled job.
	writeTimeout = 30 * time.Second
	// collectAttempts is how many times a result download is tried.
	collectAttempts = 3
)

// truncate cuts s to at most max bytes on a UTF-8 boundary, ending in a marker
// when it cut anything.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(truncatedMark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMark
}

// conclude is the state transition for an analysis that ended: pure, no I/O.
// It clears the awaited run and returns the completed check run to report;
// runner-supplied text is capped to what GitHub accepts.
func conclude(state PRState, outcome Outcome) (PRState, CheckRun) {
	state, run := concludeUncapped(state, outcome)
	run.Summary = truncate(run.Summary, maxSummaryBytes)
	return state, run
}

func concludeUncapped(state PRState, outcome Outcome) (PRState, CheckRun) {
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
		return state, neutral(run, "Analysis failed", truncate(outcome.Failed.Cause, maxCauseBytes))
	default:
		return state, neutral(run, "Analysis failed", "analysis ended without an outcome")
	}
}

func neutral(run CheckRun, title, summary string) CheckRun {
	run.Conclusion, run.Title, run.Summary = ConclusionNeutral, title, summary
	return run
}

// CommentWrite is a comment write Reconcile asks the Service to perform.
// Summary writes target the summary comment; the Service renders its body from
// the final state because it links comments created by earlier writes.
// Otherwise Index is the proposal in State.Proposals. ID 0 means create (Review
// holds the new review comment); else edit comment ID to Body.
type CommentWrite struct {
	Summary bool
	Index   int
	ID      int64
	Review  ReviewComment
	Body    string
}

// Reconcile is the state transition for a finished run: pure, no I/O. It
// returns prev with only Proposals and SummaryCommentID changed, and the
// comment writes that realize it. existing is the PR's current comments: our
// own comments carrying our markers are reused when state lacks their IDs, and
// an outdated proposal keeps its current body. Created comments' IDs and URLs
// belong in the returned state at the writes' Index.
func Reconcile(prev PRState, pr PullRequest, verdict review.Verdict, changed []review.ChangedFile, existing []Comment) (PRState, []CommentWrite) {
	next := prev
	next.Proposals = slices.Clone(prev.Proposals)
	proposals, _ := verdict.(review.Proposals)

	index := make(map[string]int, len(next.Proposals))
	for i, ps := range next.Proposals {
		index[ps.ID] = i
	}
	current := make(map[string]bool, len(proposals))
	var writes []CommentWrite

	for _, p := range proposals {
		id := ProposalID(p.DocPath, p.Section)
		if current[id] {
			continue
		}
		current[id] = true
		i, ok := index[id]
		if !ok {
			i = len(next.Proposals)
			index[id] = i
			next.Proposals = append(next.Proposals, ProposalState{ID: id})
		}
		ps := &next.Proposals[i]
		ps.DocPath, ps.Section, ps.State = p.DocPath, p.Section, ProposalOpen
		adoptMarked(ps, existing)

		p = withHeading(p)
		rc := proposalComment(pr.HeadSHA, id, p, changed)
		if c, ok := findComment(existing, CommentKindReview, ps.CommentID); ok {
			if !sameAnchor(c, rc) {
				rc.Body = renderCheckbox(id, p)
			}
			writes = append(writes, CommentWrite{Index: i, ID: ps.CommentID, Body: rc.Body})
		} else {
			ps.CommentID, ps.CommentURL = 0, ""
			writes = append(writes, CommentWrite{Index: i, Review: rc})
		}
	}

	for i := range next.Proposals {
		ps := &next.Proposals[i]
		if current[ps.ID] || ps.State == ProposalOutdated {
			continue
		}
		adoptMarked(ps, existing)
		ps.State = ProposalOutdated
		if c, ok := findComment(existing, CommentKindReview, ps.CommentID); ok {
			writes = append(writes, CommentWrite{Index: i, ID: ps.CommentID, Body: renderOutdated(ps.ID, pr.HeadSHA, c.Body)})
		}
	}

	summaryLost := false
	if _, ok := findComment(existing, CommentKindIssue, next.SummaryCommentID); !ok {
		summaryLost = next.SummaryCommentID != 0
		next.SummaryCommentID = 0
		if c, ok := findMarked(existing, CommentKindIssue, summaryMarker); ok {
			next.SummaryCommentID, summaryLost = c.ID, false
		}
	}
	if next.SummaryCommentID != 0 || len(proposals) > 0 || summaryLost {
		writes = append(writes, CommentWrite{Summary: true, ID: next.SummaryCommentID})
	}
	return next, writes
}

// withHeading restores the section's heading (and the blank lines after it)
// when a runner's Content omits it, so rendering and applying it never drop
// the heading from the doc.
func withHeading(p review.Proposal) review.Proposal {
	if p.Section == "" || p.Original == "" {
		return p
	}
	heading, rest, _ := strings.Cut(p.Original, "\n")
	if first, _, _ := strings.Cut(strings.TrimLeft(p.Content, " \t\r\n"), "\n"); headingLevel(first) == headingLevel(heading) {
		return p
	}
	lead := heading + "\n"
	for strings.HasPrefix(rest, "\n") {
		lead += "\n"
		rest = rest[1:]
	}
	p.Content = lead + strings.TrimLeft(p.Content, "\n")
	return p
}

// headingLevel is the ATX level of line ("## x" is 2), or 0 when it is not a heading.
func headingLevel(line string) int {
	line = strings.TrimSpace(line)
	level := len(line) - len(strings.TrimLeft(line, "#"))
	if level == 0 || level > 6 || (len(line) > level && line[level] != ' ') {
		return 0
	}
	return level
}

// adoptMarked records our existing review comment carrying ps's marker when
// state has no comment ID for it (a run stopped before saving).
func adoptMarked(ps *ProposalState, existing []Comment) {
	if ps.CommentID != 0 {
		return
	}
	if c, ok := findMarked(existing, CommentKindReview, proposalMarker(ps.ID)); ok {
		ps.CommentID, ps.CommentURL = c.ID, c.URL
	}
}

func findMarked(existing []Comment, kind CommentKind, marker string) (Comment, bool) {
	for _, c := range existing {
		first, _, _ := strings.Cut(c.Body, "\n")
		if c.Mine && c.Kind == kind && strings.TrimRight(first, "\r") == marker {
			return c, true
		}
	}
	return Comment{}, false
}

// findComment returns our comment id; a listed comment someone else wrote
// counts as missing so it is never edited.
func findComment(existing []Comment, kind CommentKind, id int64) (Comment, bool) {
	for _, c := range existing {
		if c.Mine && c.Kind == kind && c.ID == id {
			return c, true
		}
	}
	return Comment{}, false
}

// sameAnchor reports whether existing sits where rc would be created; a
// suggestion body is only safe to write onto the lines it was computed for.
func sameAnchor(existing Comment, rc ReviewComment) bool {
	return existing.Path == rc.Path && existing.StartLine == rc.StartLine && existing.Line == rc.Line
}

// Runners are the analysis runners a repo may use. A nil Runner means that
// runner is unavailable.
type Runners struct {
	Actions review.AsyncRunner
	Server  review.Runner
}

// Service decides and reports the pollux-agent check run for a pull request.
type Service struct {
	gh      GitHub
	store   Store
	runners Runners
	// collectBackoff is the wait before the first Collect retry; it doubles.
	collectBackoff time.Duration
}

// NewService returns a Service that reports check runs through gh, persists
// state through store, and selects among runners for analysis.
func NewService(gh GitHub, store Store, runners Runners) *Service {
	return &Service{gh: gh, store: store, runners: runners, collectBackoff: time.Second}
}

// WithCollectBackoff sets the wait before the first Collect retry (doubling
// each retry) and returns s.
func (s *Service) WithCollectBackoff(d time.Duration) *Service {
	s.collectBackoff = d
	return s
}

// HandlePullRequest selects an analysis runner for pr, runs it, and reports
// the result as the pollux-agent check run. A runner that finishes later leaves
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
	var changed []review.ChangedFile
	switch selectRunner(hasWorkflow, s.runners) {
	case runnerNone:
	case runnerActions:
		if err := s.startActions(ctx, state, pr); err != nil {
			return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
		}
		return nil
	case runnerServer:
		started, changed, err = s.start(ctx, s.runners.Server, pr)
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
	case review.Result:
		var run CheckRun
		next, run = conclude(next, resultOutcome(res))
		if _, err = s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run); err == nil && reconciles(res.Verdict) {
			next, err = s.postComments(ctx, next, pr, res.Verdict, changed)
		}
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

// startActions reports an in-progress check run, then dispatches the Actions
// run. The check run comes first so a failed or cancelled dispatch can still
// close it; once dispatched, the state writes outlive a cancelled ctx so the
// next job can find and close the check run.
func (s *Service) startActions(ctx context.Context, state PRState, pr PullRequest) error {
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, CheckRun{
		Name:    checkName,
		HeadSHA: pr.HeadSHA,
		Status:  StatusInProgress,
		Title:   "Analyzing docs impact",
		Summary: "Waiting for the pollux-agent workflow run to finish.",
	})
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()

	started, changed, err := s.start(ctx, s.runners.Actions, pr)
	next := OnPush(state, pr)
	if err == nil {
		switch res := started.(type) {
		case review.Pending:
			next = OnStarted(next, res, id)
		case review.Result:
			var run CheckRun
			next, run = conclude(next, resultOutcome(res))
			if err := s.gh.UpdateCheckRun(writeCtx, pr.InstallationID, pr.Owner, pr.Repo, id, run); err != nil {
				return fmt.Errorf("conclude check run %d: %w", id, err)
			}
			if reconciles(res.Verdict) {
				if next, err = s.postComments(writeCtx, next, pr, res.Verdict, changed); err != nil {
					return err
				}
			}
		default:
			err = fmt.Errorf("unknown review.Started %T", started)
		}
	}
	if err != nil {
		_, run := conclude(next, failedOutcome(err.Error()))
		if uerr := s.gh.UpdateCheckRun(writeCtx, pr.InstallationID, pr.Owner, pr.Repo, id, run); uerr != nil {
			return errors.Join(err, fmt.Errorf("conclude check run %d: %w", id, uerr))
		}
		return err
	}

	if err := s.store.SavePR(writeCtx, next); err != nil {
		return fmt.Errorf("save state: %w", err)
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

	_, run := conclude(state, outcome)
	var changed []review.ChangedFile
	reconcile := outcome.Result != nil && reconciles(outcome.Result.Verdict)
	pr := PullRequest{InstallationID: state.InstallationID, Owner: state.Owner, Repo: state.Repo, Number: state.Number, HeadSHA: state.HeadSHA}
	if reconcile {
		changed, err = s.gh.ListChangedFiles(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			return fmt.Errorf("handle run %d of %s/%s#%d: list changed files: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
		}
	}
	if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}
	if reconcile {
		state, err = s.postComments(ctx, state, pr, outcome.Result.Verdict, changed)
		if err != nil {
			return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
		}
	}
	next, _ := conclude(state, outcome)
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

	next, run := conclude(state, failedOutcome("no result from the pollux-agent workflow run before the deadline"))
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

	result, err := s.runners.Actions.Collect(ctx, completion)
	backoff := s.collectBackoff
	for attempt := 1; attempt < collectAttempts && err != nil && !failed && !errors.As(err, &invalid); attempt++ {
		select {
		case <-ctx.Done():
			return Outcome{}, fmt.Errorf("collect result: %w", ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		result, err = s.runners.Actions.Collect(ctx, completion)
	}
	switch {
	case errors.As(err, &invalid) && failed:
		return failedOutcome("workflow run " + rc.Conclusion + ": " + invalid.Cause.Error()), nil
	case errors.As(err, &invalid):
		return failedOutcome(invalid.Error()), nil
	case failed:
		return failedOutcome("workflow run " + rc.Conclusion), nil
	case err != nil:
		return failedOutcome(fmt.Sprintf("collect result: %v", err)), nil
	default:
		return resultOutcome(result), nil
	}
}

// postComments lists the PR's comments when there is anything to reconcile,
// performs Reconcile's writes, and records created comment IDs and URLs in the
// returned state, which is prev otherwise unchanged.
func (s *Service) postComments(ctx context.Context, prev PRState, pr PullRequest, verdict review.Verdict, changed []review.ChangedFile) (PRState, error) {
	proposals, _ := verdict.(review.Proposals)
	if len(prev.Proposals) == 0 && prev.SummaryCommentID == 0 && len(proposals) == 0 {
		return prev, nil
	}
	existing, err := s.gh.ListComments(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return PRState{}, fmt.Errorf("list comments: %w", err)
	}

	next, writes := Reconcile(prev, pr, verdict, changed, existing)
	// Saving before the first create means a run that stops after posting
	// leaves state behind, so the next run lists comments and adopts them by marker.
	if slices.ContainsFunc(writes, func(w CommentWrite) bool { return w.ID == 0 }) {
		if err := s.store.SavePR(ctx, next); err != nil {
			return PRState{}, fmt.Errorf("save state before creating comments: %w", err)
		}
	}
	for _, w := range writes {
		switch {
		case w.Summary:
			err = s.writeSummary(ctx, pr, &next, w)
		case w.ID == 0:
			err = s.createProposalComment(ctx, pr, &next, w)
		default:
			if err = s.gh.EditReviewComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, w.ID, w.Body); err != nil {
				err = fmt.Errorf("edit review comment for %s: %w", next.Proposals[w.Index].DocPath, err)
			}
		}
		if err != nil {
			return PRState{}, err
		}
	}
	return next, nil
}

func (s *Service) createProposalComment(ctx context.Context, pr PullRequest, next *PRState, w CommentWrite) error {
	ps := &next.Proposals[w.Index]
	c, err := s.gh.CreateReviewComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, w.Review)
	if err != nil {
		return fmt.Errorf("create review comment for %s: %w", ps.DocPath, err)
	}
	ps.CommentID, ps.CommentURL = c.ID, c.URL
	return nil
}

func (s *Service) writeSummary(ctx context.Context, pr PullRequest, next *PRState, w CommentWrite) error {
	body := renderSummary(*next)
	if w.ID != 0 {
		if err := s.gh.EditIssueComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, w.ID, body); err != nil {
			return fmt.Errorf("edit summary comment: %w", err)
		}
		return nil
	}
	c, err := s.gh.CreateIssueComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, body)
	if err != nil {
		return fmt.Errorf("create summary comment: %w", err)
	}
	next.SummaryCommentID = c.ID
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

func (s *Service) start(ctx context.Context, runner review.Runner, pr PullRequest) (review.Started, []review.ChangedFile, error) {
	changed, err := s.gh.ListChangedFiles(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return nil, nil, fmt.Errorf("list changed files: %w", err)
	}

	started, err := runner.Start(ctx, review.Request{
		InstallationID: pr.InstallationID,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
		Number:         pr.Number,
		BaseSHA:        pr.BaseSHA,
		HeadSHA:        pr.HeadSHA,
		ChangedFiles:   changed,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start analysis: %w", err)
	}
	return started, changed, nil
}

// reconciles reports whether a verdict concludes the analysis, so that its
// proposals belong in comments; an empty proposal list is a failed analysis.
func reconciles(v review.Verdict) bool {
	switch v := v.(type) {
	case review.NoImpact:
		return true
	case review.Proposals:
		return len(v) > 0
	}
	return false
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
