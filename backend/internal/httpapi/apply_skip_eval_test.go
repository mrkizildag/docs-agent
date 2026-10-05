package httpapi_test

import (
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func TestWebhookRerunRepeatingAppliedProposalsPassesEndToEnd(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, twoProposals())
	h.tickSummary("dev", "User", "Apply all")
	h.requireAppliedAll()
	_, creates, _ := h.gh.snapshot()

	run, state := h.pushWith(e2eAppliedSHA, pushOpts{sender: "pollux-agent[bot]", senderType: "Bot"})

	for _, p := range state.Proposals {
		if p.State != gate.ProposalApplied {
			t.Errorf("proposal %s = %s after a re-run repeating it, want applied", p.ID, p.State)
		}
	}
	if _, n, _ := h.gh.snapshot(); n != creates {
		t.Errorf("comments created by the re-run = %d, want 0", n-creates)
	}
	if run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("re-run check run = %s %q, want success: every proposal it repeats is already applied", run.Conclusion, run.Title)
	}
}

func TestWebhookSkipReasonOnlyFromAskerEndToEnd(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals())
	h.push("sha1")

	h.command("dev", "/pollux-agent skip")
	h.waitState("pending skip", func(s gate.PRState) bool { return s.PendingSkip != nil })

	h.issueComment("created", "other", "User", 101, "unrelated remark", "")
	h.issueComment("created", "dev", "User", 102, "generated docs", "")
	state := h.waitState("skip recorded", func(s gate.PRState) bool { return s.Skip != nil })
	if state.Skip.User != "dev" || state.Skip.Reason != "generated docs" {
		t.Errorf("skip = %+v, want dev's reason, not the other user's comment", *state.Skip)
	}
	if run := h.lastRun(); run.Conclusion != gate.ConclusionSuccess || !strings.Contains(run.Summary, "generated docs") {
		t.Errorf("check run = %+v, want the skip's success", run)
	}
}

func TestWebhookRerunAfterApplyOneStillProposesTheRestEndToEnd(t *testing.T) {
	t.Parallel()

	h := newApplyHarness(t, twoProposals())
	idA := gate.ProposalID("docs/a.md", "Usage")
	commentA := h.commentWith("<!-- pollux-agent:proposal:" + idA + " -->")
	h.tick("r2-t1", "dev", commentA.ID, commentA.Body)
	h.waitState("a applied with reply", func(s gate.PRState) bool {
		for _, p := range s.Proposals {
			if p.ID == idA {
				return p.State == gate.ProposalApplied && p.ReplyID != 0
			}
		}
		return false
	})
	_, creates, _ := h.gh.snapshot()

	run, state := h.pushWith(e2eAppliedSHA, pushOpts{sender: "pollux-agent[bot]", senderType: "Bot"})

	for _, p := range state.Proposals {
		want := gate.ProposalOpen
		if p.ID == idA {
			want = gate.ProposalApplied
		}
		if p.State != want {
			t.Errorf("proposal %s = %s after the re-run, want %s", p.ID, p.State, want)
		}
	}
	if _, n, _ := h.gh.snapshot(); n != creates {
		t.Errorf("comments created by the re-run = %d, want 0", n-creates)
	}
	if run.Conclusion != gate.ConclusionActionRequired {
		t.Errorf("re-run check run = %s %q, want action_required: docs/b.md is still open", run.Conclusion, run.Title)
	}
	if strings.Contains(run.Summary, "docs/a.md") || !strings.Contains(run.Summary, "docs/b.md") {
		t.Errorf("re-run summary = %q, want only the open docs/b.md proposal listed", run.Summary)
	}
}

func TestWebhookCommandReactionsEndToEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		sender string
		text   string
		want   gate.Reaction
	}{
		{name: "refused for a user without write access", sender: "stranger", text: "/pollux-agent apply", want: gate.ReactionRefused},
		{name: "done for a skip with a reason", sender: "dev", text: "/pollux-agent skip typo only", want: gate.ReactionDone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newApplyHarness(t)
			h.command(tc.sender, tc.text)
			h.waitFor("final reaction", func() bool {
				return gocmp.Equal(h.gh.reactionsOn(gate.CommentKindIssue, 100), []gate.Reaction{tc.want})
			})
		})
	}
}
