package e2e_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func TestWebhookSkipCommitAsksForReasonThenSkipsOnlyThatCommit(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals(), twoProposals())
	h.push("sha1")

	h.tickSummary("dev", "User", "Skip this commit")
	state := h.waitState("pending skip", func(s gate.PRState) bool { return s.PendingSkip != nil })
	if want := (gate.SkipAsk{User: "dev", Scope: gate.SkipCommit}); *state.PendingSkip != want {
		t.Errorf("pending skip = %+v, want %+v", *state.PendingSkip, want)
	}
	comments := h.gh.Comments()
	if ask := comments[len(comments)-1].Body; !strings.Contains(ask, "@dev") || !strings.Contains(ask, "reason") {
		t.Errorf("bot's question = %q, want it to ask @dev for a reason", ask)
	}
	if got := h.lastRun(); got.Conclusion == gate.ConclusionSuccess {
		t.Fatalf("check run succeeded before the reason arrived: %+v", got)
	}

	h.command("dev", "docs are generated for this one")
	state = h.waitState("skip recorded", func(s gate.PRState) bool { return s.Skip != nil })
	if want := (gate.Skip{User: "dev", Scope: gate.SkipCommit, Reason: "docs are generated for this one", HeadSHA: "sha1"}); *state.Skip != want || state.PendingSkip != nil {
		t.Errorf("skip = %+v, pending = %+v, want %+v and no pending ask", *state.Skip, state.PendingSkip, want)
	}
	waitFor(t, "the skip's check run", func() bool { return h.lastRun().Conclusion == gate.ConclusionSuccess })
	run := h.lastRun()
	if run.HeadSHA != "sha1" || !strings.Contains(run.Title, "dev") || !strings.Contains(run.Summary, "commit") || !strings.Contains(run.Summary, "docs are generated for this one") {
		t.Errorf("skip check run = %+v, want success naming dev, commit scope and the reason", run)
	}
	if summary := h.commentWith(summaryMarker).Body; !strings.Contains(summary, "Skipped by @dev for this commit: docs are generated for this one") {
		t.Errorf("summary does not show the skip:\n%s", summary)
	}

	next, _ := h.push("sha2")
	if next.Conclusion != gate.ConclusionActionRequired || next.HeadSHA != "sha2" {
		t.Errorf("check run for the next push = %s on %s, want action_required on sha2", next.Conclusion, next.HeadSHA)
	}
}

func TestWebhookSkipPRCarriesOverToLaterPushes(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals(), twoProposals())
	h.push("sha1")

	h.command("dev", "/pollux-agent skip-pr vendored docs")
	h.waitState("skip recorded", func(s gate.PRState) bool { return s.Skip != nil })
	waitFor(t, "the skip's check run", func() bool { return h.lastRun().Conclusion == gate.ConclusionSuccess })
	run := h.lastRun()
	if !strings.Contains(run.Summary, "vendored docs") || !strings.Contains(run.Summary, "PR") {
		t.Errorf("skip-pr check run = %+v, want success naming the PR scope and the reason", run)
	}

	next, state := h.push("sha2")
	if next.Conclusion != gate.ConclusionSuccess || next.HeadSHA != "sha2" || !strings.Contains(next.Summary, "vendored docs") {
		t.Errorf("check run for the later push = %+v, want the skip's success on sha2", next)
	}
	if n := h.runner.remaining(); n != 1 {
		t.Errorf("%d scripted verdicts left, want 1: the later push must not call the runner", n)
	}
	if state.Skip == nil || state.Skip.Scope != gate.SkipPR {
		t.Errorf("skip after the later push = %+v, want it carried over", state.Skip)
	}
}

func TestWebhookSkipReasonOnlyFromAsker(t *testing.T) {
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
	waitFor(t, "the skip's check run", func() bool { return h.lastRun().Conclusion == gate.ConclusionSuccess })
	if run := h.lastRun(); !strings.Contains(run.Summary, "generated docs") {
		t.Errorf("check run = %+v, want the skip's success", run)
	}
}

func TestWebhookRerequestAfterSkipPRRunsNoAnalysis(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, twoProposals(), twoProposals())
	h.push("sha1")
	h.command("dev", "/pollux-agent skip-pr vendored docs")
	h.waitState("skip recorded", func(s gate.PRState) bool { return s.Skip != nil })
	waitFor(t, "the skip's check run", func() bool { return h.lastRun().Conclusion == gate.ConclusionSuccess })
	before := len(h.gh.CheckRuns())

	h.sys.deliver("check_run", checkRunRerequestedBody(t))

	waitFor(t, "the re-requested check run", func() bool { return len(h.gh.CheckRuns()) > before && h.lastRun().Status == gate.StatusCompleted })
	if run := h.lastRun(); run.Conclusion != gate.ConclusionSuccess || !strings.Contains(run.Summary, "vendored docs") {
		t.Errorf("re-requested check run = %+v, want the PR skip's success", run)
	}
	if n := h.runner.remaining(); n != 1 {
		t.Errorf("%d scripted verdicts left, want 1: a re-run under a PR skip must not call the runner", n)
	}
}
