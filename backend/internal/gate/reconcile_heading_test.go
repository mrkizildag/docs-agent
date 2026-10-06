package gate_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

func TestHeadingLevelAgreesWithDocsParsing(t *testing.T) {
	t.Parallel()

	lines := []string{
		"# one", "## two", "###### six", "####### seven", "#nospace", "##\ttab", "#\ttab",
		"#", "###", "## closed ##", "##\tclosed\t##", "plain", "", "  ## indented", "#hashtag",
	}
	for _, line := range lines {
		want := 0
		for _, sec := range docs.ParseBody("docs/x.md", []byte(line+"\n")).Sections {
			if sec.Level > 0 {
				want = sec.Level
				break
			}
		}
		if got := gate.HeadingLevel(line); got != want {
			t.Errorf("HeadingLevel(%q) = %d, docs parses level %d", line, got, want)
		}
	}
}
