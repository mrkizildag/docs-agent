// Package docs reads a repo's docs/ tree into parsed docs and matches
// changed files against each doc's covers globs.
package docs

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"
)

// MaxDocBytes is the largest doc file the package reads.
const MaxDocBytes = 1 << 20

// Doc is one parsed markdown file under docs/.
type Doc struct {
	Path     string
	Title    string
	Summary  string
	Covers   []string
	Source   []byte
	Sections []Section
}

// Section is one heading's span in Doc.Source, Start to End exclusive.
// Level 0 is the untitled preamble before the first heading.
type Section struct {
	Heading string
	Level   int
	Start   int
	End     int
}

type frontmatter struct {
	Title   string   `yaml:"title"`
	Summary string   `yaml:"summary"`
	Covers  []string `yaml:"covers"`
}

// ParseDoc parses one docs/ markdown file's frontmatter and sections. Its
// errors do not name path; the caller knows it.
func ParseDoc(path string, src []byte) (Doc, error) {
	fm, body, err := splitFrontmatter(src)
	if err != nil {
		return Doc{}, err
	}

	var meta frontmatter
	if err := yaml.Unmarshal(fm, &meta); err != nil {
		return Doc{}, fmt.Errorf("decode frontmatter: %w", err)
	}

	if err := validateCovers(meta.Covers); err != nil {
		return Doc{}, err
	}

	return Doc{
		Path:     path,
		Title:    meta.Title,
		Summary:  meta.Summary,
		Covers:   meta.Covers,
		Source:   src,
		Sections: parseSections(src, body),
	}, nil
}

// ParseBody is ParseDoc without the frontmatter requirement: it keeps Path and
// Source and builds Sections from the headings after a leading "---" block,
// well-formed or not. Title, Summary, and Covers are left empty.
func ParseBody(path string, src []byte) Doc {
	bodyStart := 0

	if _, body, err := splitFrontmatter(src); err == nil {
		bodyStart = body
	} else if first, _ := cutLine(bytes.TrimPrefix(src, []byte(utf8BOM))); isDelimiter(first) {
		bodyStart = len(src)
	}

	return Doc{Path: path, Source: src, Sections: parseSections(src, bodyStart)}
}

// CheckScaffoldDoc reports why src is not a usable scaffold doc: it must parse
// and carry a title, a summary and a covers key (an empty list is a value).
func CheckScaffoldDoc(path string, src []byte) error {
	doc, err := ParseDoc(path, src)
	if err != nil {
		return fmt.Errorf("check scaffold doc %s: %w", path, err)
	}
	switch {
	case strings.TrimSpace(doc.Title) == "":
		return fmt.Errorf("check scaffold doc %s: frontmatter has no title", path)
	case strings.TrimSpace(doc.Summary) == "":
		return fmt.Errorf("check scaffold doc %s: frontmatter has no summary", path)
	case doc.Covers == nil:
		return fmt.Errorf("check scaffold doc %s: frontmatter has no covers list", path)
	}
	return nil
}

// ScaffoldFile is one file of a scaffold at its repo-relative path.
type ScaffoldFile struct {
	Path    string
	Content string
}

// CheckScaffold reports why files are not a usable starting docs folder: each
// must fit in MaxDocBytes and pass CheckScaffoldDoc at its path, and the file
// at indexPath must have an indexHeading section and link every other file
// relatively, which assumes they sit under the index's directory. It reports a missing indexPath file.
func CheckScaffold(files []ScaffoldFile, indexPath, indexHeading string) error {
	var index *ScaffoldFile

	for i, f := range files {
		if len(f.Content) > MaxDocBytes {
			return fmt.Errorf("check scaffold doc %s: %d bytes exceed the %d byte cap", f.Path, len(f.Content), MaxDocBytes)
		}

		if err := CheckScaffoldDoc(f.Path, []byte(f.Content)); err != nil {
			return err
		}

		if f.Path == indexPath {
			index = &files[i]
		}
	}

	if index == nil {
		return fmt.Errorf("check scaffold: no file at index path %s", indexPath)
	}

	doc, err := ParseDoc(index.Path, []byte(index.Content))
	if err != nil {
		return fmt.Errorf("check scaffold doc %s: %w", index.Path, err)
	}

	level := len(indexHeading) - len(strings.TrimLeft(indexHeading, "#"))
	if section, ok := doc.findSection(indexHeading); !ok || section.Level != level {
		return fmt.Errorf("check scaffold doc %s: the index must have exactly one %q section at that level", index.Path, indexHeading)
	}

	for _, f := range files {
		if f.Path == indexPath {
			continue
		}

		if link := "](" + strings.TrimPrefix(f.Path, path.Dir(indexPath)+"/") + ")"; !strings.Contains(index.Content, link) {
			return fmt.Errorf("check scaffold doc %s: the index must link its sibling docs relatively, missing %q", index.Path, link)
		}
	}

	return nil
}

// SectionSpan returns the text of the section titled heading (a leading ATX
// marker and surrounding space ignored) and its 1-based inclusive line range, from the
// heading line through the section's last line. ok is false when no heading
// matches or when several do, since a span for the wrong one would be edited.
func (d Doc) SectionSpan(heading string) (text string, start, end int, ok bool) {
	found, ok := d.findSection(heading)
	if !ok {
		return "", 0, 0, false
	}

	start = 1 + bytes.Count(d.Source[:found.Start], []byte("\n"))
	text = string(d.Source[found.Start:found.End])
	end = start + strings.Count(strings.TrimSuffix(text, "\n"), "\n")

	return text, start, end, true
}

// findSection returns the one titled section whose heading is heading with its
// ATX marker and surrounding space ignored; ok is false for none or several.
func (d Doc) findSection(heading string) (found *Section, ok bool) {
	want := normalizeHeading(heading)

	for i, s := range d.Sections {
		if s.Level == 0 || s.Heading != want {
			continue
		}

		if found != nil {
			return nil, false
		}

		found = &d.Sections[i]
	}

	return found, found != nil
}

// normalizeHeading is review.NormalizeSection, which this package cannot
// import; a test in review/basedocs pins them together.
func normalizeHeading(heading string) string {
	for {
		heading = strings.TrimSpace(heading)
		hashes := len(heading) - len(strings.TrimLeft(heading, "#"))
		if hashes < 1 || hashes > 6 || (hashes < len(heading) && heading[hashes] != ' ' && heading[hashes] != '\t') {
			return heading
		}
		heading = heading[hashes:]
	}
}

func validateCovers(covers []string) error {
	if len(covers) > maxCovers {
		return fmt.Errorf("%d covers globs, max %d", len(covers), maxCovers)
	}

	for _, glob := range covers {
		switch {
		case !doublestar.ValidatePattern(glob):
			return fmt.Errorf("invalid covers glob %q", glob)
		case strings.HasPrefix(glob, "/") || strings.HasPrefix(glob, "./") || strings.HasSuffix(glob, "/"):
			return fmt.Errorf("covers glob %q must be repo-root relative without leading \"/\" or \"./\" or trailing \"/\"", glob)
		case len(glob) > maxGlobBytes:
			return fmt.Errorf("covers glob %q longer than %d bytes", glob, maxGlobBytes)
		// Brace alternatives make doublestar matching exponential: 200ms at 22 groups, doubling per group.
		case strings.Count(glob, "{") > maxGlobBraces:
			return fmt.Errorf("covers glob %q has more than %d brace groups", glob, maxGlobBraces)
		}
	}

	return nil
}

// splitFrontmatter returns the YAML between the leading "---" line and the
// next line that is exactly "---", and the byte offset of the body after it.
func splitFrontmatter(src []byte) ([]byte, int, error) {
	firstLine, rest := cutLine(bytes.TrimPrefix(src, []byte(utf8BOM)))
	if !isDelimiter(firstLine) {
		return nil, 0, fmt.Errorf("missing frontmatter")
	}

	fmStart := len(src) - len(rest)
	offset := fmStart

	for len(rest) > 0 {
		line, next := cutLine(rest)
		lineStart := offset
		offset = len(src) - len(next)

		if isDelimiter(line) {
			return src[fmStart:lineStart], offset, nil
		}

		rest = next
	}

	return nil, 0, fmt.Errorf("unterminated frontmatter")
}

// cutLine returns the first line of b, excluding its trailing "\n", and the
// remainder of b after that "\n". If b has no "\n", the whole of b is the
// line and the remainder is empty.
func cutLine(b []byte) (line, rest []byte) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return b, nil
	}

	return b[:i], b[i+1:]
}

func trimCR(b []byte) string {
	return strings.TrimSuffix(string(b), "\r")
}

func isDelimiter(line []byte) bool {
	return strings.TrimRight(string(line), " \t\r") == "---"
}

const (
	maxCovers     = 100
	maxGlobBytes  = 256
	maxGlobBraces = 3

	utf8BOM = "\xEF\xBB\xBF"
)

const (
	fenceBacktick = '`'
	fenceTilde    = '~'
)

type heading struct {
	level int
	text  string
	start int
}

// parseSections walks src[bodyStart:] for ATX headings, fence-aware, and
// turns the resulting heading list into Sections spanning to the next
// heading of the same or higher level.
func parseSections(src []byte, bodyStart int) []Section {
	headings := scanHeadings(src, bodyStart)

	preambleEnd := len(src)
	if len(headings) > 0 {
		preambleEnd = headings[0].start
	}

	var sections []Section

	if preambleEnd > bodyStart {
		sections = append(sections, Section{Heading: "", Level: 0, Start: bodyStart, End: preambleEnd})
	}

	for i, h := range headings {
		end := len(src)

		for j := i + 1; j < len(headings); j++ {
			if headings[j].level <= h.level {
				end = headings[j].start

				break
			}
		}

		sections = append(sections, Section{Heading: h.text, Level: h.level, Start: h.start, End: end})
	}

	return sections
}

func scanHeadings(src []byte, bodyStart int) []heading {
	var (
		headings  []heading
		fenceChar byte
		fenceLen  int
	)

	pos := bodyStart
	for pos < len(src) {
		lineStart := pos
		line, next := cutLine(src[pos:])
		pos = len(src) - len(next)

		content := trimCR(line)
		trimmed := strings.TrimLeft(content, " ")
		indent := len(content) - len(trimmed)

		if fenceChar != 0 {
			if indent <= 3 && isFenceClose(trimmed, fenceChar, fenceLen) {
				fenceChar = 0
				fenceLen = 0
			}

			continue
		}

		if indent <= 3 {
			if ch, flen, ok := fenceOpen(trimmed); ok {
				fenceChar = ch
				fenceLen = flen

				continue
			}

			if level, text, ok := atxHeading(trimmed); ok {
				headings = append(headings, heading{level: level, text: text, start: lineStart})
			}
		}
	}

	return headings
}

// atxHeading matches a CommonMark ATX heading: 1-6 '#'s followed by a space,
// tab, or end of line; the heading text has trailing '#'s and space stripped.
func atxHeading(line string) (level int, text string, ok bool) {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}

	if i == 0 || i > 6 {
		return 0, "", false
	}

	if i < len(line) && line[i] != ' ' && line[i] != '\t' {
		return 0, "", false
	}

	rest := strings.Trim(line[i:], " \t")
	if stripped := strings.TrimRight(rest, "#"); stripped != rest {
		if stripped == "" || stripped[len(stripped)-1] == ' ' || stripped[len(stripped)-1] == '\t' {
			rest = strings.TrimRight(stripped, " \t")
		}
	}

	return i, rest, true
}

// fenceOpen returns the fence char and length if line opens a ``` or ~~~ fence.
func fenceOpen(line string) (ch byte, length int, ok bool) {
	if len(line) == 0 {
		return 0, 0, false
	}

	c := line[0]
	if c != fenceBacktick && c != fenceTilde {
		return 0, 0, false
	}

	n := 0
	for n < len(line) && line[n] == c {
		n++
	}

	if n < 3 || (c == fenceBacktick && strings.ContainsRune(line[n:], fenceBacktick)) {
		return 0, 0, false
	}

	return c, n, true
}

// isFenceClose reports whether line closes a fence of ch with at least
// fenceLen repeats and nothing else but trailing whitespace.
func isFenceClose(line string, ch byte, fenceLen int) bool {
	n := 0
	for n < len(line) && line[n] == ch {
		n++
	}

	if n < fenceLen || n == 0 {
		return false
	}

	return strings.TrimSpace(line[n:]) == ""
}
