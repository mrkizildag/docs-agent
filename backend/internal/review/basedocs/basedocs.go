// Package basedocs selects the docs a PR may affect from the docs tree at the
// PR's base commit, proposes restoring a covering doc the PR deleted, and fills
// a proposal's original section from the doc at head.
package basedocs

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// MaxCandidates caps the candidate docs a runner triages.
const MaxCandidates = 10

const maxRestoreReasonPaths = 3

// Selection is the base-docs outcome for a PR. Restores is non-empty only when
// the PR deleted a covering doc that can be restored; Candidates is then nil.
// Problems are the base docs that failed to parse and so cannot be candidates.
type Selection struct {
	Candidates []string
	Restores   []review.Proposal
	Problems   []docs.Problem
}

// Select matches files against the docs in baseFS.
func Select(baseFS fs.FS, files []review.ChangedFile) (Selection, error) {
	baseTree, err := docs.Parse(baseFS)
	if err != nil {
		return Selection{}, fmt.Errorf("parse base docs: %w", err)
	}
	changes := make([]docs.Change, len(files))
	for i, f := range files {
		changes[i] = docs.Change{Path: f.Path, PreviousPath: f.PreviousPath, Removed: f.Removed}
	}
	candidates, deleted := baseTree.Candidates(changes)
	selection := Selection{Candidates: candidates, Problems: baseTree.Problems}
	if len(deleted) == 0 {
		return selection, nil
	}
	restores, err := restores(baseFS, baseTree, deleted, files)
	if err != nil {
		return Selection{}, err
	}
	if len(restores) > 0 {
		selection.Candidates, selection.Restores = nil, restores
	}
	return selection, nil
}

// CheckScaffold reports why docs are not a usable starting docs folder; both
// runners apply it to the docs a model submits.
func CheckScaffold(d review.ScaffoldDocs) error {
	files := d.Files()
	scaffold := make([]docs.ScaffoldFile, len(files))
	for i, f := range files {
		scaffold[i] = docs.ScaffoldFile(f)
	}
	if err := docs.CheckScaffold(scaffold, review.IndexPath, review.IndexHeading); err != nil {
		return fmt.Errorf("scaffold docs: %w", err)
	}
	return nil
}

// restores proposes recreating each deleted base doc from its text at the base
// commit. It anchors to the first hunk of a covered file that survives the PR,
// else to the first hunk of any surviving changed file, so a doc is still
// restored when its covered files have no head-side hunk. It proposes nothing
// for a doc when every covered file is deleted too: the feature and its doc
// went together.
func restores(baseFS fs.FS, baseTree docs.Tree, deleted []string, files []review.ChangedFile) ([]review.Proposal, error) {
	readme, err := fs.ReadFile(baseFS, review.IndexPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read base %s: %w", review.IndexPath, err)
	}

	var proposals []review.Proposal
	for _, docPath := range deleted {
		i := slices.IndexFunc(baseTree.Docs, func(d docs.Doc) bool { return d.Path == docPath })
		base := baseTree.Docs[i]

		var covered []review.ChangedFile
		for _, f := range files {
			if base.CoversAny(changedPaths(f)...) {
				covered = append(covered, f)
			}
		}

		if !slices.ContainsFunc(covered, func(f review.ChangedFile) bool { return !f.Removed }) {
			continue
		}
		anchor, ok := restoreAnchor(covered, files)
		if !ok {
			return nil, fmt.Errorf("restore %s: no changed file has a hunk to anchor to", docPath)
		}

		entry, _ := docs.IndexEntry(readme, docPath)
		p := restoreProposal(docPath, string(base.Source), entry, anchor, covered)
		if err := p.Validate(files); err != nil {
			return nil, fmt.Errorf("restore %s: %w", docPath, err)
		}
		proposals = append(proposals, p)
	}
	return proposals, nil
}

func restoreAnchor(covered, files []review.ChangedFile) (review.Anchor, bool) {
	for _, group := range [][]review.ChangedFile{covered, files} {
		for _, f := range group {
			if !f.Removed && len(f.Hunks) > 0 {
				return review.Anchor{File: f.Path, Line: f.Hunks[0].Start}, true
			}
		}
	}
	return review.Anchor{}, false
}

// restoreProposal proposes recreating docPath from source, its content at the
// base commit. Paths come from the PR, so the reason quotes them to stay on
// one line.
func restoreProposal(docPath, source, indexEntry string, anchor review.Anchor, covered []review.ChangedFile) review.Proposal {
	quoted := make([]string, 0, min(len(covered), maxRestoreReasonPaths))
	for _, f := range covered[:min(len(covered), maxRestoreReasonPaths)] {
		quoted = append(quoted, strconv.Quote(f.Path))
	}
	list := strings.Join(quoted, ", ")
	if len(covered) > maxRestoreReasonPaths {
		list += ", ..."
	}
	if indexEntry == "" {
		rel := strings.TrimPrefix(docPath, "docs/")
		indexEntry = "- [" + rel + "](" + rel + ")"
	}

	return review.Proposal{
		DocPath:    docPath,
		Anchor:     anchor,
		Reason:     strconv.Quote(docPath) + " was deleted but covers " + list,
		Content:    source,
		IndexEntry: indexEntry,
	}
}

func changedPaths(f review.ChangedFile) []string {
	if f.PreviousPath == "" {
		return []string{f.Path}
	}
	return []string{f.Path, f.PreviousPath}
}
