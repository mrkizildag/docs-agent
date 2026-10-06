package gate

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// OnPush is the state transition for a new head commit: pure, no I/O. It
// drops any awaited run, so that run's result is ignored; the proposals, the
// summary comment and PR-scope skips carry over. A commit-scope skip carries
// over only for the head it was made at, and a pending skip ask, of either
// scope, only while the head stays the same. The pending apply is dropped.
func OnPush(prev PRState, pr PullRequest) PRState {
	next := PRState{
		InstallationID:   pr.InstallationID,
		Owner:            pr.Owner,
		Repo:             pr.Repo,
		Number:           pr.Number,
		HeadSHA:          pr.HeadSHA,
		HeadRef:          pr.HeadRef,
		Fork:             pr.Fork,
		ProposalsSHA:     prev.ProposalsSHA,
		SummaryCommentID: prev.SummaryCommentID,
		Proposals:        slices.Clone(prev.Proposals),
	}
	if prev.Skip != nil {
		kept := *prev.Skip
		next.Skip = &kept
		if !skipActive(next) {
			next.Skip = nil
		}
	}
	if ask := prev.PendingSkip; ask != nil && pendingSkipCancelled(prev, pr) == nil {
		kept := *ask
		next.PendingSkip = &kept
	}
	return next
}

// pendingSkipCancelled returns the pending skip ask that a push to pr cancels,
// or nil: any ask is cancelled by a new head, kept on the same one.
func pendingSkipCancelled(prev PRState, pr PullRequest) *SkipAsk {
	if prev.PendingSkip == nil || prev.HeadSHA == pr.HeadSHA {
		return nil
	}
	return prev.PendingSkip
}

// superseded returns the neutral check run that closes the check run of an
// awaited run when a new analysis of pr replaces it, on any head; ok is false
// when there is nothing to close. Pure, no I/O.
func superseded(state PRState, pr PullRequest) (run CheckRun, ok bool) {
	if state.Run == nil || state.CheckRunID == 0 {
		return CheckRun{}, false
	}
	by := "a re-run"
	if pr.HeadSHA != state.HeadSHA {
		by = shortSHA(pr.HeadSHA)
	}
	run = CheckRun{Name: CheckName, HeadSHA: state.HeadSHA, Status: StatusCompleted}
	return neutral(run, "Superseded", "Superseded by "+by), true
}

// overdue reports whether state awaits a run whose deadline has passed at now.
func overdue(state PRState, now time.Time) bool {
	return state.Run != nil && now.After(state.Run.Deadline)
}

// onStarted is the state transition for an analysis that runs elsewhere: pure, no I/O.
func onStarted(state PRState, pending review.Pending, checkRunID int64) PRState {
	state.CheckRunID = checkRunID
	state.Run = &AwaitingRun{RunID: pending.RunID, Nonce: pending.Nonce, Deadline: pending.Deadline}
	return state
}

// MatchesRun reports whether rc is the completion of the run state awaits.
func MatchesRun(state PRState, rc RunCompleted) bool {
	return state.Run != nil && rc.RunID != 0 && state.Run.RunID == rc.RunID
}

// conclude is the state transition for an analysis that ended: pure, no I/O.
// It clears the awaited run and returns the completed check run to report;
// runner-supplied text is capped to what GitHub accepts. An active skip
// replaces the outcome with its success.
func conclude(state PRState, outcome Outcome) (PRState, CheckRun) {
	state.FailureCause = ""
	if outcome.Failed != nil {
		state.FailureCause = review.Truncate(outcome.Failed.Cause, maxCauseBytes, truncatedMark)
	}
	if skipActive(state) {
		state.Run = nil
		return state, skipRun(state)
	}
	state, run := concludeUncapped(state, outcome)
	run.Summary = review.Truncate(run.Summary, maxSummaryBytes, truncatedMark)
	return state, run
}

func concludeUncapped(state PRState, outcome Outcome) (PRState, CheckRun) {
	state.Run = nil
	run := CheckRun{Name: CheckName, HeadSHA: state.HeadSHA, Status: StatusCompleted}

	switch {
	case outcome.Result != nil:
		switch v := outcome.Result.Verdict.(type) {
		case review.NoImpact:
			run.Conclusion, run.Title, run.Summary = ConclusionSuccess, "No doc impact", v.Reason
			return state, run
		case review.Proposals:
			pending := unapplied(state, v)
			if len(pending) == 0 {
				run.Conclusion, run.Title, run.Summary = ConclusionSuccess, "Docs up to date", "Every proposed doc change is already applied."
				return state, run
			}
			run.Conclusion, run.Title, run.Summary = ConclusionActionRequired, "Docs need updating", proposalsSummary(pending)
			return state, run
		default:
			return state, neutral(run, "Analysis failed", unusableCause(v))
		}
	case outcome.Failed != nil:
		title := cmp.Or(outcome.Failed.Title, "Analysis failed")
		return state, neutral(run, title, review.Truncate(outcome.Failed.Cause, maxCauseBytes, truncatedMark))
	default:
		return state, neutral(run, "Analysis failed", "analysis ended without an outcome")
	}
}

// unapplied is v without the proposals state already holds as applied with the
// same content; Reconcile neither reopens nor reposts those.
func unapplied(state PRState, v review.Proposals) review.Proposals {
	var out review.Proposals
	for _, p := range v {
		id := ProposalID(p.DocPath, p.Section)
		if !slices.ContainsFunc(state.Proposals, func(ps ProposalState) bool { return ps.ID == id && appliedAs(ps, p) }) {
			out = append(out, p)
		}
	}
	return out
}

func appliedAs(ps ProposalState, p review.Proposal) bool {
	return ps.State == ProposalApplied && ps.Content == withHeading(p).Content
}

func neutral(run CheckRun, title, summary string) CheckRun {
	run.Conclusion, run.Title, run.Summary = ConclusionNeutral, title, summary
	return run
}

func proposalsSummary(proposals review.Proposals) string {
	lines := make([]string, len(proposals))
	for i, p := range proposals {
		lines[i] = fmt.Sprintf("- %s: %s", p.DocPath, p.Reason)
	}
	return strings.Join(lines, "\n")
}
