// Package docs reads a repo's docs/ tree into parsed docs and matches
// changed files against each doc's covers globs.
package docs

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v3"
)

// MaxDocBytes is the largest doc file the package reads.
const MaxDocBytes = maxDocBytes

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

// ParseDoc parses one docs/ markdown file's frontmatter and sections.
func ParseDoc(path string, src []byte) (Doc, error) {
	fm, body, err := splitFrontmatter(src)
	if err != nil {
		return Doc{}, fmt.Errorf("parse %s: %w", path, err)
	}

	var meta frontmatter
	if err := yaml.Unmarshal(fm, &meta); err != nil {
		return Doc{}, fmt.Errorf("parse %s: decode frontmatter: %w", path, err)
	}

	if err := validateCovers(meta.Covers); err != nil {
		return Doc{}, fmt.Errorf("parse %s: %w", path, err)
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

// CheckNewDoc reports why src is not a usable new doc and returns it parsed: it
// must fit in MaxDocBytes, parse, carry a title, a summary and a covers key (an
// empty list is a value), and link other docs of repo ("owner/repo") relatively.
func CheckNewDoc(path string, src []byte, repo string) (Doc, error) {
	if len(src) > MaxDocBytes {
		return Doc{}, fmt.Errorf("check new doc %s: %d bytes exceed the %d byte cap", path, len(src), MaxDocBytes)
	}
	doc, err := ParseDoc(path, src)
	if err != nil {
		return Doc{}, fmt.Errorf("check new doc %s: %w", path, err)
	}
	switch {
	case strings.TrimSpace(doc.Title) == "":
		return Doc{}, fmt.Errorf("check new doc %s: frontmatter has no title", path)
	case strings.TrimSpace(doc.Summary) == "":
		return Doc{}, fmt.Errorf("check new doc %s: frontmatter has no summary", path)
	case doc.Covers == nil:
		return Doc{}, fmt.Errorf("check new doc %s: frontmatter has no covers list", path)
	}
	if err := CheckDocLinks(src, repo); err != nil {
		return Doc{}, fmt.Errorf("check new doc %s: %w", path, err)
	}
	return doc, nil
}

var (
	referenceLink = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:\s*<?([^\s>]+)`)
)

// CheckDocLinks reports the first inline or reference-style link in src, outside
// code, that points at a doc of repo ("owner/repo") absolutely instead of
// relatively.
func CheckDocLinks(src []byte, repo string) error {
	text := stripCode(src)
	for _, re := range []*regexp.Regexp{linkTarget, referenceLink} {
		for _, m := range re.FindAllSubmatch(text, -1) {
			if target := string(m[1]); isAbsoluteDocLink(target, repo) {
				return fmt.Errorf("link %q to another doc must be relative", target)
			}
		}
	}
	return nil
}

// stripCode drops fenced code blocks and inline code spans, where link syntax
// is text and not a link.
func stripCode(src []byte) []byte {
	var (
		out       []byte
		fenceChar byte
		fenceLen  int
	)
	rest := src
	for len(rest) > 0 {
		line, next := cutLine(rest)
		full := rest[:len(rest)-len(next)]
		rest = next

		trimmed := strings.TrimLeft(trimCR(line), " ")
		indent := len(trimCR(line)) - len(trimmed)
		switch {
		case fenceChar != 0:
			if indent <= 3 && isFenceClose(trimmed, fenceChar, fenceLen) {
				fenceChar = 0
			}
		case indent <= 3:
			if ch, n, ok := fenceOpen(trimmed); ok {
				fenceChar, fenceLen = ch, n
				continue
			}
			out = append(out, full...)
		default:
			out = append(out, full...)
		}
	}
	return stripCodeSpans(out)
}

// stripCodeSpans drops inline code spans: a backtick run closed by the next
// run of the same length. An unclosed run is literal text.
func stripCodeSpans(src []byte) []byte {
	var out []byte
	for i := 0; i < len(src); {
		if src[i] != '`' {
			out = append(out, src[i])
			i++
			continue
		}
		n := backtickRun(src, i)
		end := -1
		for j := i + n; j < len(src); {
			if src[j] != '`' {
				j++
				continue
			}
			m := backtickRun(src, j)
			if m == n {
				end = j + m
				break
			}
			j += m
		}
		if end < 0 {
			out = append(out, src[i:i+n]...)
			i += n
			continue
		}
		i = end
	}
	return out
}

func backtickRun(src []byte, i int) int {
	n := 0
	for i+n < len(src) && src[i+n] == '`' {
		n++
	}
	return n
}

// isAbsoluteDocLink reports whether a link target is a root-absolute path into
// /docs/ or a GitHub URL into a docs/ folder of repo ("owner/repo"); links to
// other repos and sites are not links to this repo's docs.
func isAbsoluteDocLink(target, repo string) bool {
	if target == "/docs" || strings.HasPrefix(target, "/docs/") || strings.HasPrefix(target, "/docs#") {
		return true
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 || !strings.EqualFold(segs[0]+"/"+segs[1], repo) {
		return false
	}
	switch strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") {
	case "github.com":
		// /owner/repo/{blob,tree,raw,edit}/<ref>/docs/...
		return len(segs) > 4 && slices.Contains([]string{"blob", "tree", "raw", "edit"}, segs[2]) && segs[4] == "docs"
	case "raw.githubusercontent.com":
		// /owner/repo/<ref>/docs/...
		return len(segs) > 3 && segs[3] == "docs"
	}
	return false
}

// CheckScaffold reports why index, architecture and setup are not a usable
// starting docs folder for repo ("owner/repo"): each must pass CheckNewDoc at
// its path, and the index must link the other two relatively.
func CheckScaffold(index, architecture, setup, repo string) error {
	for _, doc := range []struct{ path, src string }{
		{"docs/README.md", index},
		{"docs/architecture.md", architecture},
		{"docs/guides/setup.md", setup},
	} {
		if _, err := CheckNewDoc(doc.path, []byte(doc.src), repo); err != nil {
			return err
		}
	}
	for _, link := range []string{"](architecture.md)", "](guides/setup.md)"} {
		if !strings.Contains(index, link) {
			return fmt.Errorf("check scaffold docs/README.md: the index must link its sibling docs relatively, missing %q", link)
		}
	}
	return nil
}

// SectionSpan returns the text of the section titled heading (leading "#"s and
// surrounding space ignored) and its 1-based inclusive line range, from the
// heading line through the section's last line. ok is false when no heading
// matches or when several do, since a span for the wrong one would be edited.
func (d Doc) SectionSpan(heading string) (text string, start, end int, ok bool) {
	want := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(heading), "#"))

	var found *Section

	for i, s := range d.Sections {
		if s.Level == 0 || s.Heading != want {
			continue
		}

		if found != nil {
			return "", 0, 0, false
		}

		found = &d.Sections[i]
	}

	if found == nil {
		return "", 0, 0, false
	}

	start = 1 + bytes.Count(d.Source[:found.Start], []byte("\n"))
	text = string(d.Source[found.Start:found.End])
	end = start + strings.Count(strings.TrimSuffix(text, "\n"), "\n")

	return text, start, end, true
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
