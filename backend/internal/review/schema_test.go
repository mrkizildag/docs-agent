package review_test

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

func TestProposalSchemaUpToDate(t *testing.T) {
	t.Parallel()

	want, err := review.ProposalSchema()
	if err != nil {
		t.Fatalf("ProposalSchema() error = %v", err)
	}

	got, err := os.ReadFile("../../../action/proposal.schema.json")
	if err != nil {
		t.Fatalf("read action/proposal.schema.json: %v", err)
	}

	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("action/proposal.schema.json is stale; run make generate (-want +got):\n%s", diff)
	}
}
