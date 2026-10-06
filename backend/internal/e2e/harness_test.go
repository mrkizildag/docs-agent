package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite/sqlitetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const (
	summaryMarker = "<!-- pollux-agent:summary -->"
	appliedSHA    = "commit1234567890"
)

// scriptedRunner plays one queued outcome per run: a review.Verdict is a
// finished analysis, a review.Pending is an external run, an error is a failed
// one. Collect never finds a result, which is what a failed workflow run leaves.
type scriptedRunner struct{ outcomes chan any }

func newScriptedRunner(outcomes ...any) scriptedRunner {
	queued := make(chan any, len(outcomes))
	for _, o := range outcomes {
		queued <- o
	}
	return scriptedRunner{outcomes: queued}
}

// remaining is how many outcomes no run has taken yet.
func (r scriptedRunner) remaining() int { return len(r.outcomes) }

// blockedRun is an outcome that holds the analysis until its context is
// cancelled, then fails the way an interrupted server analysis does.
type blockedRun struct{ started chan struct{} }

// heldRun is an outcome that holds the analysis until release is closed, then
// finishes with no impact; a cancelled context fails it like blockedRun.
type heldRun struct{ started, release chan struct{} }

func (r scriptedRunner) Start(ctx context.Context, _ review.Request) (review.Started, error) {
	switch o := (<-r.outcomes).(type) {
	case blockedRun:
		close(o.started)
		<-ctx.Done()
		return nil, &review.FailedError{Cause: review.CauseTimeout, Err: ctx.Err()}
	case heldRun:
		close(o.started)
		select {
		case <-o.release:
			return review.Result{Runner: "fake", Verdict: review.NoImpact{Reason: "held run done"}}, nil
		case <-ctx.Done():
			return nil, &review.FailedError{Cause: review.CauseTimeout, Err: ctx.Err()}
		}
	case review.Verdict:
		return review.Result{Runner: "fake", Verdict: o}, nil
	case review.Pending:
		return o, nil
	case error:
		return nil, o
	default:
		return nil, fmt.Errorf("scriptedRunner: unsupported outcome %T", o)
	}
}

func (scriptedRunner) Collect(context.Context, review.Completion) (review.Result, error) {
	return review.Result{}, errors.New("scriptedRunner: no result artifact")
}

func (scriptedRunner) StartScaffold(context.Context, review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	return nil, errors.New("scriptedRunner does not scaffold")
}

func (scriptedRunner) CollectScaffold(context.Context, review.Completion) (review.Scaffold, error) {
	return review.Scaffold{}, errors.New("scriptedRunner does not scaffold")
}

func twoProposals() review.Proposals {
	return review.Proposals{
		{DocPath: "docs/a.md", Section: "Usage", Anchor: review.Anchor{File: "a.go", Line: 4}, Reason: "flag renamed", Original: "## Usage\nold\n", Lines: review.LineRange{Start: 3, End: 4}, Content: "## Usage\nnew\n"},
		{DocPath: "docs/b.md", Anchor: review.Anchor{File: "b.go", Line: 9}, Reason: "new feature", Content: "# B\n", IndexEntry: "- [B](b.md)"},
	}
}

func proposalMarker(id string) string { return "<!-- pollux-agent:proposal:" + id + " -->" }

func appliedReply(sha, id string) string {
	return "✅ Applied in " + sha[:7] + "\n\n<!-- pollux-agent:applied:" + id + ":" + sha + " -->"
}

// repoGitHub is a GitHub with docs/, a pull request of dev's from branch
// feature, and write access for everyone; commits get the SHA appliedSHA.
func repoGitHub() *gatetest.GitHub {
	return &gatetest.GitHub{
		PullRequest:  gate.PullRequest{HeadRef: "feature", Open: true},
		MergeBaseSHA: "base1",
		CanWrite:     true,
		CommitSHA:    appliedSHA,
		Files:        map[string]string{},
	}
}

// pushHarness drives webhooks for PR 1 through the real handler, worker and
// SQLite store, with scripted analysis outcomes and a stateful fake GitHub.
type pushHarness struct {
	t      *testing.T
	gh     *gatetest.GitHub
	store  *sqlite.Store
	loadPR func() gate.PRState
	sys    *system
	runner scriptedRunner
}

func newPushHarness(t *testing.T, outcomes ...any) *pushHarness {
	t.Helper()

	return newHarness(t, repoGitHub(), outcomes...)
}

func newHarness(t *testing.T, gh *gatetest.GitHub, outcomes ...any) *pushHarness {
	t.Helper()

	store := sqlitetest.Open(t)
	runner := newScriptedRunner(outcomes...)
	h := &pushHarness{t: t, gh: gh, store: store, runner: runner}
	h.loadPR = func() gate.PRState {
		t.Helper()

		state, err := store.LoadPR(t.Context(), "acme", "widgets", 1)
		if err != nil {
			t.Fatalf("LoadPR() error = %v", err)
		}
		return state
	}
	h.sys = start(t, store, gh, gate.Runners{Actions: runner, Server: runner})
	return h
}

// push delivers a synchronize webhook for sha and returns the check run and the
// saved state once the run has finished.
func (h *pushHarness) push(sha string) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	return h.pushWith(sha, pushOpts{})
}

func (h *pushHarness) pushWith(sha string, o pushOpts) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	h.sendWith(sha, o)
	return h.waitConcluded(sha)
}

// send delivers a synchronize webhook for sha without waiting for the run.
func (h *pushHarness) send(sha string) {
	h.t.Helper()

	h.sendWith(sha, pushOpts{})
}

func (h *pushHarness) sendWith(sha string, o pushOpts) {
	h.t.Helper()

	h.moveHead(sha)
	o.action = "synchronize"
	h.sys.deliver("pull_request", pullRequestBody(h.t, 1, sha, o))
}

// moveHead makes GitHub report sha as the pull request's head.
func (h *pushHarness) moveHead(sha string) {
	h.gh.SetPullRequest(gate.PullRequest{HeadSHA: sha, HeadRef: "feature", Open: true})
}

// waitConcluded waits until the check run of sha is concluded and the state
// records its comments, and returns that check run and state.
func (h *pushHarness) waitConcluded(sha string) (gate.CheckRun, gate.PRState) {
	h.t.Helper()

	var run gate.CheckRun
	var state gate.PRState
	waitFor(h.t, "the run on "+sha+" to conclude", func() bool {
		state = h.loadPR()
		if state.HeadSHA != sha || state.Run != nil || !h.commentIDsSaved(state) {
			return false
		}
		cr, ok := h.gh.CheckRun(state.CheckRunID)
		run = cr.Latest()
		return ok && cr.Created.HeadSHA == sha && run.Status == gate.StatusCompleted
	})
	return run, state
}

// lastRun is the check run GitHub shows for the newest check run created.
func (h *pushHarness) lastRun() gate.CheckRun {
	h.t.Helper()

	runs := h.gh.CheckRuns()
	if len(runs) == 0 {
		h.t.Fatal("no check run created yet")
	}
	return runs[len(runs)-1].Latest()
}

// commentIDsSaved reports whether state already records every comment written
// so far: gate saves the concluded state before it writes comments.
func (h *pushHarness) commentIDsSaved(state gate.PRState) bool {
	if len(h.gh.Comments()) > 0 && state.SummaryCommentID == 0 {
		return false
	}
	return !slices.ContainsFunc(state.Proposals, func(p gate.ProposalState) bool { return p.CommentID == 0 })
}

func (h *pushHarness) commentWith(marker string) gate.Comment {
	h.t.Helper()

	comments := h.gh.Comments()
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return c
		}
	}
	h.t.Fatalf("no comment contains %q in %+v", marker, comments)
	return gate.Comment{}
}

// edits is how many comment edits GitHub has taken.
func (h *pushHarness) edits() int {
	return h.gh.CallCount("EditIssueComment") + h.gh.CallCount("EditReviewComment")
}

func (h *pushHarness) waitState(what string, done func(gate.PRState) bool) gate.PRState {
	h.t.Helper()

	var state gate.PRState
	waitFor(h.t, what, func() bool {
		state = h.loadPR()
		return done(state)
	})
	return state
}

// waitReply waits until the bot has posted one more comment than before.
func (h *pushHarness) waitReply(before int) gate.Comment {
	h.t.Helper()

	waitFor(h.t, "a reply", func() bool { return len(h.gh.Comments()) > before })
	comments := h.gh.Comments()
	return comments[len(comments)-1]
}

// tick delivers a pull_request_review_comment edited webhook in which sender
// ticks the checkbox of comment id.
func (h *pushHarness) tick(sender string, id int64, unticked string) {
	h.t.Helper()

	h.sys.deliver("pull_request_review_comment", marshal(h.t, map[string]any{
		"action":       "edited",
		"changes":      map[string]any{"body": map[string]any{"from": unticked}},
		"comment":      map[string]any{"id": id, "body": strings.Replace(unticked, "- [ ]", "- [x]", 1)},
		"pull_request": map[string]any{"number": 1},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 42},
		"sender":       map[string]any{"login": sender, "type": "User"},
	}))
}

// issueComment delivers an issue_comment webhook; from is the previous body of an edit.
func (h *pushHarness) issueComment(action, sender, senderType string, id int64, body, from string) {
	h.t.Helper()

	payload := map[string]any{
		"action":       action,
		"comment":      map[string]any{"id": id, "body": body},
		"issue":        map[string]any{"number": 1, "pull_request": map[string]any{}},
		"sender":       map[string]any{"login": sender, "type": senderType},
		"installation": map[string]any{"id": 42},
		"repository":   map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}},
	}
	if from != "" {
		payload["changes"] = map[string]any{"body": map[string]any{"from": from}}
	}
	h.sys.deliver("issue_comment", marshal(h.t, payload))
}

func (h *pushHarness) command(sender, text string) {
	h.t.Helper()

	h.issueComment("created", sender, "User", 100, text, "")
}

// tickSummary delivers the edit in which sender ticks label on the summary comment.
func (h *pushHarness) tickSummary(sender, senderType, label string) {
	h.t.Helper()

	summary := h.commentWith(summaryMarker)
	h.issueComment("edited", sender, senderType, summary.ID, strings.Replace(summary.Body, "- [ ] "+label, "- [x] "+label, 1), summary.Body)
}

func (h *pushHarness) allReplied(state gate.PRState) bool {
	for _, p := range state.Proposals {
		if p.State == gate.ProposalApplied && p.ReplyID == 0 {
			return false
		}
	}
	return slices.ContainsFunc(state.Proposals, func(p gate.ProposalState) bool { return p.State == gate.ProposalApplied })
}

// newApplyHarness is a harness whose first push, sha1, proposed twoProposals
// against a docs/a.md that can take the first one.
func newApplyHarness(t *testing.T, extra ...any) *pushHarness {
	t.Helper()

	return newApplyHarnessOn(t, repoGitHub(), extra...)
}

// newApplyHarnessOn is newApplyHarness over a configured GitHub.
func newApplyHarnessOn(t *testing.T, gh *gatetest.GitHub, extra ...any) *pushHarness {
	t.Helper()

	gh.Files["docs/a.md"] = "# A\n\n## Usage\nold\n\n## Other\nx\n"
	h := newHarness(t, gh, append([]any{twoProposals()}, extra...)...)
	h.push("sha1")
	return h
}

// requireAppliedAll checks that one commit applied both proposals and every
// surface shows it.
func (h *pushHarness) requireAppliedAll() {
	h.t.Helper()

	state := h.waitState("both proposals applied with replies", h.allReplied)
	wantCommits := []gatetest.Commit{{
		Branch: "feature", Parent: "sha1", Message: "docs: apply 2 pollux-agent proposals", SHA: appliedSHA,
		Files: []gate.FileChange{
			{Path: "docs/a.md", Content: "# A\n\n## Usage\nnew\n\n## Other\nx\n"},
			{Path: "docs/b.md", Content: "# B\n"},
			{Path: "docs/README.md", Content: "- [B](b.md)\n"},
		},
	}}
	wantReplies := []gatetest.Reply{
		{To: 2, Body: appliedReply(appliedSHA, gate.ProposalID("docs/a.md", "Usage"))},
		{To: 3, Body: appliedReply(appliedSHA, gate.ProposalID("docs/b.md", ""))},
	}
	if diff := gocmp.Diff(wantCommits, h.gh.Committed()); diff != "" {
		h.t.Errorf("commits (-want +got):\n%s", diff)
	}
	if diff := gocmp.Diff(wantReplies, h.gh.Replies()); diff != "" {
		h.t.Errorf("replies (-want +got):\n%s", diff)
	}

	for _, p := range state.Proposals {
		if p.State != gate.ProposalApplied || p.AppliedSHA != appliedSHA {
			h.t.Errorf("proposal %s = %s at %q, want applied at %s", p.ID, p.State, p.AppliedSHA, appliedSHA)
		}
		if body := h.commentWith(proposalMarker(p.ID)).Body; !strings.Contains(body, "- [x] Apply this change") {
			h.t.Errorf("proposal comment not ticked:\n%s", body)
		}
	}
	summary := h.commentWith(summaryMarker).Body
	if strings.Count(summary, "applied (commit1)") != 2 || !strings.Contains(summary, "✅ All proposals applied.") || strings.Contains(summary, "Apply all") {
		h.t.Errorf("summary should show both proposals applied and the all-applied status line:\n%s", summary)
	}
}
