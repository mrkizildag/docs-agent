package gate_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

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

// apiWithComments is a repository where the sender may write, docs/ is baseFiles
// and the pull request carries the three proposal comments and the summary of threeState.
func apiWithComments() *gatetest.GitHub {
	api := &gatetest.GitHub{PullRequest: gate.PullRequest{HeadSHA: "head1", Open: true}, CanWrite: true, Files: baseFiles()}
	for range 3 {
		api.AddComment(gate.CommentKindReview, "proposal\n- [ ] Apply this change\n")
	}
	api.AddComment(gate.CommentKindIssue, "summary")
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
			proposalEdits := len(writes.Creates) + len(writes.Edits)
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
		gh          func(*gatetest.GitHub)
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
			gh:          func(f *gatetest.GitHub) { f.Files["docs/README.md"] = "# Docs\n" },
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
		{name: "live head moved on", event: reviewTick(1), gh: func(f *gatetest.GitHub) { f.PullRequest.HeadSHA = "head2" }, wantSay: "re-analyzed", wantReact: gate.ReactionRefused},
		{name: "closed pull request", event: reviewTick(1), gh: func(f *gatetest.GitHub) { f.PullRequest.Open = false }, wantSay: "closed", wantReact: gate.ReactionRefused},
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
			gh: func(f *gatetest.GitHub) {
				f.Files["docs/README.md"] = "# Docs\n\n## Index\n\n- [C](c.md)\n- [A](a.md)\n\n## More\ntext\n"
			},
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Index\n\n- [C](c.md)\n- [A](a.md)\n\n## More\ntext\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "index entry under another heading is still added", event: issueComment("/pollux-agent apply"),
			state: func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh: func(f *gatetest.GitHub) {
				f.Files["docs/README.md"] = "# Docs\n\n## Index\n\n- [A](a.md)\n\n## More\n- [C](c.md)\n"
			},
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Index\n\n- [A](a.md)\n- [C](c.md)\n\n## More\n- [C](c.md)\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "index entry already listed in a readme without an index heading is not added again", event: issueComment("/pollux-agent apply"),
			state: func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh: func(f *gatetest.GitHub) {
				f.Files["docs/README.md"] = "# Docs\n\n## Docs\n\n- [C](c.md)\n- [A](a.md)\n"
			},
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Docs\n\n- [C](c.md)\n- [A](a.md)\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "index heading without items gets the entry right after it", event: issueComment("/pollux-agent apply"),
			state:       func(s *gate.PRState) { s.Proposals = s.Proposals[2:] },
			gh:          func(f *gatetest.GitHub) { f.Files["docs/README.md"] = "# Docs\n\n## Index\n\n## More\ntext\n" },
			wantMessage: "docs: apply pollux-agent proposal for docs/c.md",
			wantFiles:   map[string]string{"docs/c.md": "# C\n", "docs/README.md": "# Docs\n\n## Index\n- [C](c.md)\n\n## More\ntext\n"},
			wantApplied: []string{"p3"}, wantReplies: []int64{3}, wantTicks: 1,
			wantReact: gate.ReactionDone,
		},
		{
			name: "branch moved, commit not ours", event: reviewTick(1),
			gh: func(f *gatetest.GitHub) {
				f.Fail, f.Branches = map[string]error{"CommitFiles": gate.ErrBranchMoved}, map[string]gate.Commit{"feature": notMine}
			},
			wantSay:   "branch moved",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "splice mismatch", event: reviewTick(1),
			gh:        func(f *gatetest.GitHub) { f.Files["docs/a.md"] = "# A\n" },
			wantSay:   "no longer matches",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "one bad proposal commits nothing", event: summaryTick("Apply all"),
			gh:        func(f *gatetest.GitHub) { f.Files["docs/b.md"] = "# B\n" },
			wantSay:   "no longer matches",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "new doc already exists", event: reviewTick(3),
			gh:        func(f *gatetest.GitHub) { f.Files["docs/c.md"] = "taken" },
			wantSay:   "no longer matches",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "branch moved, bot commit adopted", event: reviewTick(1),
			gh: func(f *gatetest.GitHub) {
				f.Fail, f.Branches = map[string]error{"CommitFiles": gate.ErrBranchMoved}, map[string]gate.Commit{"feature": adoptable}
			},
			wantApplied: []string{"p1"}, wantReplies: []int64{1},
			wantReact: gate.ReactionDone,
		},
		{
			name: "branch moved, different message", event: reviewTick(1),
			gh: func(f *gatetest.GitHub) {
				f.Fail, f.Branches = map[string]error{"CommitFiles": gate.ErrBranchMoved}, map[string]gate.Commit{"feature": wrongMessage}
			},
			wantSay:   "branch moved",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "branch moved, different parent", event: reviewTick(1),
			gh: func(f *gatetest.GitHub) {
				f.Fail, f.Branches = map[string]error{"CommitFiles": gate.ErrBranchMoved}, map[string]gate.Commit{"feature": wrongParent}
			},
			wantSay:   "branch moved",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "commit fails", event: reviewTick(1),
			gh:        func(f *gatetest.GitHub) { f.Fail = map[string]error{"CommitFiles": errors.New("boom")} },
			wantErr:   true,
			wantReact: gate.ReactionSeen,
		},
		{
			name: "denied review tick replies in the thread", event: reviewTick(1),
			gh:        func(f *gatetest.GitHub) { f.CanWrite = false },
			wantSay:   "@dev",
			wantReact: gate.ReactionRefused,
		},
		{
			name: "denied command replies to the sender", event: issueComment("/pollux-agent apply"),
			gh:        func(f *gatetest.GitHub) { f.CanWrite = false },
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
			store := newStore(t, st)
			gh := apiWithComments()
			if tc.gh != nil {
				tc.gh(gh)
			}
			svc := newService(gh, store, gate.Runners{}, nil)

			err := svc.HandleComment(t.Context(), tc.event)

			if (err != nil) != tc.wantErr {
				t.Errorf("HandleComment() error = %v, wantErr %v", err, tc.wantErr)
			}
			commits := gh.Committed()
			if tc.wantFiles == nil && len(commits) != 0 {
				t.Errorf("commits = %+v, want none", commits)
			}
			if tc.wantFiles != nil {
				if len(commits) != 1 {
					t.Fatalf("commits = %d, want 1", len(commits))
				}
				got := map[string]string{}
				for _, f := range commits[0].Files {
					got[f.Path] = f.Content
				}
				if diff := cmp.Diff(tc.wantFiles, got); diff != "" {
					t.Errorf("committed files (-want +got):\n%s", diff)
				}
				if commits[0].Message != tc.wantMessage {
					t.Errorf("message = %q, want %q", commits[0].Message, tc.wantMessage)
				}
			}

			if diff := cmp.Diff([]gate.Reaction{tc.wantReact}, gh.Reactions(tc.event.Kind, tc.event.CommentID)); diff != "" {
				t.Errorf("reactions on comment %d (-want +got):\n%s", tc.event.CommentID, diff)
			}

			var applied []string
			stored := loadPR(t, store, 3)
			for i, p := range stored.Proposals {
				if p.State == gate.ProposalApplied && st.Proposals[i].State == gate.ProposalOpen {
					applied = append(applied, p.ID)
				}
			}
			if diff := cmp.Diff(tc.wantApplied, applied); diff != "" {
				t.Errorf("newly applied (-want +got):\n%s", diff)
			}

			var applyReplies []int64
			var says []string
			for _, r := range gh.Replies() {
				if strings.HasPrefix(r.Body, "✅ Applied in ") {
					applyReplies = append(applyReplies, r.To)
				} else {
					says = append(says, r.Body)
				}
			}
			comments := gh.Comments()
			for _, c := range comments {
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
				for _, p := range stored.Proposals {
					if p.CommentID == id && p.ReplyID == 0 {
						t.Errorf("proposal %s has no ReplyID after its reply", p.ID)
					}
				}
			}
			if n := gh.CallCount("EditReviewComment"); n != tc.wantTicks {
				t.Errorf("checkbox edits = %d, want %d", n, tc.wantTicks)
			}
			if tc.wantTicks > 0 {
				for _, p := range stored.Proposals {
					if p.State == gate.ProposalApplied && !strings.Contains(comments[p.CommentID-1].Body, "[x] Apply this change") {
						t.Errorf("comment %d body = %q, want the Apply box ticked", p.CommentID, comments[p.CommentID-1].Body)
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

		gh := apiWithComments()
		svc := newService(gh, newStore(t, threeState()), gate.Runners{}, nil)
		before := len(gh.Calls())

		if err := svc.HandleComment(t.Context(), issueComment("thanks!")); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		if calls := gh.Calls()[before:]; len(calls) != 0 || len(gh.Comments()) != 4 {
			t.Errorf("GitHub calls = %+v, comments = %d, want none made", calls, len(gh.Comments()))
		}
	})

	t.Run("a redelivered tick never commits twice", func(t *testing.T) {
		t.Parallel()

		store := newStore(t, threeState())
		gh := apiWithComments()
		svc := newService(gh, store, gate.Runners{}, nil)

		for range 2 {
			if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
				t.Fatalf("HandleComment() error = %v", err)
			}
		}
		if len(gh.Committed()) != 1 || len(gh.Replies()) != 1 {
			t.Errorf("commits %d, replies %d, want 1 and 1", len(gh.Committed()), len(gh.Replies()))
		}
	})
}

func TestHandleCommentRefusalUnticks(t *testing.T) {
	t.Parallel()

	t.Run("a denied review tick replies and unticks the box", func(t *testing.T) {
		t.Parallel()

		gh := apiWithComments()
		gh.CanWrite = false
		gh.SetCommentBody(1, "proposal\n- [x] Apply this change\n")
		svc := newService(gh, newStore(t, threeState()), gate.Runners{}, nil)

		if err := svc.HandleComment(t.Context(), reviewTick(1)); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		if replies := gh.Replies(); len(replies) != 1 || !strings.Contains(replies[0].Body, "write access") {
			t.Errorf("replies = %v, want one write-access refusal", replies)
		}
		if got, want := gh.Comments()[0].Body, "proposal\n- [ ] Apply this change\n"; got != want {
			t.Errorf("comment 1 body = %q, want %q", got, want)
		}
	})

	t.Run("a fork summary tick replies and redraws the summary unticked", func(t *testing.T) {
		t.Parallel()

		state := threeState()
		state.Fork = true
		gh := apiWithComments()
		gh.SetCommentBody(summaryID, "- [x] Apply all")
		svc := newService(gh, newStore(t, state), gate.Runners{}, nil)

		if err := svc.HandleComment(t.Context(), summaryTick("Apply all")); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		c := callsOf(gh)
		if replies := gh.Replies(); len(replies) != 0 || c.createIssue != 1 {
			t.Errorf("review replies %d, issue comments %d, want 0 and 1", len(replies), c.createIssue)
		}
		body := gh.Comments()[summaryID-1].Body
		if c.editIssue != 1 || strings.Contains(body, "- [x] Apply all") || !strings.Contains(body, "fork") {
			t.Errorf("summary edits %d, body = %q, want one redraw without a ticked Apply all", c.editIssue, body)
		}
	})

	t.Run("a denied command edits no comment", func(t *testing.T) {
		t.Parallel()

		gh := apiWithComments()
		gh.CanWrite = false
		svc := newService(gh, newStore(t, threeState()), gate.Runners{}, nil)

		if err := svc.HandleComment(t.Context(), issueComment("/pollux-agent apply")); err != nil {
			t.Fatalf("HandleComment() error = %v", err)
		}
		if c := callsOf(gh); c.createIssue != 1 || c.editIssue != 0 || c.editReview != 0 {
			t.Errorf("issue comments %d, edits %d and %d, want 1, 0 and 0", c.createIssue, c.editIssue, c.editReview)
		}
	})
}

func TestHandleCommentIgnoredReactsToNothing(t *testing.T) {
	t.Parallel()

	gh := apiWithComments()
	svc := newService(gh, newStore(t, threeState()), gate.Runners{}, nil)

	if err := svc.HandleComment(t.Context(), issueComment("looks good")); err != nil {
		t.Fatalf("HandleComment() error = %v", err)
	}
	if got := gh.ReactionLog(); len(got) != 0 {
		t.Errorf("reactions added = %v, want none", got)
	}
}

func TestHandleCommentApplyRefusesWhenAFileIsTooLargeToRead(t *testing.T) {
	t.Parallel()

	tooLarge := fmt.Errorf("read: %w", review.ErrFileTooLarge)
	for _, path := range []string{review.IndexPath, "docs/a.md", "docs/c.md"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			store := newStore(t, threeState())
			gh := apiWithComments()
			gh.FileErrs = map[string]error{path: tooLarge}
			svc := newService(gh, store, gate.Runners{}, nil)

			ev := summaryTick("Apply all")
			if err := svc.HandleComment(t.Context(), ev); err != nil {
				t.Fatalf("HandleComment() = %v, want nil", err)
			}
			comments := gh.Comments()
			if last := comments[len(comments)-1]; !strings.Contains(last.Body, path+" is too large to edit; nothing was committed.") {
				t.Errorf("reply = %q, want the too-large refusal naming %s", last.Body, path)
			}
			if summary := comments[summaryID-1].Body; strings.Contains(summary, "[x]") {
				t.Errorf("summary = %q, want the Apply all box unticked", summary)
			}
			if diff := cmp.Diff([]gate.Reaction{gate.ReactionRefused}, gh.Reactions(ev.Kind, ev.CommentID)); diff != "" {
				t.Errorf("reactions (-want +got):\n%s", diff)
			}
			if commits := gh.Committed(); len(commits) != 0 {
				t.Errorf("commits = %+v, want none", commits)
			}
			for _, p := range loadPR(t, store, 3).Proposals {
				if p.State != gate.ProposalOpen {
					t.Errorf("proposal %s = %v, want still open", p.ID, p.State)
				}
			}
		})
	}
}
