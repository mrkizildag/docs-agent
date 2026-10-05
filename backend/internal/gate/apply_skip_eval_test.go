package gate_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

// Criterion 12 edge: an Apply all redelivered after its commit landed but
// before the state was saved commits nothing new, and still marks, ticks, and
// replies to every proposal exactly once.
func TestEvalApplyAllRedeliveredAfterCrashCommitsOnce(t *testing.T) {
	t.Parallel()

	const msg = "docs: apply 3 pollux-agent proposals"
	state := threeState()
	state.PendingApply = &gate.PendingApply{IDs: []string{"p1", "p2", "p3"}, Message: msg, Parent: "head1"}
	store := &fakeStore{stored: state, live: true}
	api := apiWithComments()
	api.pullRequest = gate.PullRequest{HeadSHA: "botall1234", Open: true}
	landed := gate.Commit{SHA: "botall1234", Parents: []string{"head1"}, Message: msg, Mine: true}
	comments := &fakeCommentGitHub{canWrite: true, files: baseFiles(), api: api, byAdd: map[string]gate.Commit{"botall1234": landed}}
	svc := gate.NewService(api, comments, store, gate.Runners{}, nil, nil)

	for range 2 {
		if err := svc.HandleComment(t.Context(), issueComment("/pollux-agent apply")); err != nil {
			t.Fatalf("HandleComment() = %v", err)
		}
	}

	if len(comments.commits) != 0 {
		t.Errorf("commits = %d, want 0 (the landed commit is adopted)", len(comments.commits))
	}
	for _, p := range store.stored.Proposals {
		if p.State != gate.ProposalApplied || p.AppliedSHA != "botall1234" {
			t.Errorf("proposal %s = %v at %q, want applied at botall1234", p.ID, p.State, p.AppliedSHA)
		}
	}
	if n := countReplies(comments.replies, "Applied in botall1"); n != 3 {
		t.Errorf("applied replies = %d, want 3; replies = %+v", n, comments.replies)
	}
	if n := countReplies(comments.replies, "nothing was committed"); n != 0 {
		t.Errorf("refusals = %d, want 0", n)
	}
	for _, id := range []int64{1, 2, 3} {
		for _, c := range api.comments {
			if c.ID == id && !contains(c.Body, "- [x] Apply this change") {
				t.Errorf("proposal comment %d not ticked: %q", id, c.Body)
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
