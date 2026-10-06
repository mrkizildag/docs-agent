package review_test

import (
	"encoding/json"
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

func TestProposalsArgsSchema(t *testing.T) {
	t.Parallel()

	raw, err := review.ProposalsArgsSchema()
	if err != nil {
		t.Fatalf("ProposalsArgsSchema: %v", err)
	}

	var got struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties struct {
			Proposals struct {
				Type  string `json:"type"`
				Items struct {
					Properties struct {
						DocPath struct {
							Pattern string `json:"pattern"`
						} `json:"doc_path"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"proposals"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	if got.Type != "object" || got.Properties.Proposals.Type != "array" || len(got.Required) != 1 || got.Required[0] != "proposals" {
		t.Errorf("schema = %s, want an object requiring a proposals array", raw)
	}
	if got.Properties.Proposals.Items.Properties.DocPath.Pattern == "" {
		t.Errorf("schema = %s, want annotated proposal items", raw)
	}
}
