package review

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// Validate reports every way p is malformed given the files changed in the
// pull request: a bad DocPath, an Anchor outside the diff, missing Reason or
// Content, a multi-line Reason, or an IndexEntry that doesn't match whether
// Section is empty.
func (p Proposal) Validate(changed []ChangedFile) error {
	var errs []error

	if err := validateDocPath(p.DocPath); err != nil {
		errs = append(errs, err)
	}
	if err := validateAnchor(p.Anchor, changed); err != nil {
		errs = append(errs, err)
	}
	if p.Reason == "" {
		errs = append(errs, errors.New("reason: must not be empty"))
	} else if strings.ContainsAny(p.Reason, "\n\r") {
		errs = append(errs, errors.New("reason: must be one line"))
	}
	if p.Content == "" {
		errs = append(errs, errors.New("content: must not be empty"))
	}
	if (p.IndexEntry != "") != (p.Section == "") {
		errs = append(errs, errors.New("index_entry: must be set iff section is empty"))
	}

	return errors.Join(errs...)
}

func validateDocPath(docPath string) error {
	if docPath == "" {
		return errors.New("doc_path: must not be empty")
	}
	if path.IsAbs(docPath) || strings.HasPrefix(docPath, "/") {
		return fmt.Errorf("doc_path %q: must be relative", docPath)
	}
	if path.Clean(docPath) != docPath {
		return fmt.Errorf("doc_path %q: must be a clean path", docPath)
	}
	if strings.Contains(docPath, "..") {
		return fmt.Errorf("doc_path %q: must not contain \"..\"", docPath)
	}
	if docPath == "docs" || !strings.HasPrefix(docPath, "docs/") {
		return fmt.Errorf("doc_path %q: must be under \"docs/\"", docPath)
	}
	return nil
}

func validateAnchor(anchor Anchor, changed []ChangedFile) error {
	for _, file := range changed {
		if file.Path != anchor.File {
			continue
		}
		for _, hunk := range file.Hunks {
			if anchor.Line >= hunk.Start && anchor.Line <= hunk.End {
				return nil
			}
		}
		return fmt.Errorf("anchor.line %d: outside the diff hunks of %q", anchor.Line, anchor.File)
	}
	return fmt.Errorf("anchor.file %q: not a changed file", anchor.File)
}
