package gate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// ErrBranchMoved is returned by CommentGitHub.CommitFiles when the branch no
// longer points at the parent commit.
var ErrBranchMoved = errors.New("gate: branch moved")

// FileChange is a whole-file replacement in a commit.
type FileChange struct {
	Path    string
	Content string
}

// Commit is a commit as Apply's crash recovery needs it.
type Commit struct {
	SHA     string
	Message string
	Parents []string
}

// Reaction is a GitHub reaction's content.
type Reaction string

const (
	ReactionSeen    Reaction = "eyes"     // the bot picked the action up
	ReactionDone    Reaction = "rocket"   // the action completed
	ReactionRefused Reaction = "confused" // the action was refused; a reply says why
)

// CommentGitHub is what acting on a comment needs from GitHub. It is wired with
// Service.WithComments.
type CommentGitHub interface {
	// Permission reports whether user may write to the repository.
	Permission(ctx context.Context, installationID int64, owner, repo, user string) (canWrite bool, err error)
	// FileAtRef returns the file at ref; ok is false when it does not exist there.
	FileAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (content []byte, ok bool, err error)
	// CommitFiles commits files on top of parentSHA and moves branch to the new
	// commit without forcing. It returns ErrBranchMoved when branch is no longer at parentSHA.
	CommitFiles(ctx context.Context, installationID int64, owner, repo, branch, parentSHA string, files []FileChange, message string) (sha string, err error)
	// BranchCommit returns the commit branch points at.
	BranchCommit(ctx context.Context, installationID int64, owner, repo, branch string) (Commit, error)
	// React adds reaction to comment id of kind and returns the reaction's ID;
	// adding one that already exists returns the existing ID.
	React(ctx context.Context, installationID int64, owner, repo string, kind CommentKind, id int64, reaction Reaction) (int64, error)
	// Unreact removes reaction reactionID from comment id of kind.
	Unreact(ctx context.Context, installationID int64, owner, repo string, kind CommentKind, id, reactionID int64) error
	// ReplyToReviewComment posts a reply in the thread of review comment inReplyTo.
	ReplyToReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, inReplyTo int64, body string) (Comment, error)
}

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

	commandPrefix = "/pollux-agent"
	applyCommand  = "apply"
	skipCommand   = "skip"
	skipPRCommand = "skip-pr"

	indexPath    = "docs/README.md"
	indexHeading = "## Index"
)

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
		if ticked != "- [x] "+applyLabel {
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
		case "- [x] " + applyAllLabel:
			return Intent{Kind: IntentApplyAll}
		case "- [x] " + skipCommitLabel:
			return Intent{Kind: IntentSkipAsk, Scope: SkipCommit}
		case "- [x] " + skipPRLabel:
			return Intent{Kind: IntentSkipAsk, Scope: SkipPR}
		case "- [x] " + rerunLabel:
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

// OnApply is the state transition for a commit that applied the open proposals
// ids at sha: pure, no I/O.
func OnApply(s PRState, ids []string, sha string) PRState {
	s.Proposals = slices.Clone(s.Proposals)
	for i := range s.Proposals {
		if p := &s.Proposals[i]; p.State == ProposalOpen && slices.Contains(ids, p.ID) {
			p.State, p.AppliedSHA = ProposalApplied, sha
		}
	}
	return s
}

// splice replaces original, which must occur exactly once in file, with content.
func splice(file, original, content string) (string, error) {
	if original == "" {
		return "", errors.New("the proposal has no original section text")
	}
	if n := strings.Count(file, original); n != 1 {
		return "", fmt.Errorf("section text occurs %d times in the doc, want exactly 1", n)
	}
	return strings.Replace(file, original, content, 1), nil
}

// addIndexEntry inserts entry after the last list item of the "## Index"
// section of readme, or appends it when there is none.
func addIndexEntry(readme, entry string) string {
	lines := strings.Split(readme, "\n")
	inIndex, last := false, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "## "):
			inIndex = strings.TrimSpace(l) == indexHeading
		case inIndex && strings.HasPrefix(l, "- "):
			last = i
		}
	}
	if last < 0 {
		if readme != "" && !strings.HasSuffix(readme, "\n") {
			readme += "\n"
		}
		return readme + entry + "\n"
	}
	return strings.Join(slices.Concat(lines[:last+1], []string{entry}, lines[last+1:]), "\n")
}

// WithComments sets the GitHub access HandleComment needs and returns s.
func (s *Service) WithComments(c CommentGitHub) *Service {
	s.comments = c
	return s
}

// HandleComment acts on a comment: it applies proposals, hands a skip to
// handleSkip, or re-runs the analysis, when the sender may write. Comments that ask for nothing cost no
// GitHub call. A redelivery of an applied proposal only posts the replies still
// missing.
func (s *Service) HandleComment(ctx context.Context, ev CommentEvent) error {
	op := fmt.Sprintf("handle comment %d of %s/%s#%d", ev.CommentID, ev.Owner, ev.Repo, ev.Number)
	if s.comments == nil {
		return fmt.Errorf("%s: no comment GitHub configured", op)
	}
	state, err := s.store.LoadPR(ctx, ev.Owner, ev.Repo, ev.Number)
	if err != nil {
		return fmt.Errorf("%s: load state: %w", op, err)
	}
	intent := ParseIntent(ev, state)
	if intent.Kind == IntentNone {
		return nil
	}

	seen, err := s.comments.React(ctx, state.InstallationID, state.Owner, state.Repo, ev.Kind, ev.CommentID, ReactionSeen)
	if err != nil {
		return fmt.Errorf("%s: react %s: %w", op, ReactionSeen, err)
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	final, err := s.act(ctx, writeCtx, state, ev, intent, op)
	if err != nil {
		return err
	}
	return s.settle(writeCtx, state, ev, seen, final, op)
}

// act runs intent for ev and reports how it ended: ReactionDone when the
// action completed, ReactionRefused when a reply told the sender why not.
func (s *Service) act(ctx, writeCtx context.Context, state PRState, ev CommentEvent, in Intent, op string) (Reaction, error) {
	canWrite, err := s.comments.Permission(ctx, state.InstallationID, state.Owner, state.Repo, ev.Sender)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if !canWrite {
		return s.say(writeCtx, state, ev, "you need write access to this repository to do that.", op)
	}

	switch in.Kind {
	case IntentApply, IntentApplyAll:
		return s.handleApply(ctx, writeCtx, state, ev, in, op)
	case IntentNone:
		return ReactionDone, nil
	case IntentSkipAsk, IntentSkip, IntentSkipReason:
		return s.handleSkip(writeCtx, state, in, ev.Sender, op)
	case IntentRerun:
		rerun := RerunRequest{InstallationID: ev.InstallationID, PRRef: PRRef{Owner: ev.Owner, Repo: ev.Repo, Number: ev.Number}, SummaryCommentID: ev.CommentID}
		if err := s.HandleRerun(ctx, rerun); err != nil {
			return "", fmt.Errorf("%s: %w", op, err)
		}
		return ReactionDone, nil
	default:
		return "", fmt.Errorf("%s: unknown intent %d", op, in.Kind)
	}
}

// settle swaps the 👀 on ev's comment for final, after the action's own writes.
func (s *Service) settle(ctx context.Context, state PRState, ev CommentEvent, seen int64, final Reaction, op string) error {
	if err := s.comments.Unreact(ctx, state.InstallationID, state.Owner, state.Repo, ev.Kind, ev.CommentID, seen); err != nil {
		return fmt.Errorf("%s: remove %s reaction: %w", op, ReactionSeen, err)
	}
	if _, err := s.comments.React(ctx, state.InstallationID, state.Owner, state.Repo, ev.Kind, ev.CommentID, final); err != nil {
		return fmt.Errorf("%s: react %s: %w", op, final, err)
	}
	return nil
}

// handleApply commits the open proposals the intent targets as one commit.
// ctx bounds the reads and the commit; writeCtx the state and comment writes
// that must survive a cancelled job.
func (s *Service) handleApply(ctx, writeCtx context.Context, state PRState, ev CommentEvent, in Intent, op string) (Reaction, error) {
	if state.Fork {
		return s.say(writeCtx, state, ev, "Apply is not available on a fork the bot cannot push to.", op)
	}
	var targets []int
	for i, p := range state.Proposals {
		if p.State == ProposalOpen && (in.Kind == IntentApplyAll || p.ID == in.ProposalID) {
			targets = append(targets, i)
		}
	}
	if len(targets) == 0 {
		posted, err := s.replyApplied(writeCtx, state, in.ProposalID, op)
		if err != nil {
			return "", err
		}
		if posted == 0 && in.Kind == IntentApplyAll {
			if _, err := s.say(writeCtx, state, ev, "✅ Nothing left to apply.", op); err != nil {
				return "", err
			}
		}
		return ReactionDone, nil
	}
	if state.HeadSHA != state.ProposalsSHA {
		return s.say(writeCtx, state, ev, "a newer push is being re-analyzed; nothing was committed. Apply again once the proposals refresh.", op)
	}

	files, err := s.applyFiles(ctx, state, targets)
	if errors.Is(err, errDocMismatch) {
		return s.say(writeCtx, state, ev, "a proposal no longer matches the doc ("+err.Error()+"); nothing was committed.", op)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	message := applyMessage(state, targets)

	sha, err := s.comments.CommitFiles(ctx, state.InstallationID, state.Owner, state.Repo, state.HeadRef, state.HeadSHA, files, message)
	if errors.Is(err, ErrBranchMoved) {
		var adopted bool
		if sha, adopted, err = s.adoptCommit(ctx, state, message); err != nil {
			return "", fmt.Errorf("%s: %w", op, err)
		}
		if !adopted {
			return s.say(writeCtx, state, ev, "the branch moved while applying; nothing was committed. A re-analysis follows.", op)
		}
	} else if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}

	ids := make([]string, len(targets))
	for n, i := range targets {
		ids[n] = state.Proposals[i].ID
	}
	state = OnApply(state, ids, sha)
	if err := s.store.SavePR(writeCtx, state); err != nil {
		return "", fmt.Errorf("%s: save state after commit %s: %w", op, shortSHA(sha), err)
	}
	if in.Kind == IntentApplyAll {
		if err := s.tickApplied(writeCtx, state, ids, op); err != nil {
			return "", err
		}
	}
	if state.SummaryCommentID != 0 {
		if err := s.gh.EditIssueComment(writeCtx, state.InstallationID, state.Owner, state.Repo, state.SummaryCommentID, renderSummary(state)); err != nil {
			return "", fmt.Errorf("%s: edit summary comment: %w", op, err)
		}
	}
	if _, err := s.replyApplied(writeCtx, state, "", op); err != nil {
		return "", err
	}
	return ReactionDone, nil
}

// errDocMismatch marks a proposal that cannot be applied to the doc at head.
var errDocMismatch = errors.New("doc mismatch")

// applyFiles builds the file changes for the proposals at targets, reading
// each doc once at the head. It returns errDocMismatch for user-facing failures.
func (s *Service) applyFiles(ctx context.Context, state PRState, targets []int) ([]FileChange, error) {
	docs := map[string]string{}
	var changed []string
	load := func(path string) (string, bool, error) {
		if c, ok := docs[path]; ok {
			return c, true, nil
		}
		b, ok, err := s.comments.FileAtRef(ctx, state.InstallationID, state.Owner, state.Repo, path, state.HeadSHA)
		if err != nil {
			return "", false, fmt.Errorf("read %s at %s: %w", path, shortSHA(state.HeadSHA), err)
		}
		if ok {
			docs[path] = string(b)
		}
		return string(b), ok, nil
	}
	set := func(path, content string) {
		docs[path] = content
		if !slices.Contains(changed, path) {
			changed = append(changed, path)
		}
	}

	for _, i := range targets {
		p := state.Proposals[i]
		if p.Section != "" {
			doc, ok, err := load(p.DocPath)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("%w: %s does not exist at %s", errDocMismatch, p.DocPath, shortSHA(state.HeadSHA))
			}
			updated, err := splice(doc, p.Original, p.Content)
			if err != nil {
				return nil, fmt.Errorf("%w: %s: %w", errDocMismatch, p.DocPath, err)
			}
			set(p.DocPath, updated)
			continue
		}
		_, exists, err := load(p.DocPath)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("%w: %s already exists at %s", errDocMismatch, p.DocPath, shortSHA(state.HeadSHA))
		}
		set(p.DocPath, p.Content)
		if p.IndexEntry == "" {
			continue
		}
		readme, _, err := load(indexPath)
		if err != nil {
			return nil, err
		}
		set(indexPath, addIndexEntry(readme, p.IndexEntry))
	}

	files := make([]FileChange, len(changed))
	for n, path := range changed {
		files[n] = FileChange{Path: path, Content: docs[path]}
	}
	return files, nil
}

func applyMessage(state PRState, targets []int) string {
	if len(targets) > 1 {
		return fmt.Sprintf("docs: apply %d pollux-agent proposals", len(targets))
	}
	p := state.Proposals[targets[0]]
	message := "docs: apply pollux-agent proposal for " + p.DocPath
	if p.Section != "" {
		message += " § " + p.Section
	}
	return message
}

// adoptCommit recovers from a commit that landed before its state was saved:
// the branch tip is ours when it is the head's child with the message we would
// have written.
func (s *Service) adoptCommit(ctx context.Context, state PRState, message string) (sha string, adopted bool, err error) {
	c, err := s.comments.BranchCommit(ctx, state.InstallationID, state.Owner, state.Repo, state.HeadRef)
	if err != nil {
		return "", false, fmt.Errorf("read branch %s: %w", state.HeadRef, err)
	}
	if !slices.Equal(c.Parents, []string{state.HeadSHA}) || c.Message != message {
		return "", false, nil
	}
	return c.SHA, true, nil
}

// tickApplied flips the Apply checkbox of the applied proposals ids that have one.
func (s *Service) tickApplied(ctx context.Context, state PRState, ids []string, op string) error {
	existing, err := s.gh.ListComments(ctx, state.InstallationID, state.Owner, state.Repo, state.Number)
	if err != nil {
		return fmt.Errorf("%s: list comments: %w", op, err)
	}
	for _, p := range state.Proposals {
		if p.CommentID == 0 || !slices.Contains(ids, p.ID) {
			continue
		}
		c, ok := findComment(existing, CommentKindReview, p.CommentID)
		if !ok {
			continue
		}
		body, ok := tickApply(c.Body)
		if !ok {
			continue
		}
		if err := s.gh.EditReviewComment(ctx, state.InstallationID, state.Owner, state.Repo, p.CommentID, body); err != nil {
			return fmt.Errorf("%s: tick proposal %s: %w", op, p.ID, err)
		}
	}
	return nil
}

// replyApplied posts the "Applied" reply still missing under each applied
// proposal's comment (just proposal only, when it is not ""), saving after each.
func (s *Service) replyApplied(ctx context.Context, state PRState, only, op string) (posted int, err error) {
	for i, p := range state.Proposals {
		if p.State != ProposalApplied || p.ReplyID != 0 || p.AppliedSHA == "" || p.CommentID == 0 || (only != "" && p.ID != only) {
			continue
		}
		c, err := s.comments.ReplyToReviewComment(ctx, state.InstallationID, state.Owner, state.Repo, state.Number, p.CommentID, "✅ Applied in "+shortSHA(p.AppliedSHA))
		if err != nil {
			return posted, fmt.Errorf("%s: %w", op, err)
		}
		state.Proposals[i].ReplyID = c.ID
		if err := s.store.SavePR(ctx, state); err != nil {
			return posted, fmt.Errorf("%s: save reply %d: %w", op, c.ID, err)
		}
		posted++
	}
	return posted, nil
}

// say answers ev: in the review thread for a review comment, else as an issue
// comment addressed to the sender. A refused tick is then unticked so it can be
// ticked again. It returns ReactionRefused for the caller to settle.
func (s *Service) say(ctx context.Context, state PRState, ev CommentEvent, text, op string) (Reaction, error) {
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
// review comment loses the tick on that one line.
func (s *Service) untick(ctx context.Context, state PRState, ev CommentEvent, op string) error {
	if ev.Kind == CommentKindIssue {
		if state.SummaryCommentID == 0 || ev.CommentID != state.SummaryCommentID {
			return nil
		}
		if err := s.gh.EditIssueComment(ctx, state.InstallationID, state.Owner, state.Repo, state.SummaryCommentID, renderSummary(state)); err != nil {
			return fmt.Errorf("%s: untick summary comment: %w", op, err)
		}
		return nil
	}
	existing, err := s.gh.ListComments(ctx, state.InstallationID, state.Owner, state.Repo, state.Number)
	if err != nil {
		return fmt.Errorf("%s: list comments: %w", op, err)
	}
	c, ok := findComment(existing, CommentKindReview, ev.CommentID)
	if !ok {
		return nil
	}
	want := strings.TrimSpace(ev.Ticked)
	lines := strings.Split(c.Body, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != want {
			continue
		}
		lines[i] = strings.Replace(l, "- [x]", "- [ ]", 1)
		if err := s.gh.EditReviewComment(ctx, state.InstallationID, state.Owner, state.Repo, ev.CommentID, strings.Join(lines, "\n")); err != nil {
			return fmt.Errorf("%s: untick comment %d: %w", op, ev.CommentID, err)
		}
		return nil
	}
	return nil
}

func shortSHA(sha string) string { return sha[:min(7, len(sha))] }
