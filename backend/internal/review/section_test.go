package review_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestNormalizeSection(t *testing.T) {
	for in, want := range map[string]string{
		"Setup":         "Setup",
		"## Setup":      "Setup",
		"  ### Setup  ": "Setup",
		"#":             "",
		"####### Setup": "####### Setup",
		"#Setup":        "#Setup",
		"#channels":     "#channels",
		"## #channels":  "#channels",
		"##\t#channels": "#channels",
	} {
		if got := review.NormalizeSection(in); got != want {
			t.Errorf("NormalizeSection(%q) = %q, want %q", in, got, want)
		}
	}
}

// Sections are normalized again downstream (SectionSpan, ProposalID), so a
// second pass must not change the result.
func TestNormalizeSectionIsIdempotent(t *testing.T) {
	for _, in := range []string{"## # Foo", "# Foo", "## #channels", "### ## Setup", "Setup", "#", "####### Setup"} {
		once := review.NormalizeSection(in)
		if twice := review.NormalizeSection(once); twice != once {
			t.Errorf("NormalizeSection(NormalizeSection(%q)) = %q, want %q", in, twice, once)
		}
	}
}
