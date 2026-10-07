package actions_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestCollectFinalizeCriteria(t *testing.T) {
	t.Parallel()

	changed := []review.ChangedFile{
		{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}},
		{Path: "gone.go", Removed: true},
	}
	const dupDoc = "---\ntitle: A\nsummary: S.\ncovers:\n  - main.go\n---\n# A\n\n## Usage\none\n\n## Usage\ntwo\n"

	removedAnchor := validProposal()
	removedAnchor["anchor"] = map[string]any{"file": "gone.go", "line": 1}

	many := make([]any, 40)
	for i := range many {
		p := validProposal()
		p["section"] = "Missing heading " + strings.Repeat("x", 100)
		many[i] = p
	}

	tests := []struct {
		name    string
		files   map[string][]byte
		output  map[string]any
		wantErr string
		check   func(t *testing.T, got review.Result, err error)
	}{
		{
			name:    "duplicate heading is ambiguous",
			files:   map[string][]byte{"docs/a.md": []byte(dupDoc)},
			output:  map[string]any{"proposals": []any{validProposal()}},
			wantErr: "ambiguous",
		},
		{
			name:    "anchor on a removed file",
			files:   map[string][]byte{"docs/a.md": []byte(usageDoc)},
			output:  map[string]any{"proposals": []any{removedAnchor}},
			wantErr: "proposal 0:",
		},
		{
			name:   "many bad proposals stay bounded",
			files:  map[string][]byte{"docs/a.md": []byte(usageDoc)},
			output: map[string]any{"proposals": many},
			check: func(t *testing.T, _ review.Result, err error) {
				var invalid *review.InvalidResultError
				if !errors.As(err, &invalid) {
					t.Fatalf("err = %v, want InvalidResultError", err)
				}
				if n := len(err.Error()); n > 1100 {
					t.Errorf("error length %d, want <= 1100", n)
				}
			},
		},
		{
			name:   "long no-impact reason is capped to one line",
			output: map[string]any{"proposals": []any{}, "no_impact_reason": strings.Repeat("word\n", 500)},
			check: func(t *testing.T, got review.Result, err error) {
				if err != nil {
					t.Fatal(err)
				}
				ni, ok := got.Verdict.(review.NoImpact)
				if !ok || len(ni.Reason) > 303 || strings.Contains(ni.Reason, "\n") {
					t.Errorf("verdict = %#v", got.Verdict)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeAPI{
				artifact: artifact(t, "abc", "n1", map[string]any{"structured_output": tc.output}),
				changed:  changed,
				files:    tc.files,
			}
			got, err := newRunner(api).Collect(t.Context(), review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", Nonce: "n1"})
			if tc.check != nil {
				tc.check(t, got, err)
				return
			}
			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want InvalidResultError containing %q", err, tc.wantErr)
			}
		})
	}
}
