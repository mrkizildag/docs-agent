package finalize_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/finalize"
)

type fakeHead struct {
	files map[string]string
	other map[string]bool // paths that exist but are not readable files
	reads map[string]int
	err   error
}

func (h *fakeHead) Exists(_ context.Context, path string) (bool, error) {
	_, ok := h.files[path]
	return ok || h.other[path], h.err
}

func (h *fakeHead) ReadFile(_ context.Context, path string) ([]byte, bool, error) {
	if h.reads == nil {
		h.reads = map[string]int{}
	}
	h.reads[path]++
	s, ok := h.files[path]
	return []byte(s), ok, h.err
}

const guide = "---\ntitle: Guide\nsummary: s\ncovers: [a.go]\n---\n# Guide\n\n## Usage\n\nold usage\n\n## Setup\n\nold setup\n"

func changedFiles() []review.ChangedFile {
	return []review.ChangedFile{
		{Path: "a.go", Hunks: []review.LineRange{{Start: 10, End: 20}}},
		{Path: "gone.go", Removed: true},
	}
}

func edit(doc, section string) review.Proposal {
	return review.Proposal{
		DocPath: doc, Section: section, Anchor: review.Anchor{File: "a.go", Line: 12},
		Reason: "r", Content: "secret body\n",
	}
}

func newDoc(path string) review.Proposal {
	return review.Proposal{
		DocPath: path, Anchor: review.Anchor{File: "a.go", Line: 12}, Reason: "r",
		Content:    "---\ntitle: T\nsummary: s\ncovers: [a.go]\n---\n# T\n",
		IndexEntry: "- [T](t.md)",
	}
}

func TestProposals(t *testing.T) {
	sel := &basedocs.Selection{Uncovered: []string{"a.go"}}

	tests := []struct {
		name     string
		head     *fakeHead
		rules    finalize.Rules
		raw      []review.Proposal
		problems map[int][]string // index -> substrings
		check    func(t *testing.T, got []review.Proposal)
	}{
		{
			name:  "section is filled and normalized",
			head:  &fakeHead{files: map[string]string{"docs/g.md": guide}},
			rules: finalize.Rules{Changed: changedFiles()},
			raw:   []review.Proposal{edit("docs/g.md", "## Usage")},
			check: func(t *testing.T, got []review.Proposal) {
				if got[0].Section != "Usage" || got[0].Original != "## Usage\n\nold usage\n\n" || got[0].Lines != (review.LineRange{Start: 8, End: 11}) {
					t.Errorf("got %+v", got[0])
				}
			},
		},
		{
			name:  "broken frontmatter still fills Original",
			head:  &fakeHead{files: map[string]string{"docs/g.md": "---\ntitle: [\n---\n# Setup\n\ntext\n"}},
			rules: finalize.Rules{Changed: changedFiles()},
			raw:   []review.Proposal{edit("docs/g.md", "Setup")},
			check: func(t *testing.T, got []review.Proposal) {
				if !strings.Contains(got[0].Original, "text") {
					t.Errorf("Original = %q", got[0].Original)
				}
			},
		},
		{
			name:     "unknown heading lists headings",
			head:     &fakeHead{files: map[string]string{"docs/g.md": guide}},
			rules:    finalize.Rules{Changed: changedFiles()},
			raw:      []review.Proposal{edit("docs/g.md", "Nope")},
			problems: map[int][]string{0: {`"Nope"`, `"Usage"`, `"Setup"`}},
		},
		{
			name:     "missing doc",
			head:     &fakeHead{},
			rules:    finalize.Rules{Changed: changedFiles()},
			raw:      []review.Proposal{edit("docs/g.md", "Usage")},
			problems: map[int][]string{0: {"no such doc"}},
		},
		{
			name:     "duplicate heading",
			head:     &fakeHead{files: map[string]string{"docs/g.md": "# A\n\n## Usage\n\nx\n\n## Usage\n\ny\n"}},
			rules:    finalize.Rules{Changed: changedFiles()},
			raw:      []review.Proposal{edit("docs/g.md", "Usage")},
			problems: map[int][]string{0: {"ambiguous"}},
		},
		{
			name:  "anchor outside the hunks lists the commentable lines",
			head:  &fakeHead{files: map[string]string{"docs/g.md": guide}},
			rules: finalize.Rules{Changed: changedFiles()},
			raw: []review.Proposal{func() review.Proposal {
				p := edit("docs/g.md", "Usage")
				p.Anchor.Line = 99
				return p
			}()},
			problems: map[int][]string{0: {"anchor.line 99", "commentable lines: 10-20"}},
		},
		{
			name:  "anchor on removed or unchanged file rejected",
			head:  &fakeHead{files: map[string]string{"docs/g.md": guide}},
			rules: finalize.Rules{Changed: changedFiles()},
			raw: []review.Proposal{
				func() review.Proposal { p := edit("docs/g.md", "Usage"); p.Anchor.File = "gone.go"; return p }(),
				func() review.Proposal { p := edit("docs/g.md", "Usage"); p.Anchor.File = "other.go"; return p }(),
			},
			problems: map[int][]string{0: {"anchor.file"}, 1: {"anchor.file"}},
		},
		{
			name:     "new doc at existing file",
			head:     &fakeHead{files: map[string]string{"docs/t.md": "x"}},
			rules:    finalize.Rules{Changed: changedFiles(), Selection: sel, AllowNewDoc: true},
			raw:      []review.Proposal{newDoc("docs/t.md")},
			problems: map[int][]string{0: {"already exists at head"}},
		},
		{
			name:     "new doc at existing directory",
			head:     &fakeHead{other: map[string]bool{"docs/t.md": true}},
			rules:    finalize.Rules{Changed: changedFiles(), Selection: sel, AllowNewDoc: true},
			raw:      []review.Proposal{newDoc("docs/t.md")},
			problems: map[int][]string{0: {"already exists at head"}},
		},
		{
			name:  "new doc at free path accepted",
			head:  &fakeHead{},
			rules: finalize.Rules{Changed: changedFiles(), Selection: sel, AllowNewDoc: true},
			raw:   []review.Proposal{newDoc("docs/t.md")},
		},
		{
			name:     "new doc refused when not allowed",
			head:     &fakeHead{},
			rules:    finalize.Rules{Changed: changedFiles(), Selection: sel},
			raw:      []review.Proposal{newDoc("docs/t.md")},
			problems: map[int][]string{0: {"not allowed"}},
		},
		{
			name:     "new doc refused without selection",
			head:     &fakeHead{},
			rules:    finalize.Rules{Changed: changedFiles()},
			raw:      []review.Proposal{newDoc("docs/t.md")},
			problems: map[int][]string{0: {"not allowed"}},
		},
		{
			name:     "whitespace-only section is not a new doc",
			head:     &fakeHead{},
			rules:    finalize.Rules{Changed: changedFiles(), Selection: sel, AllowNewDoc: true},
			raw:      []review.Proposal{func() review.Proposal { p := newDoc("docs/t.md"); p.Section = "   "; return p }()},
			problems: map[int][]string{0: {"must name a heading"}},
		},
		{
			name:     "hash-only section rejected",
			head:     &fakeHead{files: map[string]string{"docs/g.md": guide}},
			rules:    finalize.Rules{Changed: changedFiles()},
			raw:      []review.Proposal{edit("docs/g.md", " ## ")},
			problems: map[int][]string{0: {"must name a heading"}},
		},
		{
			name:  "every problem is reported with its index, one line, no content",
			head:  &fakeHead{files: map[string]string{"docs/g.md": guide}},
			rules: finalize.Rules{Changed: changedFiles()},
			raw: []review.Proposal{
				edit("docs/g.md", "Nope"),
				edit("docs/g.md", "Usage"),
				func() review.Proposal { p := edit("docs/missing.md", "Usage"); p.Reason = "a\nb"; return p }(),
			},
			problems: map[int][]string{0: {"Nope"}, 2: {"reason"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, problems, err := finalize.Proposals(t.Context(), tt.head, tt.rules, tt.raw)
			if err != nil {
				t.Fatal(err)
			}

			if len(problems) != len(tt.problems) {
				t.Fatalf("problems = %v, want indexes %v", problems, tt.problems)
			}
			for _, p := range problems {
				msg := p.Err.Error()
				for _, want := range tt.problems[p.Index] {
					if !strings.Contains(msg, want) {
						t.Errorf("proposal %d: %q lacks %q", p.Index, msg, want)
					}
				}
				if strings.ContainsAny(msg, "\n\r") || strings.Contains(msg, "secret body") {
					t.Errorf("proposal %d: unsafe problem text %q", p.Index, msg)
				}
			}
			if len(problems) > 0 && (!strings.HasPrefix(problems.Error(), "proposal ")) {
				t.Errorf("Error() = %q", problems.Error())
			}

			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestProposalsReadsEachDocOnce(t *testing.T) {
	head := &fakeHead{files: map[string]string{"docs/g.md": guide}}
	raw := []review.Proposal{edit("docs/g.md", "Usage"), edit("docs/g.md", "Setup"), edit("docs/g.md", "Nope")}

	if _, _, err := finalize.Proposals(t.Context(), head, finalize.Rules{Changed: changedFiles()}, raw); err != nil {
		t.Fatal(err)
	}
	if head.reads["docs/g.md"] != 1 {
		t.Errorf("reads = %d, want 1", head.reads["docs/g.md"])
	}
}

func TestProposalsHeadErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	head := &fakeHead{err: boom}

	_, _, err := finalize.Proposals(t.Context(), head, finalize.Rules{Changed: changedFiles()}, []review.Proposal{edit("docs/g.md", "Usage")})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

func TestNoImpactReason(t *testing.T) {
	if got := finalize.NoImpactReason("  a\n\nb\tc  "); got != "a b c" {
		t.Errorf("got %q", got)
	}
	got := finalize.NoImpactReason(strings.Repeat("x", 1000))
	if len(got) != 303 || !strings.HasSuffix(got, "...") {
		t.Errorf("len = %d", len(got))
	}
}

func TestProposalsUnknownHeadingKeepsWholeHeadingList(t *testing.T) {
	var doc strings.Builder
	for i := range 40 {
		fmt.Fprintf(&doc, "## Heading number %02d\n\ntext\n\n", i)
	}
	head := &fakeHead{files: map[string]string{"docs/g.md": doc.String()}}

	_, problems, err := finalize.Proposals(t.Context(), head, finalize.Rules{Changed: changedFiles()}, []review.Proposal{edit("docs/g.md", "Nope")})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Err.Error(), `"Heading number 39"`) {
		t.Errorf("problems = %v", problems)
	}
}

func TestProposalsCapsQuotedModelText(t *testing.T) {
	head := &fakeHead{files: map[string]string{"docs/g.md": guide}}

	_, problems, err := finalize.Proposals(t.Context(), head, finalize.Rules{Changed: changedFiles()}, []review.Proposal{edit("docs/g.md", strings.Repeat("x", 1000))})
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || len(problems[0].Err.Error()) > 600 {
		t.Errorf("problems = %v", problems)
	}
}

func TestProposalsTooMany(t *testing.T) {
	head := &fakeHead{files: map[string]string{"docs/g.md": guide}}
	raw := make([]review.Proposal, finalize.MaxProposals+1)
	for i := range raw {
		raw[i] = edit("docs/g.md", "Usage")
	}

	got, problems, err := finalize.Proposals(t.Context(), head, finalize.Rules{Changed: changedFiles()}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil || len(problems) != 1 || problems[0].Index != finalize.MaxProposals || !strings.Contains(problems[0].Err.Error(), "too many proposals: 21") {
		t.Errorf("got %v, problems = %v", got, problems)
	}
	if len(head.reads) != 0 {
		t.Errorf("reads = %v, want none", head.reads)
	}
}

func TestProposalsAllowNewDocNeedsSelection(t *testing.T) {
	_, _, err := finalize.Proposals(t.Context(), &fakeHead{}, finalize.Rules{Changed: changedFiles(), AllowNewDoc: true}, []review.Proposal{newDoc("docs/t.md")})
	if err == nil {
		t.Error("err = nil, want programming error")
	}
}
