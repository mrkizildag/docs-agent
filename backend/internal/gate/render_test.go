package gate_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestProposalCommentBodies(t *testing.T) {
	t.Parallel()

	gh := &fakeGitHub{changed: []review.ChangedFile{{Path: "docs/s.md", Hunks: []review.LineRange{{Start: 1, End: 10}}}}}
	checkbox := review.Proposal{
		DocPath: "docs/a.md", Section: "Usage", Reason: "flag renamed", Anchor: review.Anchor{File: "a.go", Line: 4},
		Original: "## Usage\nold\n", Lines: review.LineRange{Start: 3, End: 4}, Content: "## Usage\nnew\n",
	}
	newDoc := review.Proposal{
		DocPath: "docs/b.md", Reason: "new feature", Anchor: review.Anchor{File: "b.go", Line: 9},
		Content: "# B\nbody\n", IndexEntry: "- [B](b.md)",
	}
	suggest := review.Proposal{
		DocPath: "docs/s.md", Section: "Run", Reason: "port changed", Anchor: review.Anchor{File: "c.go", Line: 2},
		Original: "## Run\nport 1\n", Lines: review.LineRange{Start: 5, End: 6}, Content: "## Run\nport 2\n",
	}
	proposalService(t, gh, &fakeStore{}, review.Proposals{checkbox, newDoc, suggest})

	if len(gh.reviewComments) != 3 || gh.createIssue != 1 {
		t.Fatalf("review comments = %d, summaries = %d, want 3 and 1", len(gh.reviewComments), gh.createIssue)
	}

	cb := gh.reviewComments[0]
	if cb.Path != "a.go" || cb.Line != 4 || cb.CommitSHA != "abc123" {
		t.Errorf("checkbox comment anchored at %s:%d on %s, want a.go:4 on abc123", cb.Path, cb.Line, cb.CommitSHA)
	}
	for _, want := range []string{"flag renamed", "`docs/a.md`", `"Usage"`, "```diff\n-## Usage\n-old\n+## Usage\n+new\n```", "- [ ] Apply this change"} {
		if !strings.Contains(cb.Body, want) {
			t.Errorf("checkbox body missing %q:\n%s", want, cb.Body)
		}
	}

	nd := gh.reviewComments[1]
	if nd.Path != "b.go" || nd.Line != 9 {
		t.Errorf("new doc comment anchored at %s:%d, want b.go:9", nd.Path, nd.Line)
	}
	for _, want := range []string{"new feature", "`docs/b.md`", "```diff\n+# B\n+body\n```", "- [B](b.md)", "- [ ] Apply this change"} {
		if !strings.Contains(nd.Body, want) {
			t.Errorf("new doc body missing %q:\n%s", want, nd.Body)
		}
	}
	if strings.Contains(nd.Body, "```diff\n-") {
		t.Errorf("new doc body has removed lines:\n%s", nd.Body)
	}

	sg := gh.reviewComments[2]
	if sg.Path != "docs/s.md" || sg.StartLine != 5 || sg.Line != 6 {
		t.Errorf("suggestion anchored at %s:%d-%d, want docs/s.md:5-6", sg.Path, sg.StartLine, sg.Line)
	}
	if !strings.Contains(sg.Body, "port changed") || !strings.Contains(sg.Body, "```suggestion\n## Run\nport 2\n```") || strings.Contains(sg.Body, "[ ]") {
		t.Errorf("suggestion body = %q, want reason, suggestion block, no checkbox", sg.Body)
	}

	summary := gh.comments[len(gh.comments)-1].Body
	for i, want := range []string{"| `docs/a.md` | Usage |", "| `docs/b.md` | (new doc) |", "| `docs/s.md` | Run |"} {
		row := want + " [view](" + gh.comments[i].URL + ") | open |"
		if !strings.Contains(summary, row) {
			t.Errorf("summary missing row %q:\n%s", row, summary)
		}
	}
}
