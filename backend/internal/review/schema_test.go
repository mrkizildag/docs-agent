package review_test

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestSchemasUpToDate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		generate func() ([]byte, error)
		path     string
	}{
		{"proposal", review.ProposalSchema, "../../../action/proposal.schema.json"},
		{"result", review.ResultSchema, "../../../action/result.schema.json"},
		{"scaffold", review.ScaffoldSchema, "../../../action/scaffold.schema.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want, err := tc.generate()
			if err != nil {
				t.Fatalf("generate %s schema: %v", tc.name, err)
			}

			got, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}

			if diff := cmp.Diff(string(want), string(got)); diff != "" {
				t.Errorf("%s is stale; run make generate (-want +got):\n%s", tc.path, diff)
			}
		})
	}
}
