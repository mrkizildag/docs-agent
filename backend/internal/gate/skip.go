package gate

import (
	"context"
	"fmt"
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

// handleSkip acts on a skip intent from a sender already allowed to write.
func (s *Service) handleSkip(ctx context.Context, state PRState, in Intent, sender, op string) (Reaction, error) {
	var err error
	switch in.Kind {
	case IntentSkipAsk:
		ask := SkipAsk{User: sender, Scope: in.Scope}
		if (state.PendingSkip == nil || *state.PendingSkip != ask) && (!skipActive(state) || state.Skip.Scope != ask.Scope) {
			err = s.askSkipReason(ctx, state, ask, op)
		}
	case IntentSkip:
		err = s.skip(ctx, state, Skip{User: sender, Scope: in.Scope, Reason: in.Reason}, op)
	case IntentSkipReason:
		if state.PendingSkip != nil {
			err = s.skip(ctx, state, Skip{User: state.PendingSkip.User, Scope: state.PendingSkip.Scope, Reason: in.Reason}, op)
		}
	case IntentNone, IntentApply, IntentApplyAll, IntentRerun:
	}
	if err != nil {
		return "", err
	}
	return ReactionDone, nil
}

func (s *Service) askSkipReason(ctx context.Context, state PRState, ask SkipAsk, op string) error {
	body := fmt.Sprintf("@%s, reply with the reason for skipping this %s; your next comment on this PR becomes the reason.", ask.User, ask.Scope.noun())
	if _, err := s.gh.CreateIssueComment(ctx, state.InstallationID, state.Owner, state.Repo, state.Number, body); err != nil {
		return fmt.Errorf("%s: ask for skip reason: %w", op, err)
	}
	state.PendingSkip = &ask
	return s.redrawAndSave(ctx, state, op)
}

func (s *Service) skip(ctx context.Context, state PRState, sk Skip, op string) error {
	if state.Skip != nil && *state.Skip == (Skip{User: sk.User, Scope: sk.Scope, Reason: sk.Reason, HeadSHA: state.HeadSHA}) {
		return nil
	}
	state, run := OnSkip(state, sk)
	if state.CheckRunID == 0 {
		id, err := s.gh.CreateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, run)
		if err != nil {
			return fmt.Errorf("%s: create check run: %w", op, err)
		}
		state.CheckRunID = id
	} else if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("%s: update check run %d: %w", op, state.CheckRunID, err)
	}
	return s.redrawAndSave(ctx, state, op)
}

// redrawAndSave redraws the summary before saving, so a retry after a failed
// redraw still sees the change as new.
func (s *Service) redrawAndSave(ctx context.Context, state PRState, op string) error {
	if state.SummaryCommentID != 0 {
		if err := s.gh.EditIssueComment(ctx, state.InstallationID, state.Owner, state.Repo, state.SummaryCommentID, renderSummary(state)); err != nil {
			return fmt.Errorf("%s: edit summary comment: %w", op, err)
		}
	}
	if err := s.store.SavePR(ctx, state); err != nil {
		return fmt.Errorf("%s: save state: %w", op, err)
	}
	return nil
}
