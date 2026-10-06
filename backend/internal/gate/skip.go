package gate

import (
	"context"
	"fmt"
	"strings"
)

// skipActive reports whether a skip covers the head state reports on.
func skipActive(s PRState) bool {
	return s.Skip != nil && (s.Skip.Scope == SkipPR || s.Skip.HeadSHA == s.HeadSHA)
}

func (sc SkipScope) noun() string {
	if sc == SkipPR {
		return "PR"
	}
	return "commit"
}

// skipRun is the success check run for the active skip of s: pure, no I/O.
func skipRun(s PRState) CheckRun {
	sk := s.Skip
	return CheckRun{
		Name:       CheckName,
		HeadSHA:    s.HeadSHA,
		Status:     StatusCompleted,
		Conclusion: ConclusionSuccess,
		Title:      fmt.Sprintf("Skipped by @%s", sk.User),
		Summary:    truncate(fmt.Sprintf("@%s skipped the docs check for this %s: %s", sk.User, sk.Scope.noun(), sk.Reason), maxSummaryBytes),
	}
}

// OnSkip is the state transition for a skip made at the current head: pure, no
// I/O. The skip wins over any awaited run, so that run's result is ignored.
func OnSkip(s PRState, sk Skip) (PRState, CheckRun) {
	s.Skip = &Skip{User: sk.User, Scope: sk.Scope, Reason: sk.Reason, HeadSHA: s.HeadSHA}
	s.PendingSkip = nil
	s.Run = nil
	return s, skipRun(s)
}

// maxReasonRunes caps a skip reason, which is echoed in the summary and the check run.
const maxReasonRunes = 500

// cleanReason makes a user-written skip reason safe to echo: one line, capped,
// with @mentions and checkbox markup broken so it can notify nobody and cannot
// pass for a box in the summary.
func cleanReason(reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	reason = strings.ReplaceAll(reason, "@", "@\u200b")
	reason = strings.ReplaceAll(reason, "- [", "-\u200b [")
	if r := []rune(reason); len(r) > maxReasonRunes {
		reason = strings.TrimSpace(string(r[:maxReasonRunes-1])) + "…"
	}
	return reason
}

// handleSkip acts on a skip intent from a sender already allowed to write.
func (s *Service) handleSkip(ctx context.Context, state PRState, in Intent, sender string) (Reaction, error) {
	ctx, cancel := writeContext(ctx)
	defer cancel()
	var err error
	switch in.Kind {
	case IntentSkipAsk:
		ask := SkipAsk{User: sender, Scope: in.Scope}
		if (state.PendingSkip != nil && *state.PendingSkip == ask) || (skipActive(state) && state.Skip.Scope == ask.Scope) {
			err = s.redrawSummary(ctx, state)
		} else {
			err = s.askSkipReason(ctx, state, ask)
		}
	case IntentSkip:
		err = s.skip(ctx, state, Skip{User: sender, Scope: in.Scope, Reason: cleanReason(in.Reason)})
	case IntentSkipReason:
		if state.PendingSkip != nil {
			err = s.skip(ctx, state, Skip{User: state.PendingSkip.User, Scope: state.PendingSkip.Scope, Reason: cleanReason(in.Reason)})
		}
	case IntentNone, IntentApply, IntentApplyAll, IntentRerun:
	}
	if err != nil {
		return "", err
	}
	return ReactionDone, nil
}

func (s *Service) askSkipReason(ctx context.Context, state PRState, ask SkipAsk) error {
	body := fmt.Sprintf("@%s, reply with the reason for skipping this %s; your next comment on this PR becomes the reason.", ask.User, ask.Scope.noun())
	if _, err := s.gh.CreateIssueComment(ctx, state.InstallationID, state.Owner, state.Repo, state.Number, body); err != nil {
		return fmt.Errorf("ask for skip reason: %w", err)
	}
	state.PendingSkip = &ask
	return s.saveAndRedraw(ctx, state)
}

// skip concludes the check run as skipped. A skip already in state only redraws
// the summary, which finishes a run that failed after saving.
func (s *Service) skip(ctx context.Context, state PRState, sk Skip) error {
	if state.Skip != nil && *state.Skip == (Skip{User: sk.User, Scope: sk.Scope, Reason: sk.Reason, HeadSHA: state.HeadSHA}) {
		return s.redrawSummary(ctx, state)
	}
	state, run := OnSkip(state, sk)
	if state.CheckRunID == 0 {
		id, err := s.gh.CreateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, run)
		if err != nil {
			return fmt.Errorf("create check run: %w", err)
		}
		state.CheckRunID = id
	} else if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("update check run %d: %w", state.CheckRunID, err)
	}
	return s.saveAndRedraw(ctx, state)
}

// saveAndRedraw saves state before redrawing the summary, so a failed redraw
// never leaves a concluded check run with unsaved state; a retry redraws again.
func (s *Service) saveAndRedraw(ctx context.Context, state PRState) error {
	if err := s.store.SavePR(ctx, state); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return s.redrawSummary(ctx, state)
}
