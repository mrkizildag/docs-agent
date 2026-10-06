package e2e_test

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestWebhookApplyCommitsOnce(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t)
	state := h.loadPR()
	idA := gate.ProposalID("docs/a.md", "Usage")
	commentA := h.commentWith(proposalMarker(idA))

	h.tick("dev", commentA.ID, commentA.Body)
	h.waitState("applied proposal with reply", func(s gate.PRState) bool {
		return len(s.Proposals) > 0 && s.Proposals[0].ReplyID != 0
	})
	waitFor(t, "proposal comment reacted done", func() bool {
		return gocmp.Equal(h.gh.Reactions(gate.CommentKindReview, commentA.ID), []gate.Reaction{gate.ReactionDone})
	})

	wantCommit := []gatetest.Commit{{
		Branch: "feature", Parent: "sha1", Message: "docs: apply pollux-agent proposal for docs/a.md § Usage", SHA: appliedSHA,
		Files: []gate.FileChange{{Path: "docs/a.md", Content: "# A\n\n## Usage\nnew\n\n## Other\nx\n"}},
	}}
	wantReply := []gatetest.Reply{{To: commentA.ID, Body: appliedReply(appliedSHA, idA)}}
	if diff := gocmp.Diff(wantCommit, h.gh.Committed()); diff != "" {
		t.Errorf("commits (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(wantReply, h.gh.Replies()); diff != "" {
		t.Errorf("replies (-want +got):\n%s", diff)
	}

	got := h.loadPR()
	reply := h.commentWith(appliedReply(appliedSHA, idA))
	if p := got.Proposals[0]; p.State != gate.ProposalApplied || p.AppliedSHA != appliedSHA || p.ReplyID != reply.ID {
		t.Errorf("proposal = %+v, want applied at %s with reply %d", p, appliedSHA, reply.ID)
	}
	if other := state.Proposals[1].State; got.Proposals[1].State != other {
		t.Errorf("other proposal state = %s, want unchanged %s", got.Proposals[1].State, other)
	}

	// Jobs on the PR run in order, so the third Permission check starts after
	// the redelivered job has finished.
	h.tick("dev", commentA.ID, commentA.Body)
	h.tick("dev", commentA.ID, commentA.Body)
	waitFor(t, "third comment job", func() bool { return h.gh.CallCount("Permission") >= 3 })
	if commits, replies := h.gh.Committed(), h.gh.Replies(); len(commits) != 1 || len(replies) != 1 {
		t.Errorf("after redelivery: %d commits, %d replies, want 1, 1", len(commits), len(replies))
	}
}

func TestWebhookApplyAll(t *testing.T) {
	t.Parallel()

	t.Run("summary tick", func(t *testing.T) {
		t.Parallel()
		h := newApplyHarness(t)
		h.tickSummary("dev", "User", "Apply all")
		h.requireAppliedAll()
	})

	t.Run("command", func(t *testing.T) {
		t.Parallel()
		h := newApplyHarness(t)
		h.command("dev", "/pollux-agent apply")
		h.requireAppliedAll()
	})
}

func TestWebhookBotCommitIsAnalyzedOnce(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, review.NoImpact{Reason: "docs already match"})
	h.tickSummary("dev", "User", "Apply all")
	h.requireAppliedAll()

	run, state := h.pushWith(appliedSHA, pushOpts{sender: "pollux-agent[bot]", senderType: "Bot"})

	if run.Conclusion != gate.ConclusionSuccess || run.HeadSHA != appliedSHA {
		t.Errorf("check run for the bot's commit = %s on %s, want success on %s", run.Conclusion, run.HeadSHA, appliedSHA)
	}
	if n := h.runner.remaining(); n != 0 {
		t.Errorf("%d scripted verdicts left, want the bot's push analyzed once", n)
	}
	for _, p := range state.Proposals {
		if p.State != gate.ProposalApplied {
			t.Errorf("proposal %s = %s after the bot's push, want applied", p.ID, p.State)
		}
	}
}

func TestWebhookRerunRepeatingAppliedProposalsPasses(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, twoProposals())
	h.tickSummary("dev", "User", "Apply all")
	h.requireAppliedAll()
	before := len(h.gh.Comments())

	run, state := h.pushWith(appliedSHA, pushOpts{sender: "pollux-agent[bot]", senderType: "Bot"})

	for _, p := range state.Proposals {
		if p.State != gate.ProposalApplied {
			t.Errorf("proposal %s = %s after a re-run repeating it, want applied", p.ID, p.State)
		}
	}
	if n := len(h.gh.Comments()); n != before {
		t.Errorf("comments created by the re-run = %d, want 0", n-before)
	}
	if run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("re-run check run = %s %q, want success: every proposal it repeats is already applied", run.Conclusion, run.Title)
	}
}

func TestWebhookRerunAfterApplyingOneStillProposesTheRest(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, twoProposals())
	idA := gate.ProposalID("docs/a.md", "Usage")
	commentA := h.commentWith(proposalMarker(idA))
	h.tick("dev", commentA.ID, commentA.Body)
	h.waitState("a applied with reply", func(s gate.PRState) bool {
		for _, p := range s.Proposals {
			if p.ID == idA {
				return p.State == gate.ProposalApplied && p.ReplyID != 0
			}
		}
		return false
	})
	before := len(h.gh.Comments())

	run, state := h.pushWith(appliedSHA, pushOpts{sender: "pollux-agent[bot]", senderType: "Bot"})

	for _, p := range state.Proposals {
		want := gate.ProposalOpen
		if p.ID == idA {
			want = gate.ProposalApplied
		}
		if p.State != want {
			t.Errorf("proposal %s = %s after the re-run, want %s", p.ID, p.State, want)
		}
	}
	if n := len(h.gh.Comments()); n != before {
		t.Errorf("comments created by the re-run = %d, want 0", n-before)
	}
	if run.Conclusion != gate.ConclusionActionRequired {
		t.Errorf("re-run check run = %s %q, want action_required: docs/b.md is still open", run.Conclusion, run.Title)
	}
	if strings.Contains(run.Summary, "docs/a.md") || !strings.Contains(run.Summary, "docs/b.md") {
		t.Errorf("re-run summary = %q, want only the open docs/b.md proposal listed", run.Summary)
	}
}

func TestWebhookCommandReactions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		canWrite bool
		sender   string
		text     string
		want     gate.Reaction
	}{
		{name: "refused for a user without write access", canWrite: false, sender: "stranger", text: "/pollux-agent apply", want: gate.ReactionRefused},
		{name: "done for a skip with a reason", canWrite: true, sender: "dev", text: "/pollux-agent skip typo only", want: gate.ReactionDone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gh := repoGitHub()
			gh.CanWrite = tc.canWrite
			h := newApplyHarnessOn(t, gh)
			h.command(tc.sender, tc.text)
			waitFor(t, "final reaction", func() bool {
				return gocmp.Equal(h.gh.Reactions(gate.CommentKindIssue, 100), []gate.Reaction{tc.want})
			})
		})
	}
}

func TestWebhookApplyWithoutWriteAccessIsRefused(t *testing.T) {
	t.Parallel()

	gh := repoGitHub()
	gh.CanWrite = false
	h := newApplyHarnessOn(t, gh)
	before := len(h.gh.Comments())

	h.command("stranger", "/pollux-agent apply")
	reply := h.waitReply(before)

	if n := len(h.gh.Comments()); n != before+1 {
		t.Errorf("comments created = %d, want exactly one reply", n-before)
	}
	if !strings.Contains(reply.Body, "@stranger") || !strings.Contains(reply.Body, "write access") {
		t.Errorf("reply = %q, want it to tell @stranger about write access", reply.Body)
	}
	if commits, replies := h.gh.Committed(), h.gh.Replies(); len(commits) != 0 || len(replies) != 0 {
		t.Errorf("%d commits and %d thread replies, want none", len(commits), len(replies))
	}
}

func TestWebhookIgnoredEditsApplyNothing(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, review.NoImpact{Reason: "barrier"})
	summary := h.commentWith(summaryMarker)
	commentA := h.commentWith(proposalMarker(gate.ProposalID("docs/a.md", "Usage")))

	h.tickSummary("pollux-agent[bot]", "Bot", "Apply all")
	h.issueComment("edited", "dev", "User", summary.ID, summary.Body, strings.Replace(summary.Body, "- [ ] Apply all", "- [x] Apply all", 1))
	h.sys.deliver("pull_request_review_comment", marshal(t, map[string]any{
		"action":       "edited",
		"changes":      map[string]any{"body": map[string]any{"from": commentA.Body}},
		"comment":      map[string]any{"id": commentA.ID, "body": strings.Replace(commentA.Body, "- [ ]", "- [x]", 1)},
		"pull_request": map[string]any{"number": 1},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": "pollux-agent[bot]", "type": "Bot"},
	}))

	// Jobs on a PR run in order, so a finished push means the edits above were handled.
	_, state := h.push("sha2")

	if n, commits, replies := h.gh.CallCount("Permission"), h.gh.Committed(), h.gh.Replies(); n != 0 || len(commits) != 0 || len(replies) != 0 {
		t.Errorf("permission checks = %d, commits = %d, replies = %d, want none from ignored edits", n, len(commits), len(replies))
	}
	for _, p := range state.Proposals {
		if p.State == gate.ProposalApplied {
			t.Errorf("proposal %s applied by an ignored edit", p.ID)
		}
	}
}
