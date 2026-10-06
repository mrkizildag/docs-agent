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
	} {
		if got := review.NormalizeSection(in); got != want {
			t.Errorf("NormalizeSection(%q) = %q, want %q", in, got, want)
		}
	}
}
