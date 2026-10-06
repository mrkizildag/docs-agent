package e2e_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// updatedIDs is the check run of every UpdateCheckRun call, in order.
func (h *pushHarness) updatedIDs() []int64 {
	var ids []int64
	for _, c := range h.gh.Calls() {
		if c.Method == "UpdateCheckRun" {
			ids = append(ids, c.ID)
		}
	}
	return ids
}

// waitRerun waits until the check run id is concluded and the PR's state is idle on it.
func (h *pushHarness) waitRerun(id int64) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	state := h.waitState(fmt.Sprintf("check run %d to finish", id), func(s gate.PRState) bool { return s.CheckRunID == id && s.Run == nil })
	var run gate.CheckRun
	waitFor(h.t, fmt.Sprintf("check run %d to conclude", id), func() bool {
		cr, ok := h.checkRun(id)
		run = cr.Latest()
		return ok && run.Status == gate.StatusCompleted
	})
	return run, state
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(waitTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestFailedAnalysisIsRerunFromSummaryCheckbox(t *testing.T) {
	t.Parallel()

	const (
		providerText = "provider said: leak-me"
		rerunBox     = "- [ ] Re-run analysis"
	)
	failure := &review.FailedError{Cause: review.CauseProvider, Err: errors.New(providerText)}
	h := newPushHarness(t, failure, review.NoImpact{Reason: "docs already match"})

	run, state := h.push("sha1")
	if run.Conclusion != gate.ConclusionNeutral || run.Title != "Analysis failed" || run.Summary != "The model provider returned an error." {
		t.Fatalf("failed check run = %+v, want neutral \"Analysis failed\" with the fixed provider cause", run)
	}
	if strings.Contains(run.Summary, "leak-me") {
		t.Errorf("check run summary leaks the provider text: %q", run.Summary)
	}
	summary := h.commentWith(summaryMarker)
	if !strings.Contains(summary.Body, "The model provider returned an error.") || !strings.Contains(summary.Body, rerunBox+"\n") || strings.Contains(summary.Body, "leak-me") {
		t.Errorf("summary after failure:\n%s\nwant the fixed cause and an unticked Re-run box", summary.Body)
	}
	if state.SummaryCommentID != summary.ID {
		t.Errorf("saved SummaryCommentID = %d, want %d", state.SummaryCommentID, summary.ID)
	}

	ticked := strings.Replace(summary.Body, rerunBox, "- [x] Re-run analysis", 1)
	h.issueComment("edited", "dev", "User", summary.ID, summary.Body+"\nedited", summary.Body)
	h.issueComment("edited", "pollux-agent[bot]", "Bot", summary.ID, ticked, summary.Body)
	h.issueComment("edited", "dev", "User", summary.ID+1, ticked, summary.Body)
	h.tickSummary("dev", "User", "Re-run analysis")

	run, _ = h.waitRerun(2)
	if run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("re-run check conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
	}

	if n := len(h.gh.Comments()); n != 1 {
		t.Errorf("comments = %d, want the one summary comment edited in place", n)
	}
	if body := h.commentWith(summaryMarker).Body; strings.Contains(body, "Analysis failed") || strings.Contains(body, rerunBox) {
		t.Errorf("summary after re-run:\n%s\nwant no failure cause and no Re-run box", body)
	}
	if ids := h.updatedIDs(); !slices.Equal(ids, []int64{1, 2}) {
		t.Errorf("concluded check runs = %v, want check runs 1 and 2 (the extra edits start nothing)", ids)
	}
}

func TestCheckRunRerequestedStartsFreshAnalysis(t *testing.T) {
	t.Parallel()

	h := newPushHarness(t, review.NoImpact{Reason: "docs already match"}, review.NoImpact{Reason: "docs already match"})

	if run, _ := h.push("sha1"); run.Conclusion != gate.ConclusionSuccess {
		t.Fatalf("first conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
	}

	h.sys.deliver("check_run", checkRunRerequestedBody(t))

	run, state := h.waitRerun(2)
	if run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("re-run conclusion = %q, want %q", run.Conclusion, gate.ConclusionSuccess)
	}
	if state.HeadSHA != "sha1" {
		t.Errorf("HeadSHA = %q, want sha1", state.HeadSHA)
	}
}

func TestRerunAndPushTogetherEndOnNewestHead(t *testing.T) {
	t.Parallel()

	noImpact := review.NoImpact{Reason: "docs already match"}
	h := newPushHarness(t, noImpact, noImpact, noImpact)
	h.push("sha1")

	h.moveHead("sha2")
	h.sys.deliver("check_run", checkRunRerequestedBody(t))
	h.sys.deliver("pull_request", pullRequestBody(t, 1, "sha2", pushOpts{action: "synchronize"}))

	var state gate.PRState
	h.waitState("the newest head to finish", func(s gate.PRState) bool {
		state = s
		cr, ok := h.checkRun(s.CheckRunID)
		ids := h.updatedIDs()
		return s.HeadSHA == "sha2" && s.Run == nil && s.CheckRunID > 1 && ok && cr.Latest().Status == gate.StatusCompleted && ids[len(ids)-1] == s.CheckRunID
	})

	ids := h.updatedIDs()
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("check run %d concluded twice: %v", id, ids)
		}
		seen[id] = true
	}
	if last := ids[len(ids)-1]; last != state.CheckRunID {
		t.Errorf("last concluded = %d, want the newest check run %d", last, state.CheckRunID)
	}
}

func TestActionsRerunFailingAgainEditsSummaryInPlace(t *testing.T) {
	t.Parallel()

	const (
		rerunBox = "- [ ] Re-run analysis"
		cause    = "The pollux-agent workflow run failed."
	)
	pending := func(runID int64) review.Pending {
		return review.Pending{RunID: runID, Nonce: fmt.Sprintf("n%d", runID), Deadline: time.Now().Add(time.Hour)}
	}
	gh := repoGitHub()
	gh.Workflow = true
	h := newHarness(t, gh, twoProposals(), pending(101), pending(102))

	h.push("sha1")

	failRun := func(runID, checkRunID int64) gate.CheckRun {
		t.Helper()

		h.waitState(fmt.Sprintf("run %d to be awaited", runID), func(s gate.PRState) bool { return s.Run != nil && s.Run.RunID == runID })
		h.sys.deliver("workflow_run", workflowRunBody(t, runID, "failure"))
		run, _ := h.waitRerun(checkRunID)
		return run
	}

	h.sys.deliver("check_run", checkRunRerequestedBody(t))
	failRun(101, 2)

	summary := h.commentWith(summaryMarker)
	if !strings.Contains(summary.Body, cause) || !strings.Contains(summary.Body, rerunBox+"\n") {
		t.Fatalf("summary after the first failure:\n%s\nwant the workflow-failure cause and an unticked Re-run box", summary.Body)
	}

	h.tickSummary("dev", "User", "Re-run analysis")
	run := failRun(102, 3)

	if run.Conclusion != gate.ConclusionNeutral || run.Title != "Analysis failed" || run.Summary != cause {
		t.Errorf("re-run check run = %+v, want neutral \"Analysis failed\" with summary %q", run, cause)
	}
	comments := h.gh.Comments()
	if len(comments) != 3 {
		t.Errorf("comments = %d, want 3 (two proposals and one summary edited in place)", len(comments))
	}
	summaries := 0
	for _, c := range comments {
		if strings.Contains(c.Body, summaryMarker) {
			summaries++
		}
	}
	if summaries != 1 {
		t.Errorf("summary comments = %d, want 1", summaries)
	}
	body := h.commentWith(summaryMarker).Body
	for _, want := range []string{cause, "`docs/a.md`", "`docs/b.md`", "| open |"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary after the second failure lacks %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, rerunBox+"\n") {
		t.Errorf("summary after the second failure:\n%s\nwant an unticked Re-run box", body)
	}
}

// A push that supersedes a running server analysis closes its check run as
// superseded; the interrupted analysis is not reported as a failure.
func TestSupersededServerAnalysisIsNotReportedFailed(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	h := newPushHarness(t, blockedRun{started: started}, review.NoImpact{Reason: "fine"})

	h.send("sha1")
	waitClosed(t, started, "the first analysis to start")
	h.push("sha2")

	first, ok := h.checkRun(1)
	if ids := h.updatedIDs(); !slices.Equal(ids, []int64{1, 2}) || !ok || first.Latest().Title != "Superseded" {
		t.Fatalf("updated check runs = %v, check run 1 = %+v, want check run 1 superseded, then check run 2", ids, first.Latest())
	}
	for _, c := range h.gh.Comments() {
		if strings.Contains(c.Body, "**Analysis failed:**") {
			t.Errorf("comment reports a failure:\n%s", c.Body)
		}
	}
}

func TestRerunQueuesBehindRunningAnalysisOfSameHead(t *testing.T) {
	t.Parallel()

	held := heldRun{started: make(chan struct{}), release: make(chan struct{})}
	h := newPushHarness(t, held, review.NoImpact{Reason: "docs already match"})

	h.send("sha1")
	waitClosed(t, held.started, "the first analysis to start")
	h.sys.deliver("check_run", checkRunRerequestedBody(t))
	close(held.release)

	run, state := h.waitRerun(2)
	if state.HeadSHA != "sha1" {
		t.Errorf("HeadSHA = %q, want sha1", state.HeadSHA)
	}
	if ids := h.updatedIDs(); !slices.Equal(ids, []int64{1, 2}) || run.Conclusion != gate.ConclusionSuccess {
		t.Errorf("updated check runs = %v, check run 2 = %+v, want check runs 1 then 2, both concluded", ids, run)
	}
}

func TestPushSupersedesQueuedRerun(t *testing.T) {
	t.Parallel()

	held := heldRun{started: make(chan struct{}), release: make(chan struct{})}
	h := newPushHarness(t, held, review.NoImpact{Reason: "docs already match"})

	h.send("sha1")
	waitClosed(t, held.started, "the first analysis to start")
	h.sys.deliver("check_run", checkRunRerequestedBody(t))
	h.send("sha2")

	state := h.waitState("the newest head to finish", func(s gate.PRState) bool { return s.HeadSHA == "sha2" && s.Run == nil && s.CheckRunID > 1 })
	var last gate.CheckRun
	waitFor(t, "the newest check run to conclude", func() bool {
		cr, ok := h.checkRun(state.CheckRunID)
		last = cr.Latest()
		return ok && last.Status == gate.StatusCompleted
	})
	if ids := h.updatedIDs(); ids[len(ids)-1] != state.CheckRunID || last.Conclusion != gate.ConclusionSuccess {
		t.Errorf("updated check runs = %v, newest = %+v, want the newest check run %d concluded last", ids, last, state.CheckRunID)
	}
}

func TestPushSupersedesQueuedRerunTick(t *testing.T) {
	t.Parallel()

	failure := &review.FailedError{Cause: review.CauseProvider, Err: errors.New("provider down")}
	held := heldRun{started: make(chan struct{}), release: make(chan struct{})}
	h := newPushHarness(t, failure, held, review.NoImpact{Reason: "docs already match"})

	h.push("sha1")
	h.send("sha2")
	waitClosed(t, held.started, "the second analysis to start")
	h.tickSummary("dev", "User", "Re-run analysis")
	h.send("sha3")

	state := h.waitState("the newest head to finish", func(s gate.PRState) bool { return s.HeadSHA == "sha3" && s.Run == nil && s.CheckRunID > 1 })
	if state.FailureCause != "" {
		t.Errorf("FailureCause = %q, want the newest head analyzed successfully", state.FailureCause)
	}
	if n := h.runner.remaining(); n != 0 {
		t.Errorf("unconsumed analysis outcomes = %d, want 0 (the Re-run tick must not have run an analysis)", n)
	}
}
