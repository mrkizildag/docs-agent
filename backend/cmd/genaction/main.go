// Command genaction writes the Actions agent's files into the directory given
// as its argument: the proposal, result and scaffold JSON Schemas and the
// review and scaffold prompts.
package main

import (
	"fmt"
	"os"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/instructions"
)

type output struct {
	file    string
	content []byte
}

func outputs() ([]output, error) {
	var outs []output
	for _, s := range []struct {
		file     string
		generate func() ([]byte, error)
	}{
		{"proposal.schema.json", review.ProposalSchema},
		{"result.schema.json", review.ResultSchema},
		{"scaffold.schema.json", review.ScaffoldSchema},
	} {
		content, err := s.generate()
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", s.file, err)
		}
		outs = append(outs, output{s.file, content})
	}
	return append(outs,
		output{"prompt.md", []byte(instructions.ActionReview())},
		output{"scaffold.md", []byte(instructions.ActionScaffold())},
	), nil
}

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: %s <action-dir>", args[0])
	}

	outs, err := outputs()
	if err != nil {
		return err
	}

	dir, err := os.OpenRoot(args[1])
	if err != nil {
		return fmt.Errorf("open %s: %w", args[1], err)
	}
	defer func() { _ = dir.Close() }() // the root holds no buffered writes; WriteFile closes each file

	for _, o := range outs {
		if err := dir.WriteFile(o.file, o.content, 0o600); err != nil {
			return fmt.Errorf("write %s in %s: %w", o.file, args[1], err)
		}
	}
	return nil
}
