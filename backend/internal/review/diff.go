package review

import (
	"fmt"
	"strconv"
	"strings"
)

// NumberedPatch prints a file's patch with the head-side line number on every
// context and added line, so a reader can name a line GitHub accepts a review
// comment on. Removed lines carry no number: they have no head-side line.
func NumberedPatch(patch string) string {
	var b strings.Builder
	next := 0
	for i, line := range strings.Split(patch, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch {
		case strings.HasPrefix(line, "@@"):
			next = hunkHeadStart(line)
			b.WriteString(line)
		case strings.HasPrefix(line, "+"):
			fmt.Fprintf(&b, "%6d %s", next, line)
			next++
		case strings.HasPrefix(line, "-"):
			b.WriteString("       " + line)
		case strings.HasPrefix(line, " "):
			fmt.Fprintf(&b, "%6d  %s", next, line[1:])
			next++
		default:
			b.WriteString(line)
		}
	}
	return b.String()
}

// hunkHeadStart is the c of a "@@ -a,b +c,d @@" header, or 0 when it has none.
func hunkHeadStart(header string) int {
	_, rest, ok := strings.Cut(header, " +")
	if !ok {
		return 0
	}
	digits := rest
	if end := strings.IndexAny(rest, ", "); end >= 0 {
		digits = rest[:end]
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return n
}
