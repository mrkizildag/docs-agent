package review_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exact", 5, "exact"},
		{"abcdefgh", 3, "abc..."},
		{"aé", 2, "a..."},
		{"aé", 3, "aé"},
		{"日本語", 4, "日..."},
	} {
		if got := review.Truncate(tc.in, tc.max); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

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
