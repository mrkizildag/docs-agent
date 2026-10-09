// Package input is what every review runner hands its agent about a PR,
// decided once from the request and the base-docs selection.
package input

import (
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
)

// Input is the runner-agnostic view of a PR. Its slices are never nil, so it
// encodes to JSON arrays.
type Input struct {
	// BaseSHA is the PR's merge base.
	BaseSHA string `json:"base_sha"`
	// Candidates are the docs to review: those whose covers match at the base.
	Candidates []string `json:"review"`
	// Uncovered are the changed files no doc covers.
	Uncovered []string `json:"uncovered"`
	// Files is every changed file, in request order.
	Files []File `json:"files"`
}

// File is a changed file and the head-side lines a review comment may anchor on.
type File struct {
	Path string `json:"path"`
	// Ranges is empty for a removed file or one with no head-side lines.
	Ranges []review.LineRange `json:"ranges"`
}

// New builds the Input for req from the selection made at its merge base.
func New(req review.Request, sel basedocs.Selection) Input {
	files := make([]File, len(req.ChangedFiles))
	for i, f := range req.ChangedFiles {
		files[i] = File{Path: f.Path, Ranges: append([]review.LineRange{}, f.Hunks...)}
	}
	return Input{
		BaseSHA:    req.BaseSHA,
		Candidates: append([]string{}, sel.Candidates...),
		Uncovered:  append([]string{}, sel.Uncovered...),
		Files:      files,
	}
}
