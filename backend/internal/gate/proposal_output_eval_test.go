package gate_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestRerunWithRespelledHeadingEditsInPlace(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	first := proposal("docs/a.md", "Usage")
	again := proposal("docs/a.md", "## Usage ")
	again.Content, again.Reason = first.Content, "reworded reason"

	proposalService(t, gh, store, review.Proposals{first})
	proposalService(t, gh, store, review.Proposals{again})
	if gh.createReview != 1 || gh.createIssue != 1 || len(gh.comments) != 2 {
		t.Fatalf("creates %d/%d comments %d, want 1/1 2", gh.createReview, gh.createIssue, len(gh.comments))
	}
	if !strings.Contains(gh.comments[1].Body, "reworded reason") {
		t.Errorf("review comment not edited to new reason:\n%s", gh.comments[1].Body)
	}
}

func TestOutdatedProposalThatReturnsGetsANewComment(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	a, b := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")

	proposalService(t, gh, store, review.Proposals{a, b})
	proposalService(t, gh, store, review.Proposals{a})
	if !strings.HasPrefix(gh.comments[2].Body, "<!-- pollux-agent:superseded:") || !slices.Contains(gh.resolved, int64(3)) {
		t.Fatalf("b not retired after dropping it (resolved %v):\n%s", gh.resolved, gh.comments[2].Body)
	}
	proposalService(t, gh, store, review.Proposals{a, b})
	if len(gh.comments) != 4 || gh.createReview != 3 {
		t.Fatalf("comments %d creates %d, want 4 and 3", len(gh.comments), gh.createReview)
	}
	if !strings.HasPrefix(gh.comments[3].Body, "<!-- pollux-agent:proposal:") {
		t.Errorf("returned proposal's new comment lacks the live marker:\n%s", gh.comments[3].Body)
	}
	summary := gh.comments[0].Body
	if strings.Contains(summary, "outdated") || strings.Count(summary, "| open |") != 2 {
		t.Errorf("summary should show two open rows:\n%s", summary)
	}
}

func TestChangedProposalRetiresItsOldCommentBeforeSavingWithoutIt(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{onSave: func() { gh.ops = append(gh.ops, "save") }}
	changed := proposal("docs/a.md", "A")
	changed.Content = "## A\nnewer\n"

	proposalService(t, gh, store, review.Proposals{proposal("docs/a.md", "A")})
	gh.ops = nil
	proposalService(t, gh, store, review.Proposals{changed})

	resolve := slices.Index(gh.ops, "resolve-review")
	if resolve < 0 {
		t.Fatalf("ops = %v, want the old thread resolved", gh.ops)
	}
	after := gh.ops[resolve:]
	if save, create := slices.Index(after, "save"), slices.Index(after, "create-review"); save < 0 || create < save {
		t.Errorf("ops = %v, want resolve-review before the pre-create save and the create", gh.ops)
	}
	if !strings.HasPrefix(gh.comments[1].Body, "<!-- pollux-agent:superseded:") {
		t.Errorf("old comment = %q, want its marker swapped", gh.comments[1].Body)
	}
}
