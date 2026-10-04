package review_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
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
			name: "anchor line outside hunks",
			proposal: func() review.Proposal {
				p := validProposal()
				p.Anchor.Line = 100
				return p
			},
			changed:     validChanged(),
			wantErrPart: "anchor.line",
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
