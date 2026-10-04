// Package review holds the contracts shared by the analysis runners and the
// gate: what a runner is asked to review and what it returns.
package review

import (
	"context"
	"time"
)

// Runner analyzes a pull request's docs impact.
type Runner interface {
	// Start returns a Result when the runner finishes synchronously, or Pending
	// when the Result arrives later through a webhook.
	Start(ctx context.Context, req Request) (Started, error)
}

// Started is Pending or Result.
type Started interface{ isStarted() }

// Pending means the analysis runs elsewhere; its Result must arrive before Deadline.
type Pending struct {
	Nonce    string
	Deadline time.Time
}

// Result is a finished analysis.
type Result struct {
	Runner  string
	Model   string
	Verdict Verdict
}

func (Pending) isStarted() {}
func (Result) isStarted()  {}

// Verdict is NoImpact or Proposals.
type Verdict interface{ isVerdict() }

// NoImpact means no doc needs to change; Reason is one line.
type NoImpact struct {
	Reason string
}

// Proposals are the doc changes the PR needs.
type Proposals []Proposal

func (NoImpact) isVerdict()  {}
func (Proposals) isVerdict() {}

// Proposal replaces one section of a doc under docs/, or creates a new doc
// when Section is empty.
type Proposal struct {
	DocPath    string `json:"doc_path" jsonschema:"Repo-relative path, under docs/, of the doc this proposal changes or creates."`
	Section    string `json:"section" jsonschema:"Heading of the section to replace, or empty to create a new doc."`
	Anchor     Anchor `json:"anchor" jsonschema:"Line in the PR diff that caused this proposal."`
	Reason     string `json:"reason" jsonschema:"One-line explanation of why this doc change is needed."`
	Content    string `json:"content" jsonschema:"Replacement content for the section, or the full content of a new doc."`
	IndexEntry string `json:"index_entry,omitempty" jsonschema:"Entry to add to the docs index; set iff section is empty."`
}

// Anchor is the line in the PR diff that caused a proposal.
type Anchor struct {
	File string `json:"file" jsonschema:"Path of the changed file the anchor points into."`
	Line int    `json:"line" jsonschema:"Head-side, 1-based line number within the changed file."`
}

// Request is what a runner reviews.
type Request struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	BaseSHA        string
	HeadSHA        string
	ChangedFiles   []ChangedFile
	CandidateDocs  []string
}

// ChangedFile is a file in the PR diff and the head-side line ranges its hunks cover.
type ChangedFile struct {
	Path  string
	Hunks []LineRange
	Patch string // unified diff text for Path, as GitHub returns it; filled by #8.
}

// LineRange is an inclusive range of 1-based line numbers.
type LineRange struct {
	Start int
	End   int
}
