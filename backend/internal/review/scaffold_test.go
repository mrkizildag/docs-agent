package review_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestScaffoldFiles(t *testing.T) {
	t.Parallel()

	got := review.Scaffold{ScaffoldDocs: review.ScaffoldDocs{Index: "i", Architecture: "a", Setup: "s"}}.Files()
	want := []review.ScaffoldFile{
		{Path: "docs/README.md", Content: "i"},
		{Path: "docs/architecture.md", Content: "a"},
		{Path: "docs/guides/setup.md", Content: "s"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Files() = %v, want %v", got, want)
	}
}

func TestScaffoldSchemaNamesIndexHeading(t *testing.T) {
	t.Parallel()

	schema, err := review.ScaffoldSchema()
	if err != nil {
		t.Fatalf("ScaffoldSchema() error = %v", err)
	}
	if !strings.Contains(string(schema), review.IndexHeading) {
		t.Errorf("scaffold schema does not mention %q", review.IndexHeading)
	}
}
