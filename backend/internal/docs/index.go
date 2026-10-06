package docs

import (
	"regexp"
	"strings"
)

var linkTarget = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// IndexEntry returns the list item of readme, the docs index, that links to
// docPath, in the form a proposal's index entry takes ("- [Title](rel.md): ...").
// README links are relative to docs/.
func IndexEntry(readme []byte, docPath string) (string, bool) {
	rel, ok := strings.CutPrefix(docPath, "docs/")
	if !ok {
		return "", false
	}
	for line := range strings.SplitSeq(string(readme), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		for _, m := range linkTarget.FindAllStringSubmatch(line, -1) {
			target, _, _ := strings.Cut(m[1], "#")
			if strings.TrimPrefix(target, "./") == rel {
				return line, true
			}
		}
	}
	return "", false
}
