package docs_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mrkizildag/docs-agent/backend/internal/docs"
)

const reassembleSrc = "---\ntitle: T\ncovers: [\"a/**\"]\n---\nIntro\n\n# One\nx\n## Two\ny\n```md\n# fenced\n```\n### Three\nz\n## Four\nw\n# Five\nend"

// Every section, replaced, leaves every byte outside its span untouched, and the
// reassembled doc reparses with the same sections before it.
func TestReplaceEachSectionChangesOnlyItsBytes(t *testing.T) {
	t.Parallel()

	doc, err := docs.ParseDoc("docs/r.md", []byte(reassembleSrc))
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}

	wantHeadings := []string{"", "One", "Two", "Three", "Four", "Five"}
	if len(doc.Sections) != len(wantHeadings) {
		t.Fatalf("Sections = %+v, want headings %q", doc.Sections, wantHeadings)
	}

	for i, s := range doc.Sections {
		if s.Heading != wantHeadings[i] {
			t.Errorf("Sections[%d].Heading = %q, want %q", i, s.Heading, wantHeadings[i])
		}
	}

	for _, s := range doc.Sections {
		rep := []byte("REPLACED\n")
		if s.Level > 0 {
			rep = []byte(strings.Repeat("#", s.Level) + " " + s.Heading + "\nREPLACED\n")
		}
		out := append(append(append([]byte{}, doc.Source[:s.Start]...), rep...), doc.Source[s.End:]...)

		if !bytes.HasPrefix(out, doc.Source[:s.Start]) || !bytes.HasSuffix(out, doc.Source[s.End:]) {
			t.Errorf("replacing %q changed bytes outside its span", s.Heading)
		}

		if got := string(doc.Source[s.Start:s.End]); s.Level > 0 && !strings.HasPrefix(got, strings.Repeat("#", s.Level)+" "+s.Heading) {
			t.Errorf("section %q span %q does not start at its heading", s.Heading, got)
		}

		re, err := docs.ParseDoc("docs/r.md", out)
		if err != nil {
			t.Fatalf("reparse after replacing %q: %v", s.Heading, err)
		}

		for _, before := range doc.Sections {
			if before.End > s.Start {
				break
			}

			found := false
			for _, after := range re.Sections {
				if after.Heading == before.Heading && after.Start == before.Start && after.Level == before.Level {
					found = true
				}
			}

			if !found {
				t.Errorf("after replacing %q, earlier section %q moved", s.Heading, before.Heading)
			}
		}
	}

	if !bytes.Equal(doc.Source, []byte(reassembleSrc)) {
		t.Errorf("Source not byte-for-byte equal to input")
	}
}

// "## Two" spans through its "### Three" child up to "## Four".
func TestSectionSpansIncludeChildren(t *testing.T) {
	t.Parallel()

	doc, err := docs.ParseDoc("docs/r.md", []byte(reassembleSrc))
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}

	byName := map[string]docs.Section{}
	for _, s := range doc.Sections {
		byName[s.Heading] = s
	}

	if byName["Two"].End != byName["Four"].Start {
		t.Errorf("Two.End = %d, want Four.Start %d", byName["Two"].End, byName["Four"].Start)
	}

	if byName["Three"].End != byName["Four"].Start {
		t.Errorf("Three.End = %d, want Four.Start %d", byName["Three"].End, byName["Four"].Start)
	}

	if byName["One"].End != byName["Five"].Start {
		t.Errorf("One.End = %d, want Five.Start %d", byName["One"].End, byName["Five"].Start)
	}
}
