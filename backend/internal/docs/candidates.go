package docs

import (
	"slices"
	"strings"
)

// Change is one file of a pull request's diff.
type Change struct {
	Path         string
	PreviousPath string // old path of a renamed or moved file; empty otherwise.
	Removed      bool   // the PR deletes the file.
}

// Candidates returns the docs of t, the base tree, whose covers match the
// changes, in path order. A doc the PR renamed is reported at its new path;
// a doc the PR removes, or renames out of docs/ or to a non-doc path, is
// reported in deleted instead.
func (t Tree) Candidates(changes []Change) (candidates, deleted []string) {
	changed := make([]string, 0, len(changes))
	renamedTo := map[string]string{}
	removed := map[string]bool{}
	for _, c := range changes {
		changed = append(changed, c.Path)
		if c.PreviousPath != "" {
			changed = append(changed, c.PreviousPath)
			renamedTo[c.PreviousPath] = c.Path
		}
		if c.Removed {
			removed[c.Path] = true
		}
	}

	for _, basePath := range t.Match(changed) {
		switch to, renamed := renamedTo[basePath]; {
		case renamed && isDocPath(to):
			candidates = append(candidates, to)
		case renamed:
			deleted = append(deleted, basePath)
		case removed[basePath]:
			deleted = append(deleted, basePath)
		default:
			candidates = append(candidates, basePath)
		}
	}
	slices.Sort(candidates)
	return slices.Compact(candidates), deleted
}

func isDocPath(p string) bool {
	return strings.HasPrefix(p, "docs/") && isDocFile(p)
}
