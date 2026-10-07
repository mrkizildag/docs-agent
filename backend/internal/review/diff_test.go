package review_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestNumberedPatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		patch string
		want  string
	}{
		{
			name:  "context added and removed",
			patch: "@@ -1,3 +1,3 @@ func f()\n a\n-b\n+c\n d",
			want:  "@@ -1,3 +1,3 @@ func f()\n     1  a\n       -b\n     2 +c\n     3  d",
		},
		{
			name:  "multiple hunks restart numbering",
			patch: "@@ -1,2 +1,2 @@\n a\n+b\n@@ -20,2 +30,3 @@\n x\n+y\n z",
			want:  "@@ -1,2 +1,2 @@\n     1  a\n     2 +b\n@@ -20,2 +30,3 @@\n    30  x\n    31 +y\n    32  z",
		},
		{
			name:  "removed-only hunk",
			patch: "@@ -5,2 +4,0 @@\n-a\n-b",
			want:  "@@ -5,2 +4,0 @@\n       -a\n       -b",
		},
		{
			name:  "no newline marker",
			patch: "@@ -1 +1 @@\n-a\n\\ No newline at end of file\n+b\n\\ No newline at end of file",
			want:  "@@ -1 +1 @@\n       -a\n\\ No newline at end of file\n     1 +b\n\\ No newline at end of file",
		},
		{
			name:  "header without counts and a blank context line",
			patch: "@@ -1 +1 @@\n \n+b",
			want:  "@@ -1 +1 @@\n     1  \n     2 +b",
		},
		{
			name:  "wide numbers",
			patch: "@@ -1000,1 +1000,1 @@\n+a",
			want:  "@@ -1000,1 +1000,1 @@\n  1000 +a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := review.NumberedPatch(tc.patch); got != tc.want {
				t.Errorf("NumberedPatch(%q) = %q, want %q", tc.patch, got, tc.want)
			}
		})
	}
}
