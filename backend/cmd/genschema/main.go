// Command genschema writes the JSON Schema for review.Proposal to the path
// given as its single argument.
package main

import (
	"fmt"
	"os"

	"github.com/mrkizildag/docs-agent/backend/internal/review"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: %s <output-path>", args[0])
	}

	schema, err := review.ProposalSchema()
	if err != nil {
		return fmt.Errorf("generate proposal schema: %w", err)
	}

	if err := os.WriteFile(args[1], schema, 0o600); err != nil { //nolint:gosec // the path is the developer's own argument
		return fmt.Errorf("write schema to %s: %w", args[1], err)
	}
	return nil
}
