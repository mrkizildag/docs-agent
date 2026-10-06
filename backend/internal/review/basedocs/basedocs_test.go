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

const xDoc = "---\ncovers:\n  - \"*.go\"\n---\n# X\n"

func hunk(start int) []review.LineRange { return []review.LineRange{{Start: start, End: start + 5}} }

func TestSelect(t *testing.T) {
	t.Parallel()

	removedDoc := review.ChangedFile{Path: "docs/x.md", Removed: true}
	gone := review.ChangedFile{Path: "gone.go", Removed: true}
	kept := review.ChangedFile{Path: "main.go", Hunks: hunk(4)}
	binary := review.ChangedFile{Path: "logo.go"}
	renamed := review.ChangedFile{Path: "new.go", PreviousPath: "old.go"}
	other := review.ChangedFile{Path: "README.txt", Hunks: hunk(7)}
	base := func(readme string) fstest.MapFS {
		fsys := fstest.MapFS{"docs/x.md": {Data: []byte(xDoc)}}
		if readme != "" {
			fsys["docs/README.md"] = &fstest.MapFile{Data: []byte(readme)}
		}
		return fsys
	}

	tests := []struct {
		name           string
		readme         string
		files          []review.ChangedFile
		wantCandidates []string
		wantUncovered  []string
		wantRestore    bool
		wantAnchor     review.Anchor
		wantIndex      string
		wantReason     string
		wantErr        bool
	}{
		{name: "covered change selects the doc", files: []review.ChangedFile{kept}, wantCandidates: []string{"docs/x.md"}},
		{name: "uncovered change is reported", files: []review.ChangedFile{other}, wantUncovered: []string{"README.txt"}},
		{name: "uncovered file is reported when the deleted doc has nothing to restore", files: []review.ChangedFile{removedDoc, gone, other}, wantUncovered: []string{"README.txt"}},
		{name: "deleted doc is restored at the first surviving file", readme: "- [X](x.md): about x.\n", files: []review.ChangedFile{removedDoc, gone, kept}, wantRestore: true, wantAnchor: review.Anchor{File: "main.go", Line: 4}, wantIndex: "- [X](x.md): about x.", wantReason: `"docs/x.md" was deleted but covers "gone.go", "main.go"`},
		{name: "index entry is synthesised", files: []review.ChangedFile{removedDoc, kept}, wantRestore: true, wantAnchor: review.Anchor{File: "main.go", Line: 4}, wantIndex: "- [x.md](x.md)", wantReason: `"docs/x.md" was deleted but covers "main.go"`},
		{name: "every covered file deleted", files: []review.ChangedFile{removedDoc, gone}},
		{name: "covered file without hunk anchors to another changed file", files: []review.ChangedFile{removedDoc, binary, other}, wantRestore: true, wantAnchor: review.Anchor{File: "README.txt", Line: 7}, wantIndex: "- [x.md](x.md)", wantReason: `"docs/x.md" was deleted but covers "logo.go"`},
		{name: "pure rename anchors to another changed file", files: []review.ChangedFile{removedDoc, renamed, other}, wantRestore: true, wantAnchor: review.Anchor{File: "README.txt", Line: 7}, wantIndex: "- [x.md](x.md)", wantReason: `"docs/x.md" was deleted but covers "new.go"`},
		{name: "covered files deleted with the doc is not restored even when another file has a hunk", files: []review.ChangedFile{removedDoc, gone, other}, wantUncovered: []string{"README.txt"}},
		{name: "covered file survives but no file has a hunk", files: []review.ChangedFile{removedDoc, binary}, wantErr: true},
		{name: "reason lists three paths", files: []review.ChangedFile{removedDoc, {Path: "a.go", Hunks: hunk(1)}, {Path: "b.go"}, {Path: "c.go"}, {Path: "d.go"}}, wantRestore: true, wantAnchor: review.Anchor{File: "a.go", Line: 1}, wantIndex: "- [x.md](x.md)", wantReason: `"docs/x.md" was deleted but covers "a.go", "b.go", "c.go", ...`},
		{name: "reason stays on one line", files: []review.ChangedFile{removedDoc, {Path: "a\nb.go", Hunks: hunk(1)}}, wantRestore: true, wantAnchor: review.Anchor{File: "a\nb.go", Line: 1}, wantIndex: "- [x.md](x.md)", wantReason: `"docs/x.md" was deleted but covers "a\nb.go"`},
		{name: "doc renamed out of docs is restored", files: []review.ChangedFile{{Path: "old/x.md", PreviousPath: "docs/x.md"}, kept}, wantRestore: true, wantAnchor: review.Anchor{File: "main.go", Line: 4}, wantIndex: "- [x.md](x.md)", wantReason: `"docs/x.md" was deleted but covers "main.go"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := basedocs.Select(base(tc.readme), tc.files)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Select() = %+v, nil, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select() = %v, want nil", err)
			}
			if diff := cmp.Diff(tc.wantCandidates, got.Candidates); diff != "" {
				t.Errorf("Candidates (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantUncovered, got.Uncovered); diff != "" {
				t.Errorf("Uncovered (-want +got):\n%s", diff)
			}
			if !tc.wantRestore {
				if len(got.Restores) != 0 {
					t.Errorf("Restores = %+v, want none", got.Restores)
				}
				return
			}
			if len(got.Restores) != 1 {
				t.Fatalf("Restores = %+v, want one", got.Restores)
			}
			p := got.Restores[0]
			if p.Anchor != tc.wantAnchor || p.IndexEntry != tc.wantIndex || p.Section != "" || p.Content != xDoc {
				t.Errorf("Restores[0] = %+v, want anchor %v, index %q, empty section, source content", p, tc.wantAnchor, tc.wantIndex)
			}
			if p.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", p.Reason, tc.wantReason)
			}
			if strings.Contains(p.Reason, "README.txt") && !strings.Contains(tc.wantReason, "README.txt") {
				t.Errorf("Reason = %q, want it not to name an uncovered file", p.Reason)
			}
			if err := p.Validate(tc.files); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestSelectionValidateProposal(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{{Path: "pkg/new.go", Hunks: hunk(1)}}
	sel := basedocs.Selection{Uncovered: []string{"pkg/new.go"}}
	newDoc := func(covers string) review.Proposal {
		return review.Proposal{
			DocPath:    "docs/new.md",
			Anchor:     review.Anchor{File: "pkg/new.go", Line: 2},
			Reason:     "new feature",
			Content:    "---\ntitle: New\nsummary: About new.\ncovers:\n  - " + covers + "\n---\n# New\n",
			IndexEntry: "- [New](new.md): about new.",
		}
	}

	tests := []struct {
		name     string
		proposal review.Proposal
		wantErr  bool
	}{
		{name: "new doc covering an uncovered file", proposal: newDoc("pkg/*.go")},
		{name: "new doc covering no uncovered file", proposal: newDoc("other/*.go"), wantErr: true},
		{name: "new doc without frontmatter", proposal: func() review.Proposal {
			p := newDoc("pkg/*.go")
			p.Content = "# New\n"
			return p
		}(), wantErr: true},
		{name: "new doc must be .md", proposal: func() review.Proposal {
			p := newDoc("pkg/*.go")
			p.DocPath = "docs/new.mdx"
			return p
		}(), wantErr: true},
		{name: "new doc over the size cap", proposal: func() review.Proposal {
			p := newDoc("pkg/*.go")
			p.Content += strings.Repeat("x", docs.MaxDocBytes)
			return p
		}(), wantErr: true},
		{name: "section edit needs no covers match", proposal: review.Proposal{
			DocPath: "docs/x.md", Section: "X", Anchor: review.Anchor{File: "pkg/new.go", Line: 2}, Reason: "r", Content: "## X\n",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := sel.ValidateProposal(tc.proposal, changed)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateProposal() = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestSelectionEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sel  basedocs.Selection
		want bool
	}{
		{name: "nothing", want: true},
		{name: "candidates", sel: basedocs.Selection{Candidates: []string{"docs/x.md"}}},
		{name: "uncovered", sel: basedocs.Selection{Uncovered: []string{"a.go"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.sel.Empty(); got != tc.want {
				t.Errorf("Empty() = %t, want %t", got, tc.want)
			}
		})
	}
}
