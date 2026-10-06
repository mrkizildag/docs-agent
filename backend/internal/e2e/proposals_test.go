package e2e_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestWebhookPostsProposalCommentsAndLinksThemInTheSummary(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals())

	run, state := h.push("sha1")

	if run.Conclusion != gate.ConclusionActionRequired {
		t.Errorf("check run conclusion = %q, want %q", run.Conclusion, gate.ConclusionActionRequired)
	}
	posted := h.gh.PostedComments()
	if len(posted) != 3 || !strings.Contains(posted[0].Body, summaryMarker) || strings.Contains(posted[0].Body, "https://gh/review/") {
		t.Fatalf("posted comments = %+v, want the summary first, without links to comments that do not exist yet, then two review comments", posted)
	}
	reviews := h.gh.ReviewComments()
	if len(reviews) != 2 {
		t.Fatalf("review comments = %d, want 2", len(reviews))
	}
	summary := h.commentWith(summaryMarker).Body
	for i, want := range []struct{ path, marker string }{
		{"a.go", proposalMarker(gate.ProposalID("docs/a.md", "Usage"))},
		{"b.go", proposalMarker(gate.ProposalID("docs/b.md", ""))},
	} {
		if reviews[i].Path != want.path || reviews[i].CommitSHA != "sha1" || !strings.Contains(reviews[i].Body, want.marker) {
			t.Errorf("review comment %d = %+v, want path %s on sha1 with marker %s", i, reviews[i], want.path, want.marker)
		}
		comment := h.commentWith(want.marker)
		if !strings.Contains(summary, comment.URL) {
			t.Errorf("summary missing link %s to comment %d:\n%s", comment.URL, i+1, summary)
		}
		if state.Proposals[i].CommentID != comment.ID {
			t.Errorf("saved proposal %d comment ID = %d, want %d", i, state.Proposals[i].CommentID, comment.ID)
		}
	}
	if state.SummaryCommentID != h.commentWith(summaryMarker).ID {
		t.Errorf("saved SummaryCommentID = %d, want the summary comment's ID", state.SummaryCommentID)
	}
}

func TestWebhookReconcilesProposalCommentsAcrossPushes(t *testing.T) {
	t.Parallel()

	markerA := proposalMarker(gate.ProposalID("docs/a.md", "Usage"))
	markerB := proposalMarker(gate.ProposalID("docs/b.md", ""))

	t.Run("same proposals edit in place", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), twoProposals())

		h.push("sha1")
		if n := len(h.gh.Comments()); n != 3 {
			t.Fatalf("comments after first push = %d, want 3", n)
		}

		run, _ := h.push("sha2")
		if n := len(h.gh.Comments()); n != 3 {
			t.Errorf("comments after second push = %d, want 3 edited in place", n)
		}
		if edits := h.edits(); edits != 4 {
			t.Errorf("edits = %d, want 4 (two review comments and the summary, which is also edited once to add links on its first run)", edits)
		}
		if run.Conclusion != gate.ConclusionActionRequired {
			t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionActionRequired)
		}
		if body := h.commentWith(markerA).Body; strings.Contains(body, "Outdated") {
			t.Errorf("comment A marked outdated:\n%s", body)
		}
	})

	t.Run("dropped proposal is marked outdated", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), review.Proposals{twoProposals()[0]})

		h.push("sha1")
		run, state := h.push("sha2")

		if run.Conclusion != gate.ConclusionActionRequired {
			t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionActionRequired)
		}
		if body := h.commentWith(markerB).Body; !strings.Contains(body, "Outdated") {
			t.Errorf("dropped proposal comment not outdated:\n%s", body)
		}
		if body := h.commentWith(markerA).Body; strings.Contains(body, "Outdated") {
			t.Errorf("kept proposal comment marked outdated:\n%s", body)
		}
		summary := h.commentWith(summaryMarker).Body
		if !strings.Contains(summary, "| outdated |") || !strings.Contains(summary, "| open |") {
			t.Errorf("summary should show one open and one outdated row:\n%s", summary)
		}
		if n := len(h.gh.Comments()); n != 3 {
			t.Errorf("comments = %d, want 3", n)
		}
		if len(state.Proposals) != 2 {
			t.Errorf("saved proposals = %+v, want 2", state.Proposals)
		}
	})

	t.Run("no impact outdates everything", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), review.NoImpact{Reason: "docs already match"})

		h.push("sha1")
		run, _ := h.push("sha2")

		if run.Conclusion != gate.ConclusionSuccess {
			t.Errorf("conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
		}
		for _, marker := range []string{markerA, markerB} {
			if body := h.commentWith(marker).Body; !strings.Contains(body, "Outdated") {
				t.Errorf("comment %s not outdated:\n%s", marker, body)
			}
		}
		summary := h.commentWith(summaryMarker).Body
		if strings.Contains(summary, "| open |") || strings.Count(summary, "| outdated |") != 2 {
			t.Errorf("summary should show two outdated rows:\n%s", summary)
		}
		if n := len(h.gh.Comments()); n != 3 {
			t.Errorf("comments = %d, want 3", n)
		}
	})

	t.Run("recovers after a crash before saving comment IDs", func(t *testing.T) {
		t.Parallel()
		h := newPushHarness(t, twoProposals(), twoProposals())

		_, state := h.push("sha1")
		state.SummaryCommentID = 0
		for i := range state.Proposals {
			state.Proposals[i].CommentID = 0
			state.Proposals[i].CommentURL = ""
		}
		if err := h.store.SavePR(t.Context(), state); err != nil {
			t.Fatalf("SavePR() error = %v", err)
		}

		_, state = h.push("sha2")
		if n := len(h.gh.Comments()); n != 3 {
			t.Errorf("comments = %d, want 3", n)
		}
		if edits := h.edits(); edits != 4 {
			t.Errorf("edits = %d, want 4", edits)
		}
		if state.SummaryCommentID == 0 || state.Proposals[0].CommentID == 0 || state.Proposals[1].CommentID == 0 {
			t.Errorf("saved state did not re-adopt comment IDs: %+v", state)
		}
	})
}

func TestWebhookForkPullRequestGetsNoApplyOffer(t *testing.T) {
	t.Parallel()

	gh := repoGitHub()
	gh.Files["docs/a.md"] = "# A\n\n## Usage\nold\n\n## Other\nx\n"
	h := newHarness(t, gh, twoProposals())
	_, state := h.pushWith("sha1", pushOpts{headRepo: "forker/widgets"})

	for _, p := range state.Proposals {
		body := h.commentWith(proposalMarker(p.ID)).Body
		if strings.Contains(body, "- [ ] Apply this change") || !strings.Contains(body, "Apply is not available") {
			t.Errorf("fork proposal comment should explain instead of offering Apply:\n%s", body)
		}
	}
	if summary := h.commentWith(summaryMarker).Body; !strings.Contains(summary, "Apply all is not available") || strings.Contains(summary, "- [ ] Apply all") {
		t.Errorf("fork summary should say why Apply all is missing:\n%s", summary)
	}

	before := len(h.gh.Comments())
	h.command("dev", "/pollux-agent apply")
	if reply := h.waitReply(before).Body; !strings.Contains(reply, "@dev") || !strings.Contains(reply, "fork") {
		t.Errorf("reply = %q, want it to tell @dev about the fork", reply)
	}
	if commits := h.gh.Committed(); len(commits) != 0 {
		t.Errorf("commits = %+v, want none on a fork", commits)
	}
}
