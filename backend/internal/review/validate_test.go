package review_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func validProposal() review.Proposal {
	return review.Proposal{
		DocPath: "docs/guides/setup.md",
		Section: "Commands",
		Anchor:  review.Anchor{File: "backend/cmd/server/main.go", Line: 5},
		Reason:  "added a new CLI flag",
		Content: "## Commands\n\n- `make run`",
	}
}

func validChanged() []review.ChangedFile {
	return []review.ChangedFile{
		{
			Path:  "backend/cmd/server/main.go",
			Hunks: []review.LineRange{{Start: 1, End: 10}},
		},
	}
}

func TestProposalValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		proposal    func() review.Proposal
		changed     []review.ChangedFile
		wantErrPart string
	}{
		{
			name:     "valid",
			proposal: validProposal,
			changed:  validChanged(),
		},
		{
			name: "valid new doc",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = ""
				p.IndexEntry = "Setup guide"
				return p
			},
			changed: validChanged(),
		},
		{
			name: "doc path empty",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = ""
				return p
			},
			changed:     validChanged(),
			wantErrPart: "doc_path: must not be empty",
		},
		{
			name: "doc path absolute",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "/docs/guides/setup.md"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "doc_path",
		},
		{
			name: "doc path not clean",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs/guides/../guides/setup.md"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "doc_path",
		},
		{
			name: "doc path traversal",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs/../secrets.md"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "doc_path",
		},
		{
			name: "doc path not under docs",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "backend/guides/setup.md"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "doc_path",
		},
		{
			name: "doc path is docs itself",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "doc_path",
		},
		{
			name: "doc path not markdown",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs/guides/setup.txt"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "must end in .md",
		},
		{
			name: "doc path no extension",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs/guides/setup"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "must end in .md",
		},
		{
			name: "doc path mdx",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs/guides/setup.mdx"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "",
		},
		{
			name: "doc path newline",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs/guides/a\nb.md"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "control characters",
		},
		{
			name: "doc path backtick",
			proposal: func() review.Proposal {
				p := validProposal()
				p.DocPath = "docs/guides/a`b.md"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "backticks",
		},
		{
			name: "section newline",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = "Commands\n## Evil"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "section: must be one line",
		},
		{
			name: "section only hashes",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = "#"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "section: must name a heading",
		},
		{
			name: "section only hashes and spaces",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = " ## "
				return p
			},
			changed:     validChanged(),
			wantErrPart: "section: must name a heading",
		},
		{
			name: "section only a space",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = " "
				return p
			},
			changed:     validChanged(),
			wantErrPart: "section: must name a heading",
		},
		{
			name: "section only hashes and two spaces",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = "#  "
				return p
			},
			changed:     validChanged(),
			wantErrPart: "section: must name a heading",
		},
		{
			name: "section only unicode spaces",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = "#\u00a0\u2003"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "section: must name a heading",
		},
		{
			name: "section control character",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = "Com\x00mands"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "section: must be one line",
		},
		{
			name: "index entry newline",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = ""
				p.IndexEntry = "Setup\n- [x] fake"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "index_entry: must be one line",
		},
		{
			name: "anchor file not changed",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Anchor.File = "backend/cmd/server/other.go"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "anchor.file",
		},
		{
			name: "anchor line outside hunks is placed by the gate",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Anchor.Line = 100
				return p
			},
			changed: validChanged(),
		},
		{
			name:     "anchor on a removed file",
			proposal: validProposal,
			changed: func() []review.ChangedFile {
				c := validChanged()
				for i := range c {
					c[i].Removed = true
				}
				return c
			}(),
			wantErrPart: "no head-side lines",
		},
		{
			name: "reason empty",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Reason = ""
				return p
			},
			changed:     validChanged(),
			wantErrPart: "reason: must not be empty",
		},
		{
			name: "reason multi-line",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Reason = "line one\nline two"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "reason: must be one line",
		},
		{
			name: "content empty",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Content = ""
				return p
			},
			changed:     validChanged(),
			wantErrPart: "content: must not be empty",
		},
		{
			name: "index entry set with section",
			proposal: func() review.Proposal {
				p := validProposal()
				p.IndexEntry = "Setup guide"
				return p
			},
			changed:     validChanged(),
			wantErrPart: "index_entry",
		},
		{
			name: "index entry missing without section",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Section = ""
				return p
			},
			changed:     validChanged(),
			wantErrPart: "index_entry",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p := tt.proposal()
			err := p.Validate(tt.changed)

			if tt.wantErrPart == "" {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", p, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
				t.Fatalf("Validate(%+v) = %v, want error containing %q", p, err, tt.wantErrPart)
			}
		})
	}
}

func TestProposalValidateTarget(t *testing.T) {
	t.Parallel()

	if err := validProposal().ValidateTarget(); err != nil {
		t.Fatalf("ValidateTarget(valid) = %v, want nil", err)
	}

	p := validProposal()
	p.DocPath = "docs/x.txt"
	p.Section = "a\nb"
	err := p.ValidateTarget()
	if err == nil || !strings.Contains(err.Error(), "doc_path") || !strings.Contains(err.Error(), "section") {
		t.Fatalf("ValidateTarget(%+v) = %v, want doc_path and section errors", p, err)
	}
}

func TestAnchorSnap(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{
		{Path: "a.go", Hunks: []review.LineRange{{Start: 10, End: 20}, {Start: 30, End: 40}}},
		{Path: "gone.go", Removed: true, Hunks: []review.LineRange{{Start: 1, End: 5}}},
		{Path: "empty.go"},
	}

	tests := []struct {
		name   string
		anchor review.Anchor
		want   int
		wantOK bool
	}{
		{name: "inside a hunk", anchor: review.Anchor{File: "a.go", Line: 15}, want: 15, wantOK: true},
		{name: "before the first hunk", anchor: review.Anchor{File: "a.go", Line: 3}, want: 10, wantOK: true},
		{name: "between hunks nearer the first", anchor: review.Anchor{File: "a.go", Line: 22}, want: 20, wantOK: true},
		{name: "between hunks nearer the second", anchor: review.Anchor{File: "a.go", Line: 28}, want: 30, wantOK: true},
		{name: "between hunks tie takes the earlier line", anchor: review.Anchor{File: "a.go", Line: 25}, want: 20, wantOK: true},
		{name: "after the last hunk", anchor: review.Anchor{File: "a.go", Line: 99}, want: 40, wantOK: true},
		{name: "line zero", anchor: review.Anchor{File: "a.go", Line: 0}, want: 10, wantOK: true},
		{name: "negative line", anchor: review.Anchor{File: "a.go", Line: -4}, want: 10, wantOK: true},
		{name: "file not changed", anchor: review.Anchor{File: "other.go", Line: 7}, want: 7},
		{name: "removed file", anchor: review.Anchor{File: "gone.go", Line: 9}, want: 9},
		{name: "file without hunks", anchor: review.Anchor{File: "empty.go", Line: 9}, want: 9},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tc.anchor.Snap(changed)
			if got.File != tc.anchor.File || got.Line != tc.want || ok != tc.wantOK {
				t.Errorf("Snap(%+v) = %+v, %v, want line %d, %v", tc.anchor, got, ok, tc.want, tc.wantOK)
			}
			// Validation accepts exactly the anchors Snap can place, so a valid
			// proposal always gets its comment on a diff line.
			p := validProposal()
			p.Anchor = tc.anchor
			if valid := p.Validate(changed) == nil; valid != ok {
				t.Errorf("Validate accepts %+v = %v, but Snap ok = %v", tc.anchor, valid, ok)
			}
		})
	}
}

func TestAnchorSnapTieIgnoresHunkOrder(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 30, End: 40}, {Start: 10, End: 20}}}}
	if got, _ := (review.Anchor{File: "a.go", Line: 25}).Snap(changed); got.Line != 20 {
		t.Errorf("Snap(line 25) = line %d, want 20 (the earlier line on a tie)", got.Line)
	}
}
