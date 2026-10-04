package gate_test

import (
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
	again.Reason = "reworded reason"

	proposalService(t, gh, store, review.Proposals{first})
	proposalService(t, gh, store, review.Proposals{again})
	if gh.createReview != 1 || gh.createIssue != 1 || len(gh.comments) != 2 {
		t.Fatalf("creates %d/%d comments %d, want 1/1 2", gh.createReview, gh.createIssue, len(gh.comments))
	}
	if !strings.Contains(gh.comments[0].Body, "reworded reason") {
		t.Errorf("review comment not edited to new reason:\n%s", gh.comments[0].Body)
	}
}

func TestOutdatedProposalThatReturnsReopensItsComment(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{}
	store := &fakeStore{}
	a, b := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")

	proposalService(t, gh, store, review.Proposals{a, b})
	proposalService(t, gh, store, review.Proposals{a})
	if !strings.Contains(gh.comments[1].Body, "Outdated") {
		t.Fatalf("b not outdated after dropping it:\n%s", gh.comments[1].Body)
	}
	proposalService(t, gh, store, review.Proposals{a, b})
	if len(gh.comments) != 3 || gh.createReview != 2 {
		t.Fatalf("comments %d creates %d, want 3 and 2", len(gh.comments), gh.createReview)
	}
	if strings.Contains(gh.comments[1].Body, "Outdated") {
		t.Errorf("returned proposal still outdated:\n%s", gh.comments[1].Body)
	}
	summary := gh.comments[2].Body
	if strings.Contains(summary, "outdated") || strings.Count(summary, "| open |") != 2 {
		t.Errorf("summary should show two open rows:\n%s", summary)
	}
}
