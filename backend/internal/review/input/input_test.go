package input_test

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
)

func TestNew(t *testing.T) {
	t.Parallel()

	req := review.Request{
		BaseSHA: "base",
		ChangedFiles: []review.ChangedFile{
			{Path: "b.go", Hunks: []review.LineRange{{Start: 1, End: 3}, {Start: 9, End: 9}}},
			{Path: "gone.go", Removed: true},
			{Path: "bin.png"},
			{Path: "a.go", Hunks: []review.LineRange{{Start: 4, End: 5}}},
		},
	}
	sel := basedocs.Selection{Candidates: []string{"docs/z.md", "docs/a.md"}, Uncovered: []string{"b.go", "a.go"}}

	got := input.New(req, sel)

	want := input.Input{
		BaseSHA:    "base",
		Candidates: []string{"docs/z.md", "docs/a.md"},
		Uncovered:  []string{"b.go", "a.go"},
		Files: []input.File{
			{Path: "b.go", Ranges: []review.LineRange{{Start: 1, End: 3}, {Start: 9, End: 9}}},
			{Path: "gone.go", Ranges: []review.LineRange{}},
			{Path: "bin.png", Ranges: []review.LineRange{}},
			{Path: "a.go", Ranges: []review.LineRange{{Start: 4, End: 5}}},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("New() (-want +got):\n%s", diff)
	}
}

func TestNewEncodesEmptyAsArrays(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(input.New(review.Request{}, basedocs.Selection{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"base_sha":"","review":[],"uncovered":[],"files":[]}`; string(b) != want {
		t.Errorf("JSON = %s, want %s", b, want)
	}
}

func TestInputJSON(t *testing.T) {
	t.Parallel()

	in := input.New(
		review.Request{BaseSHA: "abc", ChangedFiles: []review.ChangedFile{{Path: "a.go", Hunks: []review.LineRange{{Start: 1, End: 3}}}, {Path: "x.go", Removed: true}}},
		basedocs.Selection{Candidates: []string{"docs/a.md"}, Uncovered: []string{"a.go"}},
	)
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"base_sha":"abc","review":["docs/a.md"],"uncovered":["a.go"],"files":[{"path":"a.go","ranges":[{"start":1,"end":3}]},{"path":"x.go","ranges":[]}]}`
	if string(b) != want {
		t.Errorf("JSON = %s, want %s", b, want)
	}
}
