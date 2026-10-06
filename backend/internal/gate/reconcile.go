package gate

import (
	"slices"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// CommentWrites are the comment writes Reconcile asks the Service to perform:
// review comments to create and to edit, each for the proposal at Index in
// State.Proposals, and whether to write the summary comment, which the Service
// renders from the final state because it links the comments created before it.
type CommentWrites struct {
	Creates []ProposalCreate
	Edits   []ProposalEdit
	Summary bool
}

// ProposalCreate creates a new review comment for the proposal at Index.
type ProposalCreate struct {
	Index   int
	Comment ReviewComment
}

// ProposalEdit edits review comment ID of the proposal at Index to Body.
type ProposalEdit struct {
	Index int
	ID    int64
	Body  string
}

// Reconcile is the state transition for a finished run: pure, no I/O. It
// returns prev with only Proposals, ProposalsSHA and SummaryCommentID changed, and the
// comment writes that realize it. existing is the PR's current comments: our
// own comments carrying our markers are reused when state lacks their IDs, and
// an outdated proposal keeps its current body. Created comments' IDs and URLs
// belong in the returned state at the writes' Index. An applied proposal
// stays applied, and gets no write, while the verdict repeats its Content.
func Reconcile(prev PRState, pr PullRequest, verdict review.Verdict, changed []review.ChangedFile, existing []Comment) (PRState, CommentWrites) {
	next := prev
	next.Proposals = slices.Clone(prev.Proposals)
	next.ProposalsSHA = pr.HeadSHA
	proposals, _ := verdict.(review.Proposals)

	index := make(map[string]int, len(next.Proposals))
	for i, ps := range next.Proposals {
		index[ps.ID] = i
	}
	current := make(map[string]bool, len(proposals))
	var writes CommentWrites

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
		if appliedAs(*ps, p) {
			continue
		}
		p = withHeading(p)
		ps.DocPath, ps.Section, ps.State = p.DocPath, p.Section, ProposalOpen
		ps.Content, ps.Original, ps.IndexEntry = p.Content, p.Original, p.IndexEntry
		ps.AppliedSHA, ps.ReplyID = "", 0
		adoptMarked(ps, existing)

		rc := proposalComment(pr.HeadSHA, id, p, changed, pr.Fork)
		if c, ok := findComment(existing, CommentKindReview, ps.CommentID); ok {
			if !sameAnchor(c, rc) {
				rc.Body = renderCheckbox(id, p, pr.Fork)
			}
			writes.Edits = append(writes.Edits, ProposalEdit{Index: i, ID: ps.CommentID, Body: rc.Body})
		} else {
			ps.CommentID, ps.CommentURL = 0, ""
			writes.Creates = append(writes.Creates, ProposalCreate{Index: i, Comment: rc})
		}
	}

	for i := range next.Proposals {
		ps := &next.Proposals[i]
		if current[ps.ID] || ps.State == ProposalOutdated || ps.State == ProposalApplied {
			continue
		}
		adoptMarked(ps, existing)
		ps.State = ProposalOutdated
		if c, ok := findComment(existing, CommentKindReview, ps.CommentID); ok {
			writes.Edits = append(writes.Edits, ProposalEdit{Index: i, ID: ps.CommentID, Body: renderOutdated(ps.ID, pr.HeadSHA, c.Body)})
		}
	}

	summaryLost := resolveSummary(&next, existing)
	if next.SummaryCommentID != 0 || len(proposals) > 0 || summaryLost {
		writes.Summary = true
	}
	return next, writes
}

// reconcileFailure is the state transition for a failed run: pure, no I/O. It
// returns prev with only SummaryCommentID possibly changed; the summary write
// that reports the failure is the caller's. Proposals are left as they are, so
// earlier ones stay listed.
func reconcileFailure(prev PRState, existing []Comment) PRState {
	next := prev
	resolveSummary(&next, existing)
	return next
}

// resolveSummary points next at our existing summary comment when state lacks
// a live ID for it, and reports whether state's summary comment has vanished
// with no marked one to adopt.
func resolveSummary(next *PRState, existing []Comment) (lost bool) {
	if _, ok := findComment(existing, CommentKindIssue, next.SummaryCommentID); ok {
		return false
	}
	lost = next.SummaryCommentID != 0
	next.SummaryCommentID = 0
	if c, ok := findMarked(existing, CommentKindIssue, summaryMarker); ok {
		next.SummaryCommentID, lost = c.ID, false
	}
	return lost
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

// findMine returns the first comment of ours of kind that match accepts; a
// listed comment someone else wrote never matches, so it is never edited.
func findMine(existing []Comment, kind CommentKind, match func(Comment) bool) (Comment, bool) {
	for _, c := range existing {
		if c.Mine && c.Kind == kind && match(c) {
			return c, true
		}
	}
	return Comment{}, false
}

func findMarked(existing []Comment, kind CommentKind, marker string) (Comment, bool) {
	return findMine(existing, kind, func(c Comment) bool { return hasMarker(c.Body, marker) })
}

// hasMarker reports whether marker is the first line of body.
func hasMarker(body, marker string) bool {
	first, _, _ := strings.Cut(body, "\n")
	return strings.TrimRight(first, "\r") == marker
}

func findComment(existing []Comment, kind CommentKind, id int64) (Comment, bool) {
	return findMine(existing, kind, func(c Comment) bool { return c.ID == id })
}

// sameAnchor reports whether existing sits where rc would be created; a
// suggestion body is only safe to write onto the lines it was computed for.
func sameAnchor(existing Comment, rc ReviewComment) bool {
	return existing.Path == rc.Path && existing.StartLine == rc.StartLine && existing.Line == rc.Line
}
