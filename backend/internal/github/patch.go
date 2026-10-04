package github

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// headSideRanges returns the head-side line ranges of the hunks in a unified
// diff patch. A hunk that adds or keeps no lines (count 0) yields no range.
func headSideRanges(patch string) []review.LineRange {
	var ranges []review.LineRange
	for line := range strings.Lines(patch) {
		m := hunkHeader.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// The regexp admits digits only, so the conversions cannot fail short of overflow.
		start, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		count := 1
		if m[2] != "" {
			if count, err = strconv.Atoi(m[2]); err != nil {
				continue
			}
		}
		if count == 0 {
			continue
		}
		ranges = append(ranges, review.LineRange{Start: start, End: start + count - 1})
	}
	return ranges
}
