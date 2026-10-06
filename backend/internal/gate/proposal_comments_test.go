package gate_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate/gatetest"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestRerunWithRespelledHeadingEditsInPlace(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	store := newStore(t)
	first := proposal("docs/a.md", "Usage")
	again := proposal("docs/a.md", "## Usage ")
	again.Reason = "reworded reason"

	proposalService(t, gh, store, review.Proposals{first})
	proposalService(t, gh, store, review.Proposals{again})
	comments := gh.Comments()
	if c := callsOf(gh); c.createReview != 1 || c.createIssue != 1 || len(comments) != 2 {
		t.Fatalf("creates %d/%d comments %d, want 1/1 2", c.createReview, c.createIssue, len(comments))
	}
	if !strings.Contains(comments[1].Body, "reworded reason") {
		t.Errorf("review comment not edited to new reason:\n%s", comments[1].Body)
	}
}

func TestOutdatedProposalThatReturnsReopensItsComment(t *testing.T) {
	t.Parallel()

	gh := &gatetest.GitHub{}
	store := newStore(t)
	a, b := proposal("docs/a.md", "A"), proposal("docs/b.md", "B")

	proposalService(t, gh, store, review.Proposals{a, b})
	proposalService(t, gh, store, review.Proposals{a})
	if c := gh.Comments(); !strings.Contains(c[2].Body, "Outdated") {
		t.Fatalf("b not outdated after dropping it:\n%s", c[2].Body)
	}
	proposalService(t, gh, store, review.Proposals{a, b})
	comments := gh.Comments()
	if creates := callsOf(gh).createReview; len(comments) != 3 || creates != 2 {
		t.Fatalf("comments %d creates %d, want 3 and 2", len(comments), creates)
	}
	if strings.Contains(comments[2].Body, "Outdated") {
		t.Errorf("returned proposal still outdated:\n%s", comments[2].Body)
	}
	summary := comments[0].Body
	if strings.Contains(summary, "outdated") || strings.Count(summary, "| open |") != 2 {
		t.Errorf("summary should show two open rows:\n%s", summary)
	}
}
