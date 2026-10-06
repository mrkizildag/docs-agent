package docs_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
)

func covering(path string, covers ...string) docs.Doc {
	return docs.Doc{Path: path, Covers: covers}
}

func TestTreeCandidates(t *testing.T) {
	t.Parallel()

	base := docs.Tree{Docs: []docs.Doc{
		covering("docs/a.md", "src/*.go"),
		covering("docs/b.md", "src/*.go", "lib/*.go"),
		covering("docs/c.md", "cmd/*.go"),
		covering("docs/m.mdx", "mdx/*.go"),
	}}

	tests := []struct {
		name           string
		changes        []docs.Change
		wantCandidates []string
		wantDeleted    []string
	}{
		{
			name:           "match by path, deduped, in path order",
			changes:        []docs.Change{{Path: "lib/x.go"}, {Path: "src/x.go"}},
			wantCandidates: []string{"docs/a.md", "docs/b.md"},
		},
		{
			name:           "match by previous path",
			changes:        []docs.Change{{Path: "moved/x.go", PreviousPath: "cmd/x.go"}},
			wantCandidates: []string{"docs/c.md"},
		},
		{
			name:           "doc renamed by the PR is reported at its new path",
			changes:        []docs.Change{{Path: "src/x.go"}, {Path: "docs/z.md", PreviousPath: "docs/a.md"}},
			wantCandidates: []string{"docs/b.md", "docs/z.md"},
		},
		{
			name:           "removed doc is deleted",
			changes:        []docs.Change{{Path: "src/x.go"}, {Path: "docs/a.md", Removed: true}},
			wantCandidates: []string{"docs/b.md"},
			wantDeleted:    []string{"docs/a.md"},
		},
		{
			name:           "doc renamed out of docs is deleted",
			changes:        []docs.Change{{Path: "old/a.md", PreviousPath: "docs/a.md"}, {Path: "src/x.go"}},
			wantCandidates: []string{"docs/b.md"},
			wantDeleted:    []string{"docs/a.md"},
		},
		{
			name:           "doc renamed to a non-markdown path is deleted",
			changes:        []docs.Change{{Path: "docs/a.txt", PreviousPath: "docs/a.md"}, {Path: "src/x.go"}},
			wantCandidates: []string{"docs/b.md"},
			wantDeleted:    []string{"docs/a.md"},
		},
		{
			name:           "doc renamed to another mdx path is a candidate at the new path",
			changes:        []docs.Change{{Path: "mdx/x.go"}, {Path: "docs/z.mdx", PreviousPath: "docs/m.mdx"}},
			wantCandidates: []string{"docs/z.mdx"},
		},
		{
			name:    "no match",
			changes: []docs.Change{{Path: "other.go"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			candidates, deleted := base.Candidates(tc.changes)
			if diff := cmp.Diff(tc.wantCandidates, candidates); diff != "" {
				t.Errorf("candidates (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantDeleted, deleted); diff != "" {
				t.Errorf("deleted (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDocCoversAny(t *testing.T) {
	t.Parallel()

	d := covering("docs/a.md", "src/*.go")
	if !d.CoversAny("x.go", "src/b.go") {
		t.Error("CoversAny(x.go, src/b.go) = false, want true")
	}
	if d.CoversAny("x.go", "lib/b.go") {
		t.Error("CoversAny(x.go, lib/b.go) = true, want false")
	}
}
