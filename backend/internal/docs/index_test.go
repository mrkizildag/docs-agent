package docs_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
)

func TestIndexEntry(t *testing.T) {
	t.Parallel()

	readme := []byte("# Docs\n\n## Index\n\n- [Arch](architecture.md): parts.\n- [Q](features/q.md#top): q.\n- [Other](features/other.md): see [Q](features/q.md).\n\nfeatures/z.md\n")

	tests := []struct {
		name    string
		docPath string
		want    string
		wantOK  bool
	}{
		{name: "top-level", docPath: "docs/architecture.md", want: "- [Arch](architecture.md): parts.", wantOK: true},
		{name: "link with anchor", docPath: "docs/features/q.md", want: "- [Q](features/q.md#top): q.", wantOK: true},
		{name: "unlisted", docPath: "docs/features/z.md"},
		{name: "outside docs", docPath: "other/architecture.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := docs.IndexEntry(readme, tc.docPath)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("IndexEntry(%q) = %q, %v; want %q, %v", tc.docPath, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
