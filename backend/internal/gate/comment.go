package gate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Reaction is a GitHub reaction's content.
type Reaction string

const (
	ReactionSeen    Reaction = "eyes"     // the bot picked the action up
	ReactionDone    Reaction = "rocket"   // the action completed
	ReactionRefused Reaction = "confused" // the action was refused; a reply says why
)

// CommentEvent is an edited or new comment on a pull request, decoded by the
// transport. Ticked is the checkbox line that went from unchecked to checked in
// this edit, else "".
type CommentEvent struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	Sender         string
	CommentID      int64
	Kind           CommentKind
	Ticked         string
	Body           string
}

// IntentKind is what a comment asks the gate to do.
type IntentKind int

const (
	IntentNone       IntentKind = iota
	IntentApply                 // ProposalID
	IntentApplyAll              // summary tick or "/pollux-agent apply"
	IntentSkipAsk               // Scope; a skip tick, or a skip command without a reason
	IntentSkip                  // Scope and Reason; a skip command with a reason
	IntentSkipReason            // Reason; the pending asker's next comment
	IntentRerun                 // summary tick of the Re-run box
)

// Intent is the action a comment asks for.
type Intent struct {
	Kind       IntentKind
	ProposalID string
	Scope      SkipScope
	Reason     string
}

const (
	applyLabel      = "Apply this change"
	applyAllLabel   = "Apply all"
	skipCommitLabel = "Skip this commit"
	skipPRLabel     = "Skip this PR"
	rerunLabel      = "Re-run analysis"

	commandPrefix = "/pollux-agent"
	applyCommand  = "apply"
	skipCommand   = "skip"
	skipPRCommand = "skip-pr"
)

// IsRerunTick reports whether ticked, the line a summary edit flipped, is the
// Re-run box; the transport queues those as push-cancellable analysis jobs.
func IsRerunTick(ticked string) bool {
	return strings.TrimSpace(ticked) == checkbox(true, rerunLabel)
}

// ParseIntent decodes what ev asks for given the PR's state: pure, no I/O.
func ParseIntent(ev CommentEvent, s PRState) Intent {
	if ticked := strings.TrimSpace(ev.Ticked); ticked != "" {
		return parseTick(ev, ticked, s)
	}
	body := strings.TrimSpace(ev.Body)
	if body == "" {
		return Intent{}
	}
	firstLine, _, _ := strings.Cut(body, "\n")
	if strings.HasPrefix(strings.TrimSpace(firstLine), commandPrefix+" ") {
		if ev.Kind != CommentKindIssue {
			return Intent{}
		}
		return parseCommand(strings.TrimSpace(body[len(commandPrefix):]))
	}
	if s.PendingSkip != nil && strings.EqualFold(ev.Sender, s.PendingSkip.User) {
		return Intent{Kind: IntentSkipReason, Reason: body}
	}
	return Intent{}
}

func parseTick(ev CommentEvent, ticked string, s PRState) Intent {
	switch ev.Kind {
	case CommentKindReview:
		if ticked != checkbox(true, applyLabel) {
			return Intent{}
		}
		for _, p := range s.Proposals {
			if p.CommentID != 0 && p.CommentID == ev.CommentID {
				return Intent{Kind: IntentApply, ProposalID: p.ID}
			}
		}
	case CommentKindIssue:
		if s.SummaryCommentID == 0 || ev.CommentID != s.SummaryCommentID {
			return Intent{}
		}
		switch ticked {
		case checkbox(true, applyAllLabel):
			return Intent{Kind: IntentApplyAll}
		case checkbox(true, skipCommitLabel):
			return Intent{Kind: IntentSkipAsk, Scope: SkipCommit}
		case checkbox(true, skipPRLabel):
			return Intent{Kind: IntentSkipAsk, Scope: SkipPR}
		case checkbox(true, rerunLabel):
			return Intent{Kind: IntentRerun}
		}
	default:
	}
	return Intent{}
}

// parseCommand decodes the text after the command prefix: a command word and,
// for the skip commands, the reason that follows it.
func parseCommand(rest string) Intent {
	word, reason := rest, ""
	if i := strings.IndexFunc(rest, unicode.IsSpace); i >= 0 {
		word, reason = rest[:i], strings.TrimSpace(rest[i:])
	}
	switch word {
	case applyCommand:
		return Intent{Kind: IntentApplyAll}
	case skipCommand:
		return skipIntent(SkipCommit, reason)
	case skipPRCommand:
		return skipIntent(SkipPR, reason)
	default:
		return Intent{}
	}
}

func skipIntent(scope SkipScope, reason string) Intent {
	if reason == "" {
		return Intent{Kind: IntentSkipAsk, Scope: scope}
	}
	return Intent{Kind: IntentSkip, Scope: scope, Reason: reason}
}

// writeContext bounds writes that must survive a cancelled job; it starts when
// called, so a slow step before it cannot use up the budget.
func writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}

// HandleComment acts on a comment: it applies proposals, hands a skip to
// handleSkip, or re-runs the analysis, when the sender may write. Comments that
// ask for nothing cost no GitHub call; a sender without write access gets a
// reply and a refusal reaction. A redelivery of an applied proposal only posts
// what is still missing.
func (s *Service) HandleComment(ctx context.Context, ev CommentEvent) error {
	op := fmt.Sprintf("handle comment %d of %s/%s#%d", ev.CommentID, ev.Owner, ev.Repo, ev.Number)
	state, err := s.store.LoadPR(ctx, ev.Owner, ev.Repo, ev.Number)
	if err != nil {
		return fmt.Errorf("%s: load state: %w", op, err)
	}
	intent := ParseIntent(ev, state)
	if intent.Kind == IntentNone {
		return nil
	}
	state.InstallationID = cmp.Or(state.InstallationID, ev.InstallationID)

	canWrite, err := s.comments.Permission(ctx, state.InstallationID, state.Owner, state.Repo, ev.Sender)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !canWrite {
		final, err := s.say(ctx, state, ev, "you need write access to this repository to do that.", op)
		if err != nil {
			return err
		}
		return s.settle(ctx, state, ev, 0, final, op)
	}

	seen, err := s.comments.React(ctx, state.InstallationID, state.Owner, state.Repo, ev.Kind, ev.CommentID, ReactionSeen)
	if err != nil {
		return fmt.Errorf("%s: react %s: %w", op, ReactionSeen, err)
	}
	final, err := s.act(ctx, state, ev, intent, op)
	if err != nil {
		return err
	}
	return s.settle(ctx, state, ev, seen, final, op)
}

// act runs intent for ev, whose sender may write, and reports how it ended:
// ReactionDone when the action completed, ReactionRefused when a reply told the
// sender why not, "" when a newer job superseded it and left the work to that job.
func (s *Service) act(ctx context.Context, state PRState, ev CommentEvent, in Intent, op string) (Reaction, error) {
	if state.HeadSHA == "" {
		return s.say(ctx, state, ev, "pollux-agent hasn't analyzed this PR yet; push or reopen it, then try again.", op)
	}
	switch in.Kind {
	case IntentApply, IntentApplyAll:
		return s.handleApply(ctx, state, ev, in, op)
	case IntentNone:
		return ReactionDone, nil
	case IntentSkipAsk, IntentSkip, IntentSkipReason:
		return s.handleSkip(ctx, state, in, ev.Sender, op)
	case IntentRerun:
		rerun := RerunRequest{InstallationID: state.InstallationID, PRRef: PRRef{Owner: ev.Owner, Repo: ev.Repo, Number: ev.Number}, SummaryCommentID: ev.CommentID}
		var reported *reportedFailure
		if err := s.HandleRerun(ctx, rerun); err != nil && !errors.As(err, &reported) {
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
				return "", nil
			}
			return "", fmt.Errorf("%s: %w", op, err)
		}
		return ReactionDone, nil
	default:
		return "", fmt.Errorf("%s: unknown intent %d", op, in.Kind)
	}
}

// settle swaps the 👀 on ev's comment (seen, 0 when none was added) for final,
// after the action's own writes; an empty final only removes the 👀.
func (s *Service) settle(ctx context.Context, state PRState, ev CommentEvent, seen int64, final Reaction, op string) error {
	ctx, cancel := writeContext(ctx)
	defer cancel()
	if seen != 0 {
		if err := s.comments.Unreact(ctx, state.InstallationID, state.Owner, state.Repo, ev.Kind, ev.CommentID, seen); err != nil {
			return fmt.Errorf("%s: remove %s reaction: %w", op, ReactionSeen, err)
		}
	}
	if final == "" {
		return nil
	}
	if _, err := s.comments.React(ctx, state.InstallationID, state.Owner, state.Repo, ev.Kind, ev.CommentID, final); err != nil {
		return fmt.Errorf("%s: react %s: %w", op, final, err)
	}
	return nil
}

// say answers ev: in the review thread for a review comment, else as an issue
// comment addressed to the sender. A refused tick is then unticked so it can be
// ticked again. It returns ReactionRefused for the caller to settle.
func (s *Service) say(ctx context.Context, state PRState, ev CommentEvent, text, op string) (Reaction, error) {
	ctx, cancel := writeContext(ctx)
	defer cancel()
	if ev.Kind == CommentKindReview {
		if _, err := s.comments.ReplyToReviewComment(ctx, state.InstallationID, state.Owner, state.Repo, state.Number, ev.CommentID, "@"+ev.Sender+" "+text); err != nil {
			return "", fmt.Errorf("%s: reply: %w", op, err)
		}
	} else if _, err := s.gh.CreateIssueComment(ctx, state.InstallationID, state.Owner, state.Repo, state.Number, "@"+ev.Sender+" "+text); err != nil {
		return "", fmt.Errorf("%s: reply: %w", op, err)
	}
	if strings.TrimSpace(ev.Ticked) != "" {
		if err := s.untick(ctx, state, ev, op); err != nil {
			return "", err
		}
	}
	return ReactionRefused, nil
}

// untick restores the box ev ticked: the summary is redrawn from state, a
// review comment loses the tick on its Apply box.
func (s *Service) untick(ctx context.Context, state PRState, ev CommentEvent, op string) error {
	if ev.Kind == CommentKindIssue {
		if state.SummaryCommentID == 0 || ev.CommentID != state.SummaryCommentID {
			return nil
		}
		return s.redrawSummary(ctx, state, op)
	}
	existing, err := s.gh.ListComments(ctx, state.InstallationID, state.Owner, state.Repo, state.Number)
	if err != nil {
		return fmt.Errorf("%s: list comments: %w", op, err)
	}
	c, ok := findComment(existing, CommentKindReview, ev.CommentID)
	if !ok {
		return nil
	}
	body, ok := setCheckbox(c.Body, applyLabel, false)
	if !ok {
		return nil
	}
	if err := s.gh.EditReviewComment(ctx, state.InstallationID, state.Owner, state.Repo, ev.CommentID, body); err != nil {
		return fmt.Errorf("%s: untick comment %d: %w", op, ev.CommentID, err)
	}
	return nil
}

// redrawSummary rewrites the summary comment from state; it does nothing when
// there is no summary comment.
func (s *Service) redrawSummary(ctx context.Context, state PRState, op string) error {
	if state.SummaryCommentID == 0 {
		return nil
	}
	if err := s.gh.EditIssueComment(ctx, state.InstallationID, state.Owner, state.Repo, state.SummaryCommentID, renderSummary(state)); err != nil {
		return fmt.Errorf("%s: edit summary comment: %w", op, err)
	}
	return nil
}
