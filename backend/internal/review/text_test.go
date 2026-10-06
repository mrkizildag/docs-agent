package review_test

import (
	"testing"
	"unicode/utf8"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in     string
		max    int
		marker string
		want   string
	}{
		{"short", 10, "...", "short"},
		{"exact", 5, "...", "exact"},
		{"abcdefgh", 6, "...", "abc..."},
		{"aébcd", 5, "...", "a..."},
		{"日本語日本語", 7, "...", "日..."},
		{"abcdefgh", 2, "...", ".."},
	} {
		got := review.Truncate(tc.in, tc.max, tc.marker)
		if got != tc.want {
			t.Errorf("Truncate(%q, %d, %q) = %q, want %q", tc.in, tc.max, tc.marker, got, tc.want)
		}
		if len(got) > max(tc.max, 0) || !utf8.ValidString(got) {
			t.Errorf("Truncate(%q, %d, %q) = %q: over the bound or invalid UTF-8", tc.in, tc.max, tc.marker, got)
		}
	}
}
