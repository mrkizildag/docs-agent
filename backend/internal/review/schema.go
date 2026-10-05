package review

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// StructuredOutput is what the Actions runner's Claude Code run returns as
// structured_output: either proposals or a reason there are none.
type StructuredOutput struct {
	NoImpactReason string     `json:"no_impact_reason" jsonschema:"Why the PR needs no doc change; empty when proposals is non-empty."`
	Proposals      []Proposal `json:"proposals" jsonschema:"Doc changes the PR needs; empty when the PR has no doc impact."`
}

// ProposalSchema returns the JSON Schema for Proposal, inferred by
// reflection from the Go type.
func ProposalSchema() ([]byte, error) {
	schema, err := jsonschema.For[Proposal](nil)
	if err != nil {
		return nil, fmt.Errorf("infer schema for review.Proposal: %w", err)
	}
	annotate(schema)
	return marshalSchema(schema, "review.Proposal")
}

// ResultSchema returns the JSON Schema for StructuredOutput.
func ResultSchema() ([]byte, error) {
	schema, err := jsonschema.For[StructuredOutput](nil)
	if err != nil {
		return nil, fmt.Errorf("infer schema for review.StructuredOutput: %w", err)
	}
	proposals := schema.Properties["proposals"]
	proposals.Type, proposals.Types = "array", nil
	annotate(proposals.Items)
	return marshalSchema(schema, "review.StructuredOutput")
}

func marshalSchema(schema *jsonschema.Schema, name string) ([]byte, error) {
	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal schema for %s: %w", name, err)
	}
	return append(out, '\n'), nil
}

// The jsonschema tag only carries descriptions, so the doc path rule is added here.
func annotate(schema *jsonschema.Schema) {
	schema.Properties["doc_path"].Pattern = `^docs/.*\.mdx?$`
}
