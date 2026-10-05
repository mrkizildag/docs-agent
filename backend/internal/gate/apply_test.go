package gate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

type reply struct {
	to   int64
	body string
}

type fakeCommentGitHub struct {
	canWrite  bool
	files     map[string]string
	commitErr error
	branch    gate.Commit
	byAdd     map[string]gate.Commit // CommitAt by sha; the branch tip is found too
	sha       string                 // the commit CommitFiles makes; "abcdef1234567" when empty
	api       *fakeGitHub            // when set, replies are also added to its comments
	replyErr  error                  // ReplyToReviewComment fails with it

	permissionCalls int
	commits         [][]gate.FileChange
	messages        []string
	replies         []reply

	nextReaction int64
	reactions    map[reactionKey]map[int64]gate.Reaction
	reactionLog  []gate.Reaction // every reaction added, in order
	reactedAs    int64           // installation of the last React call
}

type reactionKey struct {
	kind gate.CommentKind
	id   int64
}

// reactionsOn returns the reactions currently on the comment.
func (f *fakeCommentGitHub) reactionsOn(kind gate.CommentKind, id int64) []gate.Reaction {
	var got []gate.Reaction
	for _, r := range f.reactions[reactionKey{kind, id}] {
		got = append(got, r)
	}
	return got
}

func (f *fakeCommentGitHub) Permission(context.Context, int64, string, string, string) (bool, error) {
	f.permissionCalls++
	return f.canWrite, nil
}

func (f *fakeCommentGitHub) FileAtRef(_ context.Context, _ int64, _, _, path, _ string) ([]byte, bool, error) {
	c, ok := f.files[path]
	return []byte(c), ok, nil
}

func (f *fakeCommentGitHub) CommitFiles(_ context.Context, _ int64, _, _, _, _ string, files []gate.FileChange, message string) (string, error) {
	if f.commitErr != nil {
		return "", f.commitErr
	}
	f.commits = append(f.commits, files)
	f.messages = append(f.messages, message)
	if f.sha != "" {
		return f.sha, nil
	}
	return "abcdef1234567", nil
}

func (f *fakeCommentGitHub) BranchCommit(context.Context, int64, string, string, string) (gate.Commit, error) {
	return f.branch, nil
}

func (f *fakeCommentGitHub) ReplyToReviewComment(_ context.Context, _ int64, _, _ string, _ int, to int64, body string) (gate.Comment, error) {
	if f.replyErr != nil {
		return gate.Comment{}, f.replyErr
	}
	f.replies = append(f.replies, reply{to: to, body: body})
	if f.api != nil {
		return f.api.addComment(gate.CommentKindReview, body), nil
	}
	return gate.Comment{ID: int64(900 + len(f.replies))}, nil
}

const (
	docA   = "# A\n\n## Usage\nold\n\n## Other\nx\n"
	docB   = "# B\n\n## B\nold\n"
	readme = "# Docs\n\n## Index\n\n- [A](a.md)\n- [B](b.md)\n\n## More\ntext\n"

	summaryID   = 4
	singleMsg   = "docs: apply pollux-agent proposal for docs/a.md § Usage"
	applyTicked = "- [x] Apply this change"
)

func baseFiles() map[string]string {
	return map[string]string{"docs/a.md": docA, "docs/b.md": docB, "docs/README.md": readme}
}

// threeState has an open proposal in docs/a.md (comment 1), one in docs/b.md
// (comment 2) and a new doc docs/c.md (comment 3); comment 4 is the summary.
func threeState() gate.PRState {
	return gate.PRState{
		InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3,
		HeadSHA: "head1", ProposalsSHA: "head1", HeadRef: "feature", SummaryCommentID: summaryID,
		Proposals: []gate.ProposalState{
			{ID: "p1", DocPath: "docs/a.md", Section: "Usage", CommentID: 1, State: gate.ProposalOpen, Original: "## Usage\nold\n", Content: "## Usage\nnew\n"},
			{ID: "p2", DocPath: "docs/b.md", Section: "B", CommentID: 2, State: gate.ProposalOpen, Original: "## B\nold\n", Content: "## B\nnewer\n"},
			{ID: "p3", DocPath: "docs/c.md", CommentID: 3, State: gate.ProposalOpen, Content: "# C\n", IndexEntry: "- [C](c.md)"},
		},
	}
}

func apiWithComments() *fakeGitHub {
	api := &fakeGitHub{pullRequest: gate.PullRequest{HeadSHA: "head1", Open: true}}
	for range 3 {
		api.addComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	}
	api.addComment(gate.CommentKindIssue, "summary")
	return api
}

func reviewTick(id int64) gate.CommentEvent {
	return gate.CommentEvent{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, Sender: "dev", CommentID: id, Kind: gate.CommentKindReview, Ticked: applyTicked}
}

func issueComment(body string) gate.CommentEvent {
	return gate.CommentEvent{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 3, Sender: "dev", CommentID: 99, Kind: gate.CommentKindIssue, Body: body}
}

func summaryTick(label string) gate.CommentEvent {
	ev := issueComment("")
	ev.CommentID, ev.Ticked = summaryID, "- [x] "+label
	return ev
}

func TestParseIntent(t *testing.T) {
	t.Parallel()

	pending := func(s *gate.PRState) { s.PendingSkip = &gate.SkipAsk{User: "Dev", Scope: gate.SkipCommit} }
	tests := []struct {
		name  string
		event gate.CommentEvent
		state func(*gate.PRState)
		want  gate.Intent
	}{
		{name: "apply tick", event: reviewTick(1), want: gate.Intent{Kind: gate.IntentApply, ProposalID: "p1"}},
		{name: "crlf tick", event: func() gate.CommentEvent { e := reviewTick(2); e.Ticked += "\r"; return e }(), want: gate.Intent{Kind: gate.IntentApply, ProposalID: "p2"}},
		{name: "other label", event: func() gate.CommentEvent { e := reviewTick(1); e.Ticked = "- [x] Something else"; return e }()},
		{name: "unknown review comment", event: reviewTick(11)},
		{name: "tick on a non-summary issue comment", event: func() gate.CommentEvent { e := summaryTick("Apply all"); e.CommentID = 5; return e }()},
		{name: "apply all tick", event: summaryTick("Apply all"), want: gate.Intent{Kind: gate.IntentApplyAll}},
		{name: "skip commit tick", event: summaryTick("Skip this commit"), want: gate.Intent{Kind: gate.IntentSkipAsk, Scope: gate.SkipCommit}},
		{name: "skip pr tick", event: summaryTick("Skip this PR"), want: gate.Intent{Kind: gate.IntentSkipAsk, Scope: gate.SkipPR}},
		{name: "rerun tick", event: summaryTick("Re-run analysis"), want: gate.Intent{Kind: gate.IntentRerun}},
		{name: "unknown summary label", event: summaryTick("Something else")},
		{name: "apply command", event: issueComment("/pollux-agent apply"), want: gate.Intent{Kind: gate.IntentApplyAll}},
		{name: "skip with reason", event: issueComment("/pollux-agent skip typo only"), want: gate.Intent{Kind: gate.IntentSkip, Scope: gate.SkipCommit, Reason: "typo only"}},
		{name: "skip without reason", event: issueComment("/pollux-agent skip"), want: gate.Intent{Kind: gate.IntentSkipAsk, Scope: gate.SkipCommit}},
		{name: "skip-pr with multi-line reason", event: issueComment("/pollux-agent skip-pr\nline one\nline two\n"), want: gate.Intent{Kind: gate.IntentSkip, Scope: gate.SkipPR, Reason: "line one\nline two"}},
		{name: "skip-pr without reason", event: issueComment("  /pollux-agent skip-pr  "), want: gate.Intent{Kind: gate.IntentSkipAsk, Scope: gate.SkipPR}},
		{name: "unknown command", event: issueComment("/pollux-agent bogus"), state: pending},
		{name: "plain comment", event: issueComment("looks good")},
		{name: "pending asker answers", event: issueComment("because it is generated"), state: pending, want: gate.Intent{Kind: gate.IntentSkipReason, Reason: "because it is generated"}},
		{name: "someone else answers", event: func() gate.CommentEvent { e := issueComment("because"); e.Sender = "other"; return e }(), state: pending},
		{name: "asker sends a command", event: issueComment("/pollux-agent apply"), state: pending, want: gate.Intent{Kind: gate.IntentApplyAll}},
		{name: "asker ticks a box", event: reviewTick(11), state: pending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := threeState()
			if tc.state != nil {
				tc.state(&st)
			}
			if diff := cmp.Diff(tc.want, gate.ParseIntent(tc.event, st)); diff != "" {
				t.Errorf("ParseIntent() (-want +got):\n%s", diff)
			}
		})
	}
}

func openState() gate.PRState {
	st := threeState()
	st.Proposals = st.Proposals[:1]
	return st
}

func TestOnApply(t *testing.T) {
	t.Parallel()

	prev := openState()
	prev.Proposals = append(prev.Proposals,
		gate.ProposalState{ID: "p2", State: gate.ProposalOpen},
		gate.ProposalState{ID: "p3", State: gate.ProposalOutdated})

	got := gate.OnApply(prev, []string{"p1", "p3"}, "sha9")

	want := map[string]gate.ProposalStatus{"p1": gate.ProposalApplied, "p2": gate.ProposalOpen, "p3": gate.ProposalOutdated}
	for _, p := range got.Proposals {
		if p.State != want[p.ID] {
			t.Errorf("proposal %s state = %s, want %s", p.ID, p.State, want[p.ID])
		}
	}
	if got.Proposals[0].AppliedSHA != "sha9" || got.Proposals[2].AppliedSHA != "" {
		t.Errorf("AppliedSHA = %q, %q, want sha9 on p1 only", got.Proposals[0].AppliedSHA, got.Proposals[2].AppliedSHA)
	}
	if prev.Proposals[0].State != gate.ProposalOpen {
		t.Error("OnApply mutated its input")
	}
}

func TestReconcileKeepsApplied(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	id := gate.ProposalID("docs/a.md", "A")
	applied := gate.ProposalState{
		ID: id, DocPath: "docs/a.md", Section: "A", CommentID: 1, State: gate.ProposalApplied,
		Content: p.Content, AppliedSHA: "abc", ReplyID: 7,
	}
	existing := []gate.Comment{{ID: 1, Mine: true, Kind: gate.CommentKindReview, Body: "<!-- pollux-agent:proposal:" + id + " -->"}}
	changed := proposal("docs/a.md", "A")
	changed.Content = "## A\nnewer\n"

	tests := []struct {
		name      string
		verdict   review.Verdict
		wantState gate.ProposalStatus
		wantSHA   string
		wantReply int64
		wantEdits int
	}{
		{name: "same content stays applied", verdict: review.Proposals{p}, wantState: gate.ProposalApplied, wantSHA: "abc", wantReply: 7},
		{name: "absent stays applied", verdict: review.NoImpact{Reason: "x"}, wantState: gate.ProposalApplied, wantSHA: "abc", wantReply: 7},
		{name: "different content reopens", verdict: review.Proposals{changed}, wantState: gate.ProposalOpen, wantEdits: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state, writes := gate.Reconcile(gate.PRState{Proposals: []gate.ProposalState{applied}}, testPR(), tc.verdict, nil, existing)

			got := state.Proposals[0]
			if got.State != tc.wantState || got.AppliedSHA != tc.wantSHA || got.ReplyID != tc.wantReply {
				t.Errorf("proposal = %s %q reply %d, want %s %q reply %d", got.State, got.AppliedSHA, got.ReplyID, tc.wantState, tc.wantSHA, tc.wantReply)
			}
			proposalEdits := 0
			for _, w := range writes {
				if !w.Summary {
					proposalEdits++
				}
			}
			if proposalEdits != tc.wantEdits {
				t.Errorf("proposal writes = %d, want %d", proposalEdits, tc.wantEdits)
			}
			if state.ProposalsSHA != testPR().HeadSHA {
				t.Errorf("ProposalsSHA = %q, want %q", state.ProposalsSHA, testPR().HeadSHA)
			}
		})
	}
}

func TestReconcileStoresEdit(t *testing.T) {
	t.Parallel()

	p := proposal("docs/a.md", "A")
	p.IndexEntry = "- [A](a.md)"
	state, _ := gate.Reconcile(gate.PRState{}, testPR(), review.Proposals{p}, nil, nil)

	want := gate.ProposalState{
		ID: gate.ProposalID("docs/a.md", "A"), DocPath: "docs/a.md", Section: "A", State: gate.ProposalOpen,
		Content: p.Content, Original: p.Original, IndexEntry: p.IndexEntry,
	}
	if diff := cmp.Diff(want, state.Proposals[0]); diff != "" {
		t.Errorf("stored proposal (-want +got):\n%s", diff)
	}
}

func TestHandleComment(t *testing.T) {
	t.Parallel()

	adoptable := gate.Commit{SHA: "botbot1234", Parents: []string{"head1"}, Message: singleMsg, Mine: true}
	wrongMessage := gate.Commit{SHA: "botbot1234", Parents: []string{"head1"}, Message: "someone else's", Mine: true}
	wrongParent := gate.Commit{SHA: "botbot1234", Parents: []string{"other"}, Message: singleMsg, Mine: true}
	notMine := gate.Commit{SHA: "botbot1234", Parents: []string{"head1"}, Message: singleMsg}
	indexSplice := func(s *gate.PRState) {
		s.Proposals = []gate.ProposalState{
			s.Proposals[2],
			{ID: "p4", DocPath: "docs/README.md", Section: "Index", CommentID: 2, State: gate.ProposalOpen,
				Original: "## Index\n\n- [A](a.md)\n- [B](b.md)\n\n", Content: "## Index\n\n- [A](a.md)\n- [B](b.md)\n- [X](x.md)\n\n"},
		}
	}
	appliedReplyMissing := func(s *gate.PRState) {
		s.Proposals[0].State, s.Proposals[0].AppliedSHA = gate.ProposalApplied, "abcdef1234567"
		s.Proposals[1].State, s.Proposals[1].AppliedSHA, s.Proposals[1].ReplyID = gate.ProposalApplied, "abcdef1234567", 7
		s.Proposals[2].State = gate.ProposalOutdated
	}
	tests := []struct {
		name        string
		event       gate.CommentEvent
		state       func(*gate.PRState)
		gh          func(*fakeCommentGitHub)
		api         func(*fakeGitHub)
		wantErr     bool
		wantMessage string
		wantFiles   map[string]string
		wantApplied []string
		wantSay     string
		wantReplies []int64
		wantTicks   int
		wantReact   gate.Reaction
	}{
		{
			name: "apply one", event: reviewTick(1),
			wantMessage: singleMsg, wantFiles: map[string]string{"docs/a.md": "# A\n\n## Usage\nnew\n\n## Other\nx\n"},
			wantApplied: []string{"p1"}, wantReplies: []int64{1},
			wantReact: gate.ReactionDone,
		},
		{
			name: "apply all across two docs and a new doc", event: summaryTick("Apply all"),
			wantMessage: "docs: apply 3 pollux-agent proposals",
			wantFiles: map[string]string{
				"docs/a.md":      "# A\n\n## Usage\nnew\n\n## Other\nx\n",
				"docs/b.md":      "# B\n\n## B\nnewer\n",
				"docs/c.md":      "# C\n",
				"docs/README.md": "# Docs\n\n## Index\n\n- [A](a.md)\n- [B](b.md)\n- [C](c.md)\n\n## More\ntext\n",
			},
			wantApplied: []string{"p1", "p2", "p3"}, wantReplies: []int64{1, 2, 3}, wantTicks: 3,
			wantReact: gate.ReactionDone,
		},
		{
			name: "apply command appends the index entry when there is no index", event: issueComment("/pollux-agent apply"),
			state:       func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh:          func(f *fakeCommentGitHub) { f.files["docs/README.md"] = "# Docs\n" },
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n- [C](c.md)\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{name: "fork", event: reviewTick(1), state: func(s *gate.PRState) { s.Fork = true }, wantSay: "fork", wantReact: gate.ReactionRefused},
		{name: "stale head while a newer push is analyzed", event: reviewTick(1), state: func(s *gate.PRState) {
			s.ProposalsSHA, s.Run = "older", &gate.AwaitingRun{RunID: 1, Nonce: "n"}
		}, wantSay: "re-analyzed", wantReact: gate.ReactionRefused},
		{name: "stale head with nothing running", event: reviewTick(1), state: func(s *gate.PRState) { s.ProposalsSHA = "older" }, wantSay: "no analysis is running", wantReact: gate.ReactionRefused},
		{name: "stale head after a failed analysis offers a re-run", event: reviewTick(1), state: func(s *gate.PRState) {
			s.ProposalsSHA, s.FailureCause = "older", "The analysis timed out."
		}, wantSay: "Re-run analysis", wantReact: gate.ReactionRefused},
		{name: "live head moved on", event: reviewTick(1), api: func(f *fakeGitHub) { f.pullRequest.HeadSHA = "head2" }, wantSay: "re-analyzed", wantReact: gate.ReactionRefused},
		{name: "closed pull request", event: reviewTick(1), api: func(f *fakeGitHub) { f.pullRequest.Open = false }, wantSay: "closed", wantReact: gate.ReactionRefused},
		{name: "invalid stored proposal", event: reviewTick(1), state: func(s *gate.PRState) { s.Proposals[0].Section = "Usage\nx" }, wantSay: "not valid", wantReact: gate.ReactionRefused},
		{name: "tick on an outdated proposal is refused", event: reviewTick(1), state: func(s *gate.PRState) { s.Proposals[0].State = gate.ProposalOutdated }, wantSay: "outdated", wantReact: gate.ReactionRefused},
		{
			name: "new doc's index entry goes in after a splice of the index", event: summaryTick("Apply all"), state: indexSplice,
			wantMessage: "docs: apply 2 pollux-agent proposals",
			wantFiles: map[string]string{
				"docs/c.md":      "# C\n",
				"docs/README.md": "# Docs\n\n## Index\n\n- [A](a.md)\n- [B](b.md)\n- [X](x.md)\n- [C](c.md)\n\n## More\ntext\n",
			},
			wantApplied: []string{"p3", "p4"}, wantReplies: []int64{3, 2}, wantTicks: 2,
			wantReact: gate.ReactionDone,
		},
		{
			name: "index entry already present is not added again", event: issueComment("/pollux-agent apply"),
			state: func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh: func(f *fakeCommentGitHub) {
				f.files["docs/README.md"] = "# Docs\n\n## Index\n\n- [C](c.md)\n- [A](a.md)\n\n## More\ntext\n"
			},
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Index\n\n- [C](c.md)\n- [A](a.md)\n\n## More\ntext\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "index entry under another heading is still added", event: issueComment("/pollux-agent apply"),
			state: func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh: func(f *fakeCommentGitHub) {
				f.files["docs/README.md"] = "# Docs\n\n## Index\n\n- [A](a.md)\n\n## More\n- [C](c.md)\n"
			},
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Index\n\n- [A](a.md)\n- [C](c.md)\n\n## More\n- [C](c.md)\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "index entry already listed in a readme without an index heading is not added again", event: issueComment("/pollux-agent apply"),
			state: func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh: func(f *fakeCommentGitHub) {
				f.files["docs/README.md"] = "# Docs\n\n## Docs\n\n- [C](c.md)\n- [A](a.md)\n"
			},
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Docs\n\n- [C](c.md)\n- [A](a.md)\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "index heading without items gets the entry right after it", event: issueComment("/pollux-agent apply"),
			state:       func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh:          func(f *fakeCommentGitHub) { f.files["docs/README.md"] = "# Docs\n\n## Index\n\n## More\ntext\n" },
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Index\n- [C](c.md)\n\n## More\ntext\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "branch moved, commit not ours", event: reviewTick(1),
			gh:        func(f *fakeCommentGitHub) { f.commitErr, f.branch = gate.ErrBranchMoved, notMine },
			wantSay:   "branch moved",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "splice mismatch", event: reviewTick(1),
			gh:        func(f *fakeCommentGitHub) { f.files["docs/a.md"] = "# A\n" },
			wantSay:   "no longer matches",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "one bad proposal commits nothing", event: summaryTick("Apply all"),
			gh:        func(f *fakeCommentGitHub) { f.files["docs/b.md"] = "# B\n" },
			wantSay:   "no longer matches",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "new doc already exists", event: reviewTick(3),
			gh:        func(f *fakeCommentGitHub) { f.files["docs/c.md"] = "taken" },
			wantSay:   "no longer matches",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "branch moved, bot commit adopted", event: reviewTick(1),
			gh:          func(f *fakeCommentGitHub) { f.commitErr, f.branch = gate.ErrBranchMoved, adoptable },
			wantApplied: []string{"p1"}, wantReplies: []int64{1},
			wantReact: gate.ReactionDone,
		},
		{
			name: "branch moved, different message", event: reviewTick(1),
			gh:        func(f *fakeCommentGitHub) { f.commitErr, f.branch = gate.ErrBranchMoved, wrongMessage },
			wantSay:   "branch moved",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "branch moved, different parent", event: reviewTick(1),
			gh:        func(f *fakeCommentGitHub) { f.commitErr, f.branch = gate.ErrBranchMoved, wrongParent },
			wantSay:   "branch moved",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "commit fails", event: reviewTick(1),
			gh:        func(f *fakeCommentGitHub) { f.commitErr = errors.New("boom") },
			wantErr:   true,
			wantReact: gate.ReactionSeen,
		},
		{
			name: "denied review tick replies in the thread", event: reviewTick(1),
			gh:        func(f *fakeCommentGitHub) { f.canWrite = false },
			wantSay:   "@dev",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "denied command replies to the sender", event: issueComment("/pollux-agent apply"),
			gh:        func(f *fakeCommentGitHub) { f.canWrite = false },
			wantSay:   "@dev",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "redelivery ticks the boxes and posts only the missing replies", event: summaryTick("Apply all"), state: appliedReplyMissing,
			wantReplies: []int64{1}, wantTicks: 2,
			wantReact: gate.ReactionDone,
		},
		{
			name: "redelivery of a tick posts its missing reply", event: reviewTick(1), state: appliedReplyMissing,
			wantReplies: []int64{1},
			wantReact:   gate.ReactionDone,
		},
		{
			name: "apply all with nothing to do", event: issueComment("/pollux-agent apply"),
			state:     func(s *gate.PRState) { s.Proposals = s.Proposals[:0] },
			wantSay:   "Nothing left to apply",
			wantReact: gate.ReactionDone,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := threeState()
			if tc.state != nil {
				tc.state(&st)
			}
			store := &fakeStore{stored: st, live: true}
			gh := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
			if tc.gh != nil {
				tc.gh(gh)
			}
			api := apiWithComments()
			if tc.api != nil {
				tc.api(api)
			}
			svc := gate.NewService(api, gh, store, gate.Runners{})

			err := svc.HandleComment(t.Context(), tc.event)

			if (err != nil) != tc.wantErr {
				t.Errorf("HandleComment() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantFiles == nil && len(gh.commits) != 0 {
				t.Errorf("commits = %+v, want none", gh.commits)
			}
			if tc.wantFiles != nil {
				if len(gh.commits) != 1 {
					t.Fatalf("commits = %d, want 1", len(gh.commits))
				}
				got := map[string]string{}
				for _, f := range gh.commits[0] {
					got[f.Path] = f.Content
				}
				if diff := cmp.Diff(tc.wantFiles, got); diff != "" {
					t.Errorf("committed files (-want +got):\n%s", diff)
				}
				if gh.messages[0] != tc.wantMessage {
					t.Errorf("message = %q, want %q", gh.messages[0], tc.wantMessage)
				}
			}

			if diff := cmp.Diff([]gate.Reaction{tc.wantReact}, gh.reactionsOn(tc.event.Kind, tc.event.CommentID)); diff != "" {
				t.Errorf("reactions on comment %d (-want +got):\n%s", tc.event.CommentID, diff)
			}

			var applied []string
			for i, p := range store.stored.Proposals {
				if p.State == gate.ProposalApplied && st.Proposals[i].State == gate.ProposalOpen {
					applied = append(applied, p.ID)
				}
			}
			if diff := cmp.Diff(tc.wantApplied, applied); diff != "" {
				t.Errorf("newly applied (-want +got):\n%s", diff)
			}

			var applyReplies []int64
			var says []string
			for _, r := range gh.replies {
				if strings.HasPrefix(r.body, "✅ Applied in ") {
					applyReplies = append(applyReplies, r.to)
				} else {
					says = append(says, r.body)
				}
			}
			for _, c := range api.comments {
				if c.Kind == gate.CommentKindIssue && c.ID > summaryID {
					says = append(says, c.Body)
				}
			}
			if diff := cmp.Diff(tc.wantReplies, applyReplies); diff != "" {
				t.Errorf("Applied replies (-want +got):\n%s", diff)
			}
			if tc.wantSay == "" && len(says) != 0 {
				t.Errorf("user-facing replies = %q, want none", says)
			}
			if tc.wantSay != "" && !strings.Contains(strings.Join(says, "\n"), tc.wantSay) {
				t.Errorf("user-facing replies = %q, want one containing %q", says, tc.wantSay)
			}
			for _, id := range tc.wantReplies {
				for _, p := range store.stored.Proposals {
					if p.CommentID == id && p.ReplyID == 0 {
						t.Errorf("proposal %s has no ReplyID after its reply", p.ID)
					}
				}
			}
			if api.editReview != tc.wantTicks {
				t.Errorf("checkbox edits = %d, want %d", api.editReview, tc.wantTicks)
			}
			if tc.wantTicks > 0 {
				for _, p := range store.stored.Proposals {
					if p.State == gate.ProposalApplied && !strings.Contains(api.comments[p.CommentID-1].Body, "[x] Apply this change") {
						t.Errorf("comment %d body = %q, want the Apply box ticked", p.CommentID, api.comments[p.CommentID-1].Body)
					}
				}
			}
		})
	}
}

func TestHandleCommentSideEffects(t *testing.T) {
	t.Parallel()

	t.Run("a comment asking for nothing costs no GitHub call", func(t *testing.T) {
		t.Parallel()

		gh := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
		api := apiWithComments()
		svc := gate.NewService(api, gh, &fakeStore{stored: threeState(), live: true}, gate.Runners{})

		if err := svc.HandleComment(t.Context(), issueComment("thanks!")); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		if gh.permissionCalls != 0 || len(gh.replies) != 0 || len(api.comments) != 4 {
			t.Errorf("permission calls %d, replies %d, comments %d, want none made", gh.permissionCalls, len(gh.replies), len(api.comments))
		}
	})

	t.Run("a redelivered tick never commits twice", func(t *testing.T) {
		t.Parallel()

		store := &fakeStore{stored: threeState(), live: true}
		gh := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
		svc := gate.NewService(apiWithComments(), gh, store, gate.Runners{})

		for range 2 {
			if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
				t.Fatalf("HandleComment() error = %v", err)
			}
		}
		if len(gh.commits) != 1 || len(gh.replies) != 1 {
			t.Errorf("commits %d, replies %d, want 1 and 1", len(gh.commits), len(gh.replies))
		}
	})
}

func TestHandleCommentRefusalUnticks(t *testing.T) {
	t.Parallel()

	t.Run("a denied review tick replies and unticks the box", func(t *testing.T) {
		t.Parallel()

		gh := &fakeCommentGitHub{files: baseFiles()}
		api := apiWithComments()
		api.edit(1, "proposal\n- [x] Apply this change\n")
		svc := gate.NewService(api, gh, &fakeStore{stored: threeState(), live: true}, gate.Runners{})

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		if len(gh.replies) != 1 || !strings.Contains(gh.replies[0].body, "write access") {
			t.Errorf("replies = %v, want one write-access refusal", gh.replies)
		}
		if got, want := api.comments[0].Body, "proposal\n- [ ] Apply this change\n"; got != want {
			t.Errorf("comment 1 body = %q, want %q", got, want)
		}
	})

	t.Run("a fork summary tick replies and redraws the summary unticked", func(t *testing.T) {
		t.Parallel()

		state := threeState()
		state.Fork = true
		gh := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
		api := apiWithComments()
		api.edit(summaryID, "- [x] Apply all")
		svc := gate.NewService(api, gh, &fakeStore{stored: state, live: true}, gate.Runners{})

		if err := svc.HandleComment(t.Context(), summaryTick("Apply all")); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		if len(gh.replies) != 0 || api.createIssue != 1 {
			t.Errorf("review replies %d, issue comments %d, want 0 and 1", len(gh.replies), api.createIssue)
		}
		body := api.comments[summaryID-1].Body
		if api.editIssue != 1 || strings.Contains(body, "- [x] Apply all") || !strings.Contains(body, "fork") {
			t.Errorf("summary edits %d, body = %q, want one redraw without a ticked Apply all", api.editIssue, body)
		}
	})

	t.Run("a denied command edits no comment", func(t *testing.T) {
		t.Parallel()

		gh := &fakeCommentGitHub{files: baseFiles()}
		api := apiWithComments()
		svc := gate.NewService(api, gh, &fakeStore{stored: threeState(), live: true}, gate.Runners{})

		if err := svc.HandleComment(t.Context(), issueComment("/pollux-agent apply")); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		if api.createIssue != 1 || api.editIssue != 0 || api.editReview != 0 {
			t.Errorf("issue comments %d, edits %d and %d, want 1, 0 and 0", api.createIssue, api.editIssue, api.editReview)
		}
	})
}

func (f *fakeCommentGitHub) React(ctx context.Context, installationID int64, _, _ string, kind gate.CommentKind, id int64, reaction gate.Reaction) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("react: %w", err)
	}
	f.reactedAs = installationID
	f.reactionLog = append(f.reactionLog, reaction)
	key := reactionKey{kind, id}
	if f.reactions == nil {
		f.reactions = map[reactionKey]map[int64]gate.Reaction{}
	}
	if f.reactions[key] == nil {
		f.reactions[key] = map[int64]gate.Reaction{}
	}
	f.nextReaction++
	f.reactions[key][f.nextReaction] = reaction
	return f.nextReaction, nil
}

func (f *fakeCommentGitHub) Unreact(ctx context.Context, _ int64, _, _ string, kind gate.CommentKind, id, reactionID int64) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("unreact: %w", err)
	}
	delete(f.reactions[reactionKey{kind, id}], reactionID)
	return nil
}

func TestHandleCommentIgnoredReactsToNothing(t *testing.T) {
	t.Parallel()

	gh := &fakeCommentGitHub{canWrite: true, files: baseFiles()}
	svc := gate.NewService(apiWithComments(), gh, &fakeStore{stored: threeState(), live: true}, gate.Runners{})

	if err := svc.HandleComment(t.Context(), issueComment("looks good")); err != nil {
		t.Fatalf("HandleComment() error = %v", err)
	}
	if gh.nextReaction != 0 {
		t.Errorf("reactions added = %d, want 0", gh.nextReaction)
	}
}

func (f *fakeCommentGitHub) CommitAt(_ context.Context, _ int64, _, _, sha string) (gate.Commit, error) {
	if c, ok := f.byAdd[sha]; ok {
		return c, nil
	}
	if f.branch.SHA == sha {
		return f.branch, nil
	}
	return gate.Commit{}, nil
}
