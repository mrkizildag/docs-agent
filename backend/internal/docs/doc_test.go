package docs_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
)

func TestParseDoc(t *testing.T) {
	t.Parallel()

	const fm = "---\ntitle: T\n---\n"

	tests := []struct {
		name        string
		src         string
		wantErr     string
		wantTitle   string
		wantSummary string
		wantCovers  []string
		wantSecs    []docs.Section
	}{
		{
			name:        "valid frontmatter",
			src:         "---\ntitle: T\nsummary: S\ncovers:\n  - \"a/**\"\n  - \"b.go\"\n---\nBody\n",
			wantTitle:   "T",
			wantSummary: "S",
			wantCovers:  []string{"a/**", "b.go"},
			wantSecs: []docs.Section{{
				Heading: "",
				Level:   0,
				Start:   len("---\ntitle: T\nsummary: S\ncovers:\n  - \"a/**\"\n  - \"b.go\"\n---\n"),
				End:     len("---\ntitle: T\nsummary: S\ncovers:\n  - \"a/**\"\n  - \"b.go\"\n---\nBody\n"),
			}},
		},
		{
			name:    "no frontmatter",
			src:     "# Heading\nbody",
			wantErr: "missing frontmatter",
		},
		{
			name:    "unterminated frontmatter",
			src:     "---\ntitle: T\n",
			wantErr: "unterminated frontmatter",
		},
		{
			name:    "bad YAML",
			src:     "---\ntitle: [unterminated\n---\nbody",
			wantErr: "decode frontmatter",
		},
		{
			name:    "covers as string",
			src:     "---\ntitle: T\ncovers: \"foo\"\n---\nbody",
			wantErr: "decode frontmatter",
		},
		{
			name:    "invalid glob",
			src:     "---\ncovers:\n  - \"a[\"\n---\nbody",
			wantErr: "invalid covers glob",
		},
		{name: "covers leading slash", src: "---\ncovers:\n  - \"/a/**\"\n---\nbody", wantErr: `"/a/**"`},
		{name: "covers leading dot slash", src: "---\ncovers:\n  - \"./a\"\n---\nbody", wantErr: `"./a"`},
		{name: "covers trailing slash", src: "---\ncovers:\n  - \"a/\"\n---\nbody", wantErr: `"a/"`},
		{name: "covers too long", src: "---\ncovers:\n  - \"" + strings.Repeat("a", 257) + "\"\n---\nbody", wantErr: "longer than"},
		{name: "covers too many braces", src: "---\ncovers:\n  - \"{a,b}{c,d}{e,f}{g,h}\"\n---\nbody", wantErr: "brace groups"},
		{
			name:    "too many covers",
			src:     "---\ncovers:\n" + strings.Repeat("  - \"a\"\n", 101) + "---\nbody",
			wantErr: "101 covers globs",
		},
		{
			name:       "three brace groups ok",
			src:        "---\ntitle: T\ncovers:\n  - \"{a,b}{c,d}{e,f}\"\n---\nbody",
			wantTitle:  "T",
			wantCovers: []string{"{a,b}{c,d}{e,f}"},
			wantSecs: []docs.Section{{
				Start: len("---\ntitle: T\ncovers:\n  - \"{a,b}{c,d}{e,f}\"\n---\n"),
				End:   len("---\ntitle: T\ncovers:\n  - \"{a,b}{c,d}{e,f}\"\n---\nbody"),
			}},
		},
		{
			name:      "inline backtick span is not a fence",
			src:       fm + "```js```\n# A\n## B\n",
			wantTitle: "T",
			wantSecs: []docs.Section{
				{Level: 0, Start: len(fm), End: len(fm + "```js```\n")},
				{Heading: "A", Level: 1, Start: len(fm + "```js```\n"), End: len(fm + "```js```\n# A\n## B\n")},
				{Heading: "B", Level: 2, Start: len(fm + "```js```\n# A\n"), End: len(fm + "```js```\n# A\n## B\n")},
			},
		},
		{
			name:      "BOM and trailing whitespace on delimiters",
			src:       "\xEF\xBB\xBF--- \t\ntitle: T\n---\t \r\n## H\n",
			wantTitle: "T",
			wantSecs: []docs.Section{{
				Heading: "H", Level: 2,
				Start: len("\xEF\xBB\xBF--- \t\ntitle: T\n---\t \r\n"),
				End:   len("\xEF\xBB\xBF--- \t\ntitle: T\n---\t \r\n## H\n"),
			}},
		},
		{
			name:       "empty covers",
			src:        fm + "body",
			wantTitle:  "T",
			wantCovers: nil,
			wantSecs:   []docs.Section{{Heading: "", Level: 0, Start: len(fm), End: len(fm) + len("body")}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := docs.ParseDoc("docs/x.md", []byte(tc.src))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseDoc(%q) err = nil, want containing %q", tc.src, tc.wantErr)
				}

				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseDoc(%q) err = %q, want containing %q", tc.src, err.Error(), tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseDoc(%q) unexpected err: %v", tc.src, err)
			}

			if got.Title != tc.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tc.wantTitle)
			}

			if got.Summary != tc.wantSummary {
				t.Errorf("Summary = %q, want %q", got.Summary, tc.wantSummary)
			}

			if diff := cmp.Diff(tc.wantCovers, got.Covers); diff != "" {
				t.Errorf("Covers mismatch (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tc.wantSecs, got.Sections); diff != "" {
				t.Errorf("Sections mismatch (-want +got):\n%s", diff)
			}

			if !bytes.Equal(got.Source, []byte(tc.src)) {
				t.Errorf("Source = %q, want %q", got.Source, tc.src)
			}
		})
	}
}

func TestParseDocSectionsNesting(t *testing.T) {
	t.Parallel()

	const fm = "---\ntitle: T\n---\n"

	a := "## A\n"
	textA := "text\n"
	b := "### B\n"
	textB := "more\n"
	c := "## C\n"
	textC := "end"

	src := fm + a + textA + b + textB + c + textC

	startA := len(fm)
	startB := startA + len(a) + len(textA)
	startC := startB + len(b) + len(textB)
	end := len(src)

	got, err := docs.ParseDoc("docs/x.md", []byte(src))
	if err != nil {
		t.Fatalf("ParseDoc: unexpected err: %v", err)
	}

	want := []docs.Section{
		{Heading: "A", Level: 2, Start: startA, End: startC},
		{Heading: "B", Level: 3, Start: startB, End: startC},
		{Heading: "C", Level: 2, Start: startC, End: end},
	}

	if diff := cmp.Diff(want, got.Sections); diff != "" {
		t.Errorf("Sections mismatch (-want +got):\n%s", diff)
	}
}

func TestParseDocFencedHeadingsIgnored(t *testing.T) {
	t.Parallel()

	const fm = "---\ntitle: T\n---\n"

	preamble := "```\n# not a heading\n```\n"
	heading := "## Real\n"
	body := "body\n"
	tildeFence := "~~~\n# also not\n~~~\n"

	src := fm + preamble + heading + body + tildeFence

	startPreamble := len(fm)
	startHeading := startPreamble + len(preamble)
	end := len(src)

	got, err := docs.ParseDoc("docs/x.md", []byte(src))
	if err != nil {
		t.Fatalf("ParseDoc: unexpected err: %v", err)
	}

	want := []docs.Section{
		{Heading: "", Level: 0, Start: startPreamble, End: startHeading},
		{Heading: "Real", Level: 2, Start: startHeading, End: end},
	}

	if diff := cmp.Diff(want, got.Sections); diff != "" {
		t.Errorf("Sections mismatch (-want +got):\n%s", diff)
	}
}

func TestParseDocClosingHashesStripped(t *testing.T) {
	t.Parallel()

	const fm = "---\ntitle: T\n---\n"

	src := fm + "## Heading ##\nbody"

	got, err := docs.ParseDoc("docs/x.md", []byte(src))
	if err != nil {
		t.Fatalf("ParseDoc: unexpected err: %v", err)
	}

	if len(got.Sections) != 1 {
		t.Fatalf("Sections = %#v, want 1 section", got.Sections)
	}

	if got.Sections[0].Heading != "Heading" {
		t.Errorf("Heading = %q, want %q", got.Sections[0].Heading, "Heading")
	}
}

func TestParseDocHeadingText(t *testing.T) {
	t.Parallel()

	tests := []struct{ line, want string }{
		{"# C#", "C#"},
		{"# A ##", "A"},
		{"# ##", ""},
		{"# A \\#", "A \\#"},
		{"## Heading ##", "Heading"},
	}

	for _, tc := range tests {
		t.Run(tc.line, func(t *testing.T) {
			t.Parallel()

			got, err := docs.ParseDoc("docs/x.md", []byte("---\ntitle: T\n---\n"+tc.line+"\n"))
			if err != nil {
				t.Fatalf("ParseDoc: unexpected err: %v", err)
			}

			if len(got.Sections) != 1 || got.Sections[0].Heading != tc.want {
				t.Errorf("ParseDoc(%q) sections = %+v, want one with Heading %q", tc.line, got.Sections, tc.want)
			}
		})
	}
}

func TestParseDocCRLF(t *testing.T) {
	t.Parallel()

	src := "---\r\ntitle: T\r\n---\r\n## H\r\nbody\r\n"

	got, err := docs.ParseDoc("docs/x.md", []byte(src))
	if err != nil {
		t.Fatalf("ParseDoc: unexpected err: %v", err)
	}

	if got.Title != "T" {
		t.Errorf("Title = %q, want %q", got.Title, "T")
	}

	if len(got.Sections) != 1 {
		t.Fatalf("Sections = %#v, want 1 section", got.Sections)
	}

	if got.Sections[0].Heading != "H" || got.Sections[0].Level != 2 {
		t.Errorf("Sections[0] = %+v, want Heading H, Level 2", got.Sections[0])
	}
}

func FuzzParseDoc(f *testing.F) {
	f.Add([]byte("---\ntitle: T\n---\n## H\nbody\n"))
	f.Add([]byte("---\ntitle: T\n---\n"))
	f.Add([]byte("no frontmatter here"))
	f.Add([]byte("---\n"))
	f.Add([]byte("\xEF\xBB\xBF---\ntitle: a\n---\n```js```\n# A\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := docs.ParseDoc("docs/fuzz.md", data)
		if err != nil {
			return
		}

		prevStart := -1

		for _, s := range got.Sections {
			if s.Start < 0 || s.Start > s.End || s.End > len(got.Source) {
				t.Fatalf("section %+v out of bounds for len %d", s, len(got.Source))
			}

			if s.Start < prevStart {
				t.Fatalf("sections out of order: %+v after start %d", s, prevStart)
			}

			prevStart = s.Start
		}
	})
}

func TestDocSectionSpan(t *testing.T) {
	t.Parallel()

	doc, err := docs.ParseDoc("docs/a.md", []byte("---\ntitle: T\n---\n# A\n\n## Mid\nmid\n\n## Last\nlast"))
	if err != nil {
		t.Fatalf("ParseDoc() = %v, want nil", err)
	}

	tests := []struct {
		heading            string
		wantText           string
		wantStart, wantEnd int
		wantOK             bool
	}{
		{heading: "Mid", wantText: "## Mid\nmid\n\n", wantStart: 6, wantEnd: 8, wantOK: true},
		{heading: " ## Last ", wantText: "## Last\nlast", wantStart: 9, wantEnd: 10, wantOK: true},
		{heading: "Nope"},
		{heading: ""},
	}
	for _, tc := range tests {
		text, start, end, ok := doc.SectionSpan(tc.heading)
		if text != tc.wantText || start != tc.wantStart || end != tc.wantEnd || ok != tc.wantOK {
			t.Errorf("SectionSpan(%q) = %q, %d, %d, %v; want %q, %d, %d, %v", tc.heading, text, start, end, ok, tc.wantText, tc.wantStart, tc.wantEnd, tc.wantOK)
		}
	}
}

func TestDocSectionSpanDuplicateHeading(t *testing.T) {
	t.Parallel()

	doc, err := docs.ParseDoc("docs/a.md", []byte("---\ntitle: T\n---\n# A\n\n## Server\n### Example\none\n\n## Actions\n### Example\ntwo\n"))
	if err != nil {
		t.Fatalf("ParseDoc() = %v, want nil", err)
	}

	if text, start, end, ok := doc.SectionSpan("### Example"); ok || text != "" || start != 0 || end != 0 {
		t.Errorf("SectionSpan(duplicate) = %q, %d, %d, %v; want empty, false", text, start, end, ok)
	}

	if _, _, _, ok := doc.SectionSpan("Actions"); !ok {
		t.Errorf("SectionSpan(unique) ok = false, want true")
	}
}

func TestParseBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		src          string
		wantHeadings []string
	}{
		{name: "valid frontmatter", src: "---\ncovers: [a]\n---\n# A\n## B\n", wantHeadings: []string{"A", "B"}},
		{name: "malformed frontmatter", src: "---\ncovers: [unclosed\n# not a heading\n---\n# A\n", wantHeadings: []string{"A"}},
		{name: "unterminated frontmatter", src: "---\ncovers: x\n# not a heading\n"},
		{name: "no frontmatter", src: "# A\ntext\n", wantHeadings: []string{"A"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := docs.ParseBody("docs/a.md", []byte(tc.src))
			if d.Path != "docs/a.md" || string(d.Source) != tc.src {
				t.Errorf("ParseBody() path %q source %q, want input echoed", d.Path, d.Source)
			}
			var got []string
			for _, s := range d.Sections {
				if s.Level > 0 {
					got = append(got, s.Heading)
				}
			}
			if diff := cmp.Diff(tc.wantHeadings, got); diff != "" {
				t.Errorf("headings (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCheckScaffold(t *testing.T) {
	t.Parallel()

	const (
		frontmatter = "---\ntitle: T\nsummary: S\ncovers: []\n---\n"
		index       = frontmatter + "## Index\n[a](architecture.md) [s](guides/setup.md)\n"
	)

	tests := []struct {
		name                     string
		index, architecture, set string
		wantErr                  string
	}{
		{name: "complete", index: index, architecture: frontmatter, set: frontmatter},
		{name: "index without frontmatter", index: "[a](architecture.md) [s](guides/setup.md)", architecture: frontmatter, set: frontmatter, wantErr: "docs/README.md"},
		{name: "architecture without summary", index: index, architecture: "---\ntitle: T\ncovers: []\n---\n", set: frontmatter, wantErr: "docs/architecture.md"},
		{name: "setup without title", index: index, architecture: frontmatter, set: "---\nsummary: S\ncovers: []\n---\n", wantErr: "docs/guides/setup.md"},
		{name: "index misses architecture link", index: frontmatter + "## Index\n[s](guides/setup.md)\n", architecture: frontmatter, set: frontmatter, wantErr: "](architecture.md)"},
		{name: "architecture over the byte cap", index: index, architecture: frontmatter + strings.Repeat("x", docs.MaxDocBytes), set: frontmatter, wantErr: "byte cap"},
		{name: "index misses setup link", index: frontmatter + "## Index\n[a](architecture.md)\n", architecture: frontmatter, set: frontmatter, wantErr: "](guides/setup.md)"},
		{name: "index without the Index heading", index: frontmatter + "## Docs\n[a](architecture.md) [s](guides/setup.md)\n", architecture: frontmatter, set: frontmatter, wantErr: `"## Index"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := docs.CheckScaffold([]docs.ScaffoldFile{
				{Path: "docs/README.md", Content: tc.index},
				{Path: "docs/architecture.md", Content: tc.architecture},
				{Path: "docs/guides/setup.md", Content: tc.set},
			}, "docs/README.md", "## Index")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("CheckScaffold() error = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("CheckScaffold() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
