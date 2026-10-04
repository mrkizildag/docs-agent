package docs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
)

type docSummary struct {
	Path    string
	Title   string
	Summary string
	Covers  []string
}

func summarize(all []docs.Doc) []docSummary {
	out := make([]docSummary, len(all))
	for i, d := range all {
		out[i] = docSummary{Path: d.Path, Title: d.Title, Summary: d.Summary, Covers: d.Covers}
	}

	return out
}

func problemPaths(problems []docs.Problem) []string {
	out := make([]string, len(problems))
	for i, p := range problems {
		out[i] = p.Path
	}

	return out
}

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		dir          string
		wantDocs     []docSummary
		wantProblems []string
	}{
		{
			name: "nested folders",
			dir:  "testdata/nested",
			wantDocs: []docSummary{
				{Path: "docs/features/x/y.md", Title: "Nested feature", Summary: "A doc nested two folders deep.", Covers: []string{"nested/**"}},
			},
		},
		{
			name: "overlapping covers",
			dir:  "testdata/overlap",
			wantDocs: []docSummary{
				{Path: "docs/a.md", Title: "Doc A", Summary: "Covers shared file, variant A.", Covers: []string{"shared.go"}},
				{Path: "docs/b.md", Title: "Doc B", Summary: "Covers shared file, variant B.", Covers: []string{"shared.go"}},
			},
		},
		{
			name: "globstar covers",
			dir:  "testdata/globstar",
			wantDocs: []docSummary{
				{Path: "docs/g.md", Title: "Backend doc", Summary: "Covers everything under backend.", Covers: []string{"backend/**"}},
			},
		},
		{
			name: "root-only star covers",
			dir:  "testdata/rootstar",
			wantDocs: []docSummary{
				{Path: "docs/r.md", Title: "Root Go files", Summary: "Covers only root-level Go files.", Covers: []string{"*.go"}},
			},
		},
		{
			name: "no covers never matches",
			dir:  "testdata/nocovers",
			wantDocs: []docSummary{
				{Path: "docs/empty-list.md", Title: "Empty covers list", Summary: "Explicit empty covers list.", Covers: []string{}},
				{Path: "docs/no-covers-field.md", Title: "No covers field", Summary: "Frontmatter omits covers entirely.", Covers: nil},
			},
		},
		{
			name: "bad frontmatter files become problems, good ones still parse",
			dir:  "testdata/problems",
			wantDocs: []docSummary{
				{Path: "docs/good.md", Title: "Good doc", Summary: "Parses fine and still matches.", Covers: []string{"good/**"}},
			},
			wantProblems: []string{
				"docs/invalid-glob.md",
				"docs/invalid-yaml.md",
				"docs/missing-frontmatter.md",
			},
		},
		{
			name: "non-markdown files are ignored",
			dir:  "testdata/nonmd",
			wantDocs: []docSummary{
				{Path: "docs/keep.md", Title: "Keep me", Summary: "A normal markdown doc.", Covers: []string{"keep/**"}},
			},
		},
		{
			name: "no docs folder yields an empty tree",
			dir:  "testdata/nodocs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tree, err := docs.Parse(os.DirFS(tc.dir))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			gotDocs := summarize(tree.Docs)
			if diff := cmp.Diff(tc.wantDocs, gotDocs, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("docs mismatch (-want +got):\n%s", diff)
			}

			gotProblems := problemPaths(tree.Problems)
			if diff := cmp.Diff(tc.wantProblems, gotProblems, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("problem paths mismatch (-want +got):\n%s", diff)
			}
			for _, p := range tree.Problems {
				if p.Err == nil {
					t.Errorf("Problem %q has nil Err", p.Path)
				}
			}
		})
	}
}

func TestParseOwnDocs(t *testing.T) {
	t.Parallel()

	tree, err := docs.Parse(os.DirFS("../../.."))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(tree.Problems) != 0 {
		t.Errorf("Problems = %v, want none", tree.Problems)
	}
	if len(tree.Docs) < 5 {
		t.Errorf("len(Docs) = %d, want at least 5", len(tree.Docs))
	}
}

func TestTreeMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dir     string
		changed []string
		want    []string
	}{
		{
			name:    "nested doc matches a file under its glob root",
			dir:     "testdata/nested",
			changed: []string{"nested/sub/file.go"},
			want:    []string{"docs/features/x/y.md"},
		},
		{
			name:    "unrelated change does not match",
			dir:     "testdata/nested",
			changed: []string{"other/file.go"},
			want:    nil,
		},
		{
			name:    "one changed file matches overlapping docs, each once",
			dir:     "testdata/overlap",
			changed: []string{"shared.go", "shared.go"},
			want:    []string{"docs/a.md", "docs/b.md"},
		},
		{
			name:    "double star matches nested and direct files under the root",
			dir:     "testdata/globstar",
			changed: []string{"backend/x.go", "backend/a/b/c.go"},
			want:    []string{"docs/g.md"},
		},
		{
			name:    "double star does not match outside its root",
			dir:     "testdata/globstar",
			changed: []string{"other/x.go"},
			want:    nil,
		},
		{
			name:    "single star matches root-level files only",
			dir:     "testdata/rootstar",
			changed: []string{"root.go"},
			want:    []string{"docs/r.md"},
		},
		{
			name:    "single star does not match a nested file",
			dir:     "testdata/rootstar",
			changed: []string{"sub/root.go"},
			want:    nil,
		},
		{
			name:    "docs with no covers never match",
			dir:     "testdata/nocovers",
			changed: []string{"anything", "really/anything.go"},
			want:    nil,
		},
		{
			name:    "a doc with a bad sibling still matches",
			dir:     "testdata/problems",
			changed: []string{"good/thing.go"},
			want:    []string{"docs/good.md"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tree, err := docs.Parse(os.DirFS(tc.dir))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}

			got := tree.Match(tc.changed)
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Match() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseProblemsCarryPathAndCause(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"docs/ok.md":         {Data: []byte("---\ntitle: ok\ncovers: [\"x/**\"]\n---\n# H\n")},
		"docs/deep/a/b/c.md": {Data: []byte("---\ntitle: deep\ncovers: [\"y/*.go\"]\n---\n")},
		"docs/title-list.md": {Data: []byte("---\ntitle: [a, b]\n---\n")},
		"docs/covers-map.md": {Data: []byte("---\ncovers: {a: b}\n---\n")},
		"docs/empty.md":      {Data: []byte("")},
		"docs/scalar-fm.md":  {Data: []byte("---\njust a string\n---\n")},
		"docs/notes.txt":     {Data: []byte("ignored")},
	}

	tree, err := docs.Parse(fsys)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(tree.Docs) != 2 || tree.Docs[0].Path != "docs/deep/a/b/c.md" || tree.Docs[1].Path != "docs/ok.md" {
		t.Errorf("Docs = %+v, want deep and ok", tree.Docs)
	}

	wantProblems := []string{"docs/covers-map.md", "docs/empty.md", "docs/scalar-fm.md", "docs/title-list.md"}
	if len(tree.Problems) != len(wantProblems) {
		t.Fatalf("Problems = %+v, want %v", tree.Problems, wantProblems)
	}

	for i, p := range tree.Problems {
		if p.Path != wantProblems[i] || p.Err == nil || !strings.Contains(p.Err.Error(), p.Path) {
			t.Errorf("Problems[%d] = %+v, want path %s with cause naming it", i, p, wantProblems[i])
		}
	}

	if got := tree.Match([]string{"y/z.go", "x/q"}); len(got) != 2 {
		t.Errorf("Match = %v, want both docs", got)
	}
}

func TestParseEmptyFS(t *testing.T) {
	t.Parallel()

	tree, err := docs.Parse(fstest.MapFS{})
	if err != nil || len(tree.Docs) != 0 || len(tree.Problems) != 0 {
		t.Errorf("Parse(empty) = %+v, %v; want empty tree, nil", tree, err)
	}
}

func TestParseSkipsSymlinks(t *testing.T) {
	t.Parallel()

	const doc = "---\ntitle: T\ncovers: [\"a/**\"]\n---\n"

	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.md")
	if err := os.WriteFile(outside, []byte(doc), 0o600); err != nil {
		t.Fatalf("write outside doc: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "repo", "docs"), 0o750); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	realDoc := filepath.Join(dir, "repo", "docs", "realDoc.md")
	if err := os.WriteFile(realDoc, []byte(doc), 0o600); err != nil {
		t.Fatalf("write realDoc doc: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "repo", "docs", "link.md")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(realDoc, filepath.Join(dir, "repo", "docs", "inside.md")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	tree, err := docs.Parse(os.DirFS(filepath.Join(dir, "repo")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if diff := cmp.Diff([]string{"docs/realDoc.md"}, docPaths(tree.Docs)); diff != "" {
		t.Errorf("doc paths mismatch (-want +got):\n%s", diff)
	}
	if len(tree.Problems) != 0 {
		t.Errorf("Problems = %+v, want none", tree.Problems)
	}
}

func TestParseLargeFileBecomesProblem(t *testing.T) {
	t.Parallel()

	const maxDocBytes = 1 << 20

	head := "---\ntitle: big\ncovers: [\"a/**\"]\n---\n"
	fsys := fstest.MapFS{
		"docs/big.md":   {Data: []byte(head + strings.Repeat("x", maxDocBytes+1-len(head)))},
		"docs/exact.md": {Data: []byte(head + strings.Repeat("x", maxDocBytes-len(head)))},
		"docs/ok.md":    {Data: []byte(head)},
	}

	tree, err := docs.Parse(fsys)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if diff := cmp.Diff([]string{"docs/exact.md", "docs/ok.md"}, docPaths(tree.Docs)); diff != "" {
		t.Errorf("doc paths mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"docs/big.md"}, problemPaths(tree.Problems)); diff != "" {
		t.Errorf("problem paths mismatch (-want +got):\n%s", diff)
	}
}

func docPaths(all []docs.Doc) []string {
	out := make([]string, len(all))
	for i, d := range all {
		out[i] = d.Path
	}

	return out
}
