package review

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// ProposalSchema returns the JSON Schema for Proposal, inferred by
// reflection from the Go type.
func ProposalSchema() ([]byte, error) {
	schema, err := jsonschema.For[Proposal](nil)
	if err != nil {
		return nil, fmt.Errorf("infer schema for review.Proposal: %w", err)
	}
	annotate(schema)

	out, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal schema for review.Proposal: %w", err)
	}
	return append(out, '\n'), nil
}

// The jsonschema tag only carries descriptions, so the docs/ rule is added here.
func annotate(schema *jsonschema.Schema) {
	schema.Properties["doc_path"].Pattern = "^docs/"
}
