package review_test

import (
	"slices"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestScaffoldFiles(t *testing.T) {
	t.Parallel()

	got := review.Scaffold{Index: "i", Architecture: "a", Setup: "s"}.Files()
	want := []review.ScaffoldFile{
		{Path: "docs/README.md", Content: "i"},
		{Path: "docs/architecture.md", Content: "a"},
		{Path: "docs/guides/setup.md", Content: "s"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Files() = %v, want %v", got, want)
	}
}
