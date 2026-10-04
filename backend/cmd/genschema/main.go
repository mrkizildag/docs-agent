// Command genschema writes the JSON Schemas for review.Proposal and
// review.StructuredOutput to the two paths given as its arguments.
package main

import (
	"fmt"
	"os"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: %s <proposal-schema-path> <result-schema-path>", args[0])
	}

	outputs := []struct {
		name     string
		generate func() ([]byte, error)
		path     string
	}{
		{"proposal", review.ProposalSchema, args[1]},
		{"result", review.ResultSchema, args[2]},
	}
	for _, o := range outputs {
		schema, err := o.generate()
		if err != nil {
			return fmt.Errorf("generate %s schema: %w", o.name, err)
		}
		if err := os.WriteFile(o.path, schema, 0o600); err != nil { //nolint:gosec // the path is the developer's own argument
			return fmt.Errorf("write schema to %s: %w", o.path, err)
		}
	}
	return nil
}
