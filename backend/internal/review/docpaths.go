package review

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// IndexPath is the docs index every repo's docs tree has; IndexHeading is the
// section of it that lists the docs, where Apply adds a new doc's entry.
const (
	IndexPath    = "docs/README.md"
	IndexHeading = "## Index"
)

// ErrFileTooLarge is returned by a FileAtRef implementation when the file exists
// but exceeds the size it will read, so callers can tell it apart from a missing
// file.
var ErrFileTooLarge = errors.New("file too large to read")

// NormalizeSection is the canonical form of a proposal's section heading: no
// leading #s, no surrounding space. Proposal identity depends on it.
func NormalizeSection(section string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(section), "#"))
}

// Truncate cuts s to at most max bytes at a rune boundary and appends "..."
// when it cuts.
func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max] + "..."
}
