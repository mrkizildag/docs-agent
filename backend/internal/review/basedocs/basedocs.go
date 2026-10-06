// Package basedocs selects the docs a PR may affect from the docs tree at the
// PR's base commit, and proposes restoring a covering doc the PR deleted.
package basedocs

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
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
// the PR deleted a covering doc that can be restored; Candidates and Uncovered
// are then nil. Uncovered lists the changed files no base doc covers.
type Selection struct {
	Candidates []string
	Uncovered  []string
	Restores   []review.Proposal
}

// NothingToReview is the "No doc impact" reason when every changed file is
// under docs/ or removed, and no doc covers it.
const NothingToReview = "the PR only edits docs or removes files no doc covers"

// Empty reports whether the selection has no candidate docs and no uncovered
// files, so there is nothing to review. Runners call it after handling Restores.
func (s Selection) Empty() bool {
	return len(s.Candidates) == 0 && len(s.Uncovered) == 0
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
	uncovered := baseTree.Uncovered(changes)
	if len(deleted) == 0 {
		return Selection{Candidates: candidates, Uncovered: uncovered}, nil
	}
	restores, err := restores(baseFS, baseTree, deleted, files)
	if err != nil {
		return Selection{}, err
	}
	if len(restores) > 0 {
		return Selection{Restores: restores}, nil
	}
	return Selection{Candidates: candidates, Uncovered: uncovered}, nil
}

// ValidateProposal checks p against the changed files and, for a new doc
// (empty section), that it follows the docs conventions (title, summary, covers,
// relative links to docs of repo, "owner/repo") and its covers match an
// uncovered file.
// Restores don't go through it: they recreate a base doc that already covered
// its files.
func (s Selection) ValidateProposal(p review.Proposal, changed []review.ChangedFile, repo string) error {
	if err := p.Validate(changed); err != nil {
		return err //nolint:wrapcheck // the caller names the proposal.
	}
	if p.Section != "" {
		return nil
	}
	if path.Ext(p.DocPath) != ".md" {
		return fmt.Errorf("doc_path %q: a new doc must end in .md", p.DocPath)
	}
	doc, err := docs.CheckNewDoc(p.DocPath, []byte(p.Content), repo)
	if err != nil {
		return err //nolint:wrapcheck // CheckNewDoc names the doc.
	}
	if err := docs.CheckDocLinks([]byte(p.IndexEntry), repo); err != nil {
		return fmt.Errorf("index_entry: %w", err)
	}
	if !slices.ContainsFunc(s.Uncovered, func(f string) bool { return doc.CoversAny(f) }) {
		return fmt.Errorf("covers match none of the uncovered changed files %q", s.Uncovered)
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
	readme, err := fs.ReadFile(baseFS, "docs/README.md")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read base docs/README.md: %w", err)
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
