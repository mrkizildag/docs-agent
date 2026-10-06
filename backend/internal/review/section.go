package review

import "strings"

// NormalizeSection is the canonical form of a proposal's section heading: no
// leading ATX markers (1 to 6 #s followed by a space, a tab, or the end) and no
// surrounding space, so "#channels" still names a heading "## #channels".
// It strips markers until none is left so that it is idempotent: sections are
// normalized again downstream, and proposal identity depends on it.
// docs.Doc.SectionSpan applies the same rule.
func NormalizeSection(section string) string {
	for {
		section = strings.TrimSpace(section)
		hashes := len(section) - len(strings.TrimLeft(section, "#"))
		if hashes < 1 || hashes > 6 || (hashes < len(section) && section[hashes] != ' ' && section[hashes] != '\t') {
			return section
		}
		section = section[hashes:]
	}
}
