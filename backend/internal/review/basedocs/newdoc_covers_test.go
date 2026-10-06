package basedocs_test

import (
	"testing"
	"testing/fstest"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
)

func TestNewDocCoversOnlyCountsUncoveredChangedFiles(t *testing.T) {
	t.Parallel()

	base := fstest.MapFS{"docs/x.md": {Data: []byte("---\ncovers:\n  - \"covered/*.go\"\n---\n# X\n")}}
	changed := []review.ChangedFile{
		{Path: "covered/a.go", Hunks: hunk(1)},
		{Path: "gone.go", Removed: true},
		{Path: "docs/notes.md", Hunks: hunk(1)},
		{Path: "feature/new.go", Hunks: hunk(1)},
	}
	sel, err := basedocs.Select(base, changed)
	if err != nil {
		t.Fatalf("Select() = %v", err)
	}
	newDoc := func(covers string) review.Proposal {
		return review.Proposal{
			DocPath:    "docs/new.md",
			Anchor:     review.Anchor{File: "feature/new.go", Line: 2},
			Reason:     "new feature",
			Content:    "---\ntitle: New\nsummary: About new.\ncovers:\n  - " + covers + "\n---\n# New\n",
			IndexEntry: "- [New](new.md): about new.",
		}
	}

	tests := []struct {
		covers  string
		wantErr bool
	}{
		{covers: "feature/*.go"},
		{covers: "covered/*.go", wantErr: true},
		{covers: "gone.go", wantErr: true},
		{covers: "docs/notes.md", wantErr: true},
		{covers: "nowhere/*.go", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.covers, func(t *testing.T) {
			t.Parallel()

			err := sel.ValidateProposal(newDoc(tc.covers), changed)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateProposal(covers %q) = %v, wantErr %t", tc.covers, err, tc.wantErr)
			}
		})
	}
}
