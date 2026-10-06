package basedocs_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
)

func TestFillOriginal(t *testing.T) {
	t.Parallel()

	doc, err := docs.ParseDoc("docs/a.md", []byte("---\ntitle: A\n---\n# A\n\n## One\ntext\n\n## Dup\nx\n\n## Dup\ny\n"))
	if err != nil {
		t.Fatalf("ParseDoc: %v", err)
	}

	tests := []struct {
		name         string
		section      string
		wantOriginal string
		wantLines    review.LineRange
		wantErr      string
	}{
		{name: "found", section: "One", wantOriginal: "## One\ntext\n\n", wantLines: review.LineRange{Start: 6, End: 8}},
		{name: "new doc", section: ""},
		{name: "missing lists headings", section: "Nope", wantErr: `headings are: "A", "One", "Dup", "Dup"`},
		{name: "ambiguous", section: "Dup", wantErr: "not exactly one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := review.Proposal{DocPath: doc.Path, Section: tc.section}
			err := basedocs.FillOriginal(&p, doc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("FillOriginal() error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FillOriginal() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantOriginal, p.Original); diff != "" {
				t.Errorf("Original mismatch (-want +got):\n%s", diff)
			}
			if p.Lines != tc.wantLines {
				t.Errorf("Lines = %+v, want %+v", p.Lines, tc.wantLines)
			}
		})
	}
}

// docs.Parse and review.Proposal.ValidateTarget are in packages that cannot
// import each other, so this pins that they accept the same doc extensions.
func TestDocExtensionsAgree(t *testing.T) {
	t.Parallel()

	for _, ext := range []string{".md", ".mdx", ".MD", ".markdown", ".txt", ""} {
		p := "docs/a" + ext

		tree, err := docs.Parse(fstest.MapFS{p: {Data: []byte("---\ntitle: A\n---\n")}})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		walked := len(tree.Docs) == 1

		valid := review.Proposal{DocPath: p}.ValidateTarget() == nil

		if walked != valid {
			t.Errorf("extension %q: docs.Parse accepts = %v, review accepts = %v", ext, walked, valid)
		}

		base := docs.Tree{Docs: []docs.Doc{{Path: "docs/old.md", Covers: []string{"src/*.go"}}}}
		candidates, deleted := base.Candidates([]docs.Change{{Path: "src/x.go"}, {Path: p, PreviousPath: "docs/old.md"}})
		renamed := len(candidates) == 1 && candidates[0] == p && len(deleted) == 0
		if renamed != valid {
			t.Errorf("extension %q: Candidates treats a rename to it as a doc = %v, review accepts = %v", ext, renamed, valid)
		}
	}
}

// docs cannot import review, so SectionSpan keeps its own heading
// normalization; this pins it to review.NormalizeSection.
func TestSectionSpanNormalizesLikeReview(t *testing.T) {
	t.Parallel()

	doc := docs.ParseBody("docs/a.md", []byte("# Title\n\n## Setup\ntext\n\n## #channels\nmore\n"))
	for _, in := range []string{"Setup", "## Setup", "  ### Setup  ", "#Setup", "Set up", "setup", "#channels", "## #channels", "channels", "####### Setup"} {
		_, _, _, found := doc.SectionSpan(in)
		norm := review.NormalizeSection(in)
		if want := norm == "Setup" || norm == "#channels"; found != want {
			t.Errorf("SectionSpan(%q) found = %v, want %v (NormalizeSection = %q)", in, found, want, norm)
		}
	}
}
