package gate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// ErrBranchMoved is returned by GitHub.CommitFiles when the branch no
// longer points at the parent commit.
var ErrBranchMoved = errors.New("gate: branch moved")

// ErrCommitRejected is returned by GitHub.CommitFiles when GitHub or the
// target tree refuses the commit for a reason the user must fix (a protected
// branch, a symlink or submodule at a proposal's path); nothing was committed.
var ErrCommitRejected = errors.New("gate: commit rejected")

// CommitRejectedError is an ErrCommitRejected with a reason fit to show on the
// pull request, such as "the branch is protected".
type CommitRejectedError struct {
	Reason string
}

func (e *CommitRejectedError) Error() string { return ErrCommitRejected.Error() + ": " + e.Reason }

func (e *CommitRejectedError) Unwrap() error { return ErrCommitRejected }

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
	Mine    bool // authored by this App's bot
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
// section of readme, right after the heading when that section has no items,
// or appends it when there is no such section. It returns readme unchanged
// when a line of that section, or of the whole file when it has no such
// section, already equals entry.
func addIndexEntry(readme, entry string) string {
	lines := strings.Split(readme, "\n")
	inIndex, heading, last := false, -1, -1
	hasIndex := slices.ContainsFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "## ") && strings.TrimSpace(l) == review.IndexHeading
	})
	for i, l := range lines {
		if !hasIndex && strings.TrimSpace(l) == strings.TrimSpace(entry) {
			return readme
		}
		switch {
		case strings.HasPrefix(l, "## "):
			inIndex = strings.TrimSpace(l) == review.IndexHeading
			if inIndex && heading < 0 {
				heading = i
			}
		case inIndex && strings.TrimSpace(l) == strings.TrimSpace(entry):
			return readme
		case inIndex && strings.HasPrefix(l, "- "):
			last = i
		}
	}
	if last < 0 && heading >= 0 {
		last = heading
	}
	if last < 0 {
		if readme != "" && !strings.HasSuffix(readme, "\n") {
			readme += "\n"
		}
		return readme + entry + "\n"
	}
	return strings.Join(slices.Concat(lines[:last+1], []string{entry}, lines[last+1:]), "\n")
}

// handleApply commits the open proposals the intent targets as one commit.
// ctx bounds the reads and the commit; each group of writes after them gets its
// own write budget.
func (s *Service) handleApply(ctx context.Context, state PRState, ev CommentEvent, in Intent) (Reaction, error) {
	if state.Fork {
		return s.say(ctx, state, ev, "Apply is not available on a fork the bot cannot push to.")
	}
	targets := openTargets(state, in)
	if len(targets) == 0 {
		return s.replayApplied(ctx, state, ev, in)
	}

	live, err := s.gh.GetPullRequest(ctx, state.InstallationID, state.Owner, state.Repo, state.Number)
	if err != nil {
		return "", fmt.Errorf("get pull request: %w", err)
	}
	if pa := state.PendingApply; pa != nil {
		c, adopted, err := s.findPendingApply(ctx, state, live.HeadSHA, *pa)
		if err != nil {
			return "", err
		}
		if adopted {
			var tick []string
			if in.Kind == IntentApplyAll {
				tick = pa.IDs
			}
			if state, err = s.saveAdopted(ctx, state, c.SHA); err != nil {
				return "", err
			}
			writeCtx, cancel := writeContext(ctx)
			defer cancel()
			if _, err := s.finishApply(writeCtx, state, tick, ""); err != nil {
				return "", err
			}
			if targets = openTargets(state, in); len(targets) == 0 {
				return ReactionDone, nil
			}
		}
	}
	if !live.Open {
		return s.say(ctx, state, ev, "this pull request is closed; nothing was committed.")
	}
	if state.HeadSHA != state.ProposalsSHA || live.HeadSHA != state.ProposalsSHA {
		return s.say(ctx, state, ev, staleText(state, live))
	}
	for _, i := range targets {
		p := state.Proposals[i]
		target := review.Proposal{DocPath: p.DocPath, Section: p.Section, IndexEntry: p.IndexEntry}
		if err := target.ValidateTarget(); err != nil {
			return s.say(ctx, state, ev, "a stored proposal is not valid ("+strings.ReplaceAll(err.Error(), "\n", "; ")+"); nothing was committed.")
		}
	}

	files, err := s.applyFiles(ctx, state, targets)
	if errors.Is(err, errDocMismatch) {
		return s.say(ctx, state, ev, "a proposal no longer matches the doc ("+err.Error()+"); nothing was committed.")
	}
	var large *fileTooLargeError
	if errors.As(err, &large) {
		return s.say(ctx, state, ev, large.Error()+"; nothing was committed.")
	}
	if err != nil {
		return "", fmt.Errorf("prepare files: %w", err)
	}
	message := applyMessage(state, targets)
	ids := make([]string, len(targets))
	for n, i := range targets {
		ids[n] = state.Proposals[i].ID
	}

	// Saved before the commit so a push of that commit, which can be handled
	// before this one is retried, still finds the proposals it applied.
	state.PendingApply = &PendingApply{IDs: ids, Message: message, Parent: state.HeadSHA}
	if err := s.store.SavePR(ctx, state); err != nil {
		return "", fmt.Errorf("save pending apply: %w", err)
	}
	sha, err := s.gh.CommitFiles(ctx, state.InstallationID, state.Owner, state.Repo, state.HeadRef, state.HeadSHA, files, message)
	if errors.Is(err, ErrBranchMoved) {
		var adopted bool
		if sha, adopted, err = s.adoptCommit(ctx, state, state.PendingApply); err != nil {
			return "", err
		}
		if !adopted {
			state.PendingApply = nil
			if err := s.saveWrite(ctx, state, "clear pending apply"); err != nil {
				return "", err
			}
			return s.say(ctx, state, ev, "the branch moved while applying; nothing was committed. A re-analysis follows.")
		}
	} else if errors.Is(err, ErrCommitRejected) {
		state.PendingApply = nil
		if err := s.saveWrite(ctx, state, "clear pending apply"); err != nil {
			return "", err
		}
		text := "GitHub rejected the commit (a protected branch, or a symlink or submodule at a doc path); nothing was committed."
		if rejected := (*CommitRejectedError)(nil); errors.As(err, &rejected) {
			text = "GitHub rejected the commit: " + rejected.Reason + "; nothing was committed."
		}
		return s.say(ctx, state, ev, text)
	} else if err != nil {
		return "", fmt.Errorf("commit files: %w", err)
	}

	state = OnApply(state, ids, sha)
	state.PendingApply = nil
	if err := s.saveWrite(ctx, state, "save after commit "+shortSHA(sha)); err != nil {
		return "", err
	}
	var tick []string
	if in.Kind == IntentApplyAll {
		tick = ids
	}
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if _, err := s.finishApply(writeCtx, state, tick, ""); err != nil {
		return "", err
	}
	return ReactionDone, nil
}

// openTargets returns the indexes of the open proposals the intent targets.
func openTargets(state PRState, in Intent) []int {
	var targets []int
	for i, p := range state.Proposals {
		if p.State == ProposalOpen && (in.Kind == IntentApplyAll || p.ID == in.ProposalID) {
			targets = append(targets, i)
		}
	}
	return targets
}

// saveWrite saves state under its own write budget.
func (s *Service) saveWrite(ctx context.Context, state PRState, what string) error {
	ctx, cancel := writeContext(ctx)
	defer cancel()
	if err := s.store.SavePR(ctx, state); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// staleText says why the proposals cannot be applied at the live head.
func staleText(state PRState, live PullRequest) string {
	switch {
	case state.Skip != nil && state.Skip.Scope == SkipPR:
		return "This PR is skipped, so its proposals are not refreshed; nothing was committed."
	case state.FailureCause != "":
		return "the last analysis failed, so the proposals are out of date; nothing was committed. Tick **" + rerunLabel + "** in the summary, then apply again."
	case state.Run == nil && live.HeadSHA == state.HeadSHA:
		return "the proposals are out of date and no analysis is running; nothing was committed. Push a commit to refresh them, then apply again."
	default:
		return "a newer push is being re-analyzed; nothing was committed. Apply again once the proposals refresh."
	}
}

// replayApplied handles an Apply that targets no open proposal: a redelivery
// finishes what the first run left undone, and an outdated proposal is refused.
func (s *Service) replayApplied(ctx context.Context, state PRState, ev CommentEvent, in Intent) (Reaction, error) {
	var applied []string
	for _, p := range state.Proposals {
		if in.Kind == IntentApply && p.ID == in.ProposalID && p.State == ProposalOutdated {
			return s.say(ctx, state, ev, "this proposal is outdated; nothing was committed.")
		}
		if p.State == ProposalApplied && p.AppliedSHA != "" && (in.Kind == IntentApplyAll || p.ID == in.ProposalID) {
			applied = append(applied, p.ID)
		}
	}

	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	var tick []string
	if in.Kind == IntentApplyAll {
		tick = applied
	}
	if len(applied) > 0 {
		if _, err := s.finishApply(writeCtx, state, tick, in.ProposalID); err != nil {
			return "", err
		}
	}
	if len(applied) == 0 && in.Kind == IntentApplyAll {
		if _, err := s.say(ctx, state, ev, "✅ Nothing left to apply."); err != nil {
			return "", err
		}
	}
	return ReactionDone, nil
}

// finishApply brings the comments up to date with state's applied proposals:
// it ticks the Apply boxes of tick, redraws the summary, and posts the replies
// still missing (just proposal only, when it is not ""). It returns how many
// replies it posted or adopted.
func (s *Service) finishApply(ctx context.Context, state PRState, tick []string, only string) (int, error) {
	if len(tick) > 0 {
		if err := s.tickApplied(ctx, state, tick); err != nil {
			return 0, err
		}
	}
	if err := s.redrawSummary(ctx, state); err != nil {
		return 0, err
	}
	return s.replyApplied(ctx, state, only)
}

// errDocMismatch marks a proposal that cannot be applied to the doc at head.
var errDocMismatch = errors.New("doc mismatch")

// fileTooLargeError is a file the apply needs that is too large to read.
type fileTooLargeError struct{ path string }

func (e *fileTooLargeError) Error() string { return e.path + " is too large to edit" }

func (e *fileTooLargeError) Unwrap() error { return review.ErrFileTooLarge }

// applyFiles builds the file changes for the proposals at targets, reading
// each doc once at the head. Section splices all run first; index entries for
// new docs go in last so they cannot break a splice of the index itself. It
// returns errDocMismatch for user-facing failures.
func (s *Service) applyFiles(ctx context.Context, state PRState, targets []int) ([]FileChange, error) {
	docs := map[string]string{}
	var changed []string
	load := func(path string) (string, bool, error) {
		if c, ok := docs[path]; ok {
			return c, true, nil
		}
		b, ok, err := s.gh.FileAtRef(ctx, state.InstallationID, state.Owner, state.Repo, path, state.HeadSHA)
		if errors.Is(err, review.ErrFileTooLarge) {
			return "", false, &fileTooLargeError{path: path}
		}
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

	var indexEntries []string
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
		if p.IndexEntry != "" {
			indexEntries = append(indexEntries, p.IndexEntry)
		}
	}
	if len(indexEntries) > 0 {
		readme, _, err := load(review.IndexPath)
		if err != nil {
			return nil, err
		}
		for _, entry := range indexEntries {
			readme = addIndexEntry(readme, entry)
		}
		set(review.IndexPath, readme)
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

// isPendingApply reports whether c is the commit pa was about to create: ours,
// the child of pa's parent, with pa's message.
func isPendingApply(c Commit, pa PendingApply) bool {
	return c.Mine && c.Message == pa.Message && slices.Equal(c.Parents, []string{pa.Parent})
}

// findPendingApply returns the commit pa was about to create when it is sha or
// the parent of sha: a user push that superseded the bot's own push job lands
// on top of it.
func (s *Service) findPendingApply(ctx context.Context, state PRState, sha string, pa PendingApply) (Commit, bool, error) {
	c, err := s.gh.CommitAt(ctx, state.InstallationID, state.Owner, state.Repo, sha)
	if err != nil {
		return Commit{}, false, fmt.Errorf("read commit %s: %w", shortSHA(sha), err)
	}
	return s.lookBack(ctx, state, c, pa)
}

// lookBack returns c, or else its first parent, when it is the commit pa was
// about to create.
func (s *Service) lookBack(ctx context.Context, state PRState, c Commit, pa PendingApply) (Commit, bool, error) {
	if isPendingApply(c, pa) {
		return c, true, nil
	}
	if len(c.Parents) == 0 {
		return Commit{}, false, nil
	}
	parent, err := s.gh.CommitAt(ctx, state.InstallationID, state.Owner, state.Repo, c.Parents[0])
	if err != nil {
		return Commit{}, false, fmt.Errorf("read commit %s: %w", shortSHA(c.Parents[0]), err)
	}
	return parent, isPendingApply(parent, pa), nil
}

// adoptCommit recovers from a commit that landed before its state was saved:
// the branch tip, or the commit under it, is ours when it is the pending apply's commit.
func (s *Service) adoptCommit(ctx context.Context, state PRState, pa *PendingApply) (sha string, adopted bool, err error) {
	c, err := s.gh.BranchCommit(ctx, state.InstallationID, state.Owner, state.Repo, state.HeadRef)
	if err != nil {
		return "", false, fmt.Errorf("read branch %s: %w", state.HeadRef, err)
	}
	c, adopted, err = s.lookBack(ctx, state, c, *pa)
	if err != nil || !adopted {
		return "", false, err
	}
	return c.SHA, true, nil
}

// adoptPendingApply marks as applied the proposals of an Apply whose commit
// landed but whose state was never saved, when the push of pr is that commit
// or sits on it; without it the push would outdate them. Any other push drops
// the pending apply. The caller brings the comments up to date when adopted.
func (s *Service) adoptPendingApply(ctx context.Context, state PRState, pr PullRequest) (next PRState, adopted bool, err error) {
	pa := state.PendingApply
	if pa == nil || state.HeadRef == "" || pr.HeadSHA == state.HeadSHA {
		return state, false, nil
	}
	c, adopted, err := s.findPendingApply(ctx, state, pr.HeadSHA, *pa)
	if err != nil {
		return PRState{}, false, err
	}
	if !adopted {
		return state, false, nil
	}
	next, err = s.saveAdopted(ctx, state, c.SHA)
	return next, err == nil, err
}

// saveAdopted records that the pending apply's commit sha landed.
func (s *Service) saveAdopted(ctx context.Context, state PRState, sha string) (PRState, error) {
	state = OnApply(state, state.PendingApply.IDs, sha)
	state.PendingApply = nil
	if err := s.saveWrite(ctx, state, "save adopted commit "+shortSHA(sha)); err != nil {
		return PRState{}, err
	}
	return state, nil
}

// tickApplied flips the Apply checkbox of the applied proposals ids that have one.
func (s *Service) tickApplied(ctx context.Context, state PRState, ids []string) error {
	existing, err := s.gh.ListComments(ctx, state.InstallationID, state.Owner, state.Repo, state.Number)
	if err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	for _, p := range state.Proposals {
		if p.CommentID == 0 || !slices.Contains(ids, p.ID) {
			continue
		}
		c, ok := findComment(existing, CommentKindReview, p.CommentID)
		if !ok {
			continue
		}
		body, ok := setCheckbox(c.Body, applyLabel, true)
		if !ok {
			continue
		}
		if err := s.gh.EditReviewComment(ctx, state.InstallationID, state.Owner, state.Repo, p.CommentID, body); err != nil {
			return fmt.Errorf("tick proposal %s: %w", p.ID, err)
		}
	}
	return nil
}

// appliedMarker names the proposal and the commit, so a proposal reopened and
// applied again gets a reply for its new commit.
func appliedMarker(id, sha string) string {
	return "<!-- pollux-agent:applied:" + id + ":" + sha + " -->"
}

// replyApplied posts the "Applied" reply still missing under each applied
// proposal's comment (just proposal only, when it is not ""), saving after each.
// A reply a crashed run posted before saving its ID is adopted by its marker.
// It returns how many replies it posted or adopted.
func (s *Service) replyApplied(ctx context.Context, state PRState, only string) (posted int, err error) {
	var existing []Comment
	listed := false
	for i, p := range state.Proposals {
		if p.State != ProposalApplied || p.ReplyID != 0 || p.AppliedSHA == "" || p.CommentID == 0 || (only != "" && p.ID != only) {
			continue
		}
		if !listed {
			if existing, err = s.gh.ListComments(ctx, state.InstallationID, state.Owner, state.Repo, state.Number); err != nil {
				return posted, fmt.Errorf("list comments: %w", err)
			}
			listed = true
		}
		replyID := int64(0)
		marker := appliedMarker(p.ID, p.AppliedSHA)
		if c, ok := findMine(existing, CommentKindReview, func(c Comment) bool { return strings.Contains(c.Body, marker) }); ok {
			replyID = c.ID
		} else {
			body := "✅ Applied in " + shortSHA(p.AppliedSHA) + "\n\n" + marker
			c, err := s.gh.ReplyToReviewComment(ctx, state.InstallationID, state.Owner, state.Repo, state.Number, p.CommentID, body)
			if err != nil {
				return posted, fmt.Errorf("reply to proposal %s: %w", p.ID, err)
			}
			replyID = c.ID
		}
		state.Proposals[i].ReplyID = replyID
		if err := s.store.SavePR(ctx, state); err != nil {
			return posted, fmt.Errorf("save reply %d: %w", replyID, err)
		}
		posted++
	}
	return posted, nil
}
