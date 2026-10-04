package actions_test

import (
	"errors"
	"testing"
	"time"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
	"github.com/mrkizildag/docs-agent/backend/internal/review/actions"
)

// Validation criteria 2 and 4: a result that cannot become a valid check (no reason, bad proposal) is rejected.
func TestCollectRejectsSchemaViolations(t *testing.T) {
	t.Parallel()

	completion := review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", RunID: 99, Nonce: "n1"}
	wrongType := validProposal()
	wrongType["anchor"] = map[string]any{"file": "main.go", "line": "three"}
	missingContent := validProposal()
	delete(missingContent, "content")

	tests := []struct {
		name   string
		output map[string]any
	}{
		{name: "no proposals and no reason", output: map[string]any{"no_impact_reason": "", "proposals": []any{}}},
		{name: "no proposals and a blank reason", output: map[string]any{"no_impact_reason": "  \n", "proposals": []any{}}},
		{name: "anchor line not an integer", output: map[string]any{"proposals": []any{wrongType}}},
		{name: "proposal missing content", output: map[string]any{"proposals": []any{missingContent}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeAPI{
				artifact: artifact(t, "abc", "n1", map[string]any{"structured_output": tc.output}),
				changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
			}
			got, err := actions.New(api, time.Minute).Collect(t.Context(), completion)
			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) {
				t.Fatalf("Collect() = %+v, %v; want *review.InvalidResultError", got, err)
			}
		})
	}
}
