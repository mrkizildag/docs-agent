package gate

import (
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// Outcome is how an analysis ended: exactly one of Result or Failed is set.
type Outcome struct {
	Result *review.Result
	Failed *AnalysisFailed
}

// resultOutcome is the outcome of r, or a failed one when r's verdict cannot
// conclude an analysis.
func resultOutcome(r review.Result) Outcome {
	if cause := unusableCause(r.Verdict); cause != "" {
		return failedOutcome(cause)
	}
	return Outcome{Result: &r}
}

// unusableCause is the fixed cause for a verdict that cannot conclude an
// analysis (an empty proposal list, where no impact must be NoImpact, or an
// unknown verdict), or "" for a usable one.
func unusableCause(v review.Verdict) string {
	switch v := v.(type) {
	case review.NoImpact:
		return ""
	case review.Proposals:
		if len(v) == 0 {
			return "The runner returned an empty proposal list."
		}
		return ""
	}
	return "The runner returned an unknown verdict."
}

// unusableResultError is a result whose verdict cannot conclude the analysis.
type unusableResultError struct{ cause string }

func (e *unusableResultError) Error() string { return "unusable result: " + e.cause }

func failedOutcome(cause string) Outcome { return Outcome{Failed: &AnalysisFailed{Cause: cause}} }

// AnalysisFailed is an analysis that ended without a usable Result. Title is
// the check run title; empty means "Analysis failed".
type AnalysisFailed struct {
	Title string
	Cause string
}

const titleTooLarge = "PR too large to analyze"

// tooLargeError is a pull request over the size limits; limit says which.
type tooLargeError struct{ limit string }

func (e *tooLargeError) Error() string { return titleTooLarge + ": " + e.limit }

const (
	maxChangedFiles = 50
	maxPatchBytes   = 1 << 20
)

// oversized reports whether changed is too large to analyze, and the limit hit.
// A file with changes but no patch text counts as over the patch limit: GitHub
// omits the patch of a diff too large to return. Binary files have no changes.
func oversized(changed []review.ChangedFile) (limit string, ok bool) {
	if len(changed) > maxChangedFiles {
		return fmt.Sprintf("%d changed files; the limit is %d.", len(changed), maxChangedFiles), true
	}
	total := 0
	for _, f := range changed {
		if f.Patch == "" && f.Changes > 0 {
			return fmt.Sprintf("GitHub omitted the diff of a changed file; the limit is %d bytes of patch text.", maxPatchBytes), true
		}
		total += len(f.Patch)
	}
	if total > maxPatchBytes {
		return fmt.Sprintf("%d bytes of patch text; the limit is %d bytes.", total, maxPatchBytes), true
	}
	return "", false
}

const (
	// maxSummaryBytes is GitHub's limit on a check run summary.
	maxSummaryBytes = 65535
	maxCauseBytes   = 1000
	truncatedMark   = "… (truncated)"
	// writeTimeout bounds the state writes that must survive a cancelled job.
	writeTimeout = 30 * time.Second
	// collectAttempts is how many times a result download is tried.
	collectAttempts = 3
	// postAttempts is how many times posting a result's comments is tried.
	postAttempts = 3
)

// AnalysisDeadline is how long an analysis may stay in progress before the
// deadline sweep concludes its check run. Every runner's own timeout must be
// shorter.
const AnalysisDeadline = 10 * time.Minute

// shortSHA is the 7-character abbreviation of sha.
func shortSHA(sha string) string { return sha[:min(7, len(sha))] }
