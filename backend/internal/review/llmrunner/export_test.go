package llmrunner

import (
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
)

// SetRemote overrides the clone's remote URL. Tests use it to clone a local
// git repository instead of a real GitHub repo.
func (r *Runner) SetRemote(remote string) {
	r.remote = remote
}

// SetTimeout overrides the analysis deadline.
func (r *Runner) SetTimeout(d time.Duration) {
	r.timeout = d
}

// SetTokenBudget overrides the analysis token budget.
func (r *Runner) SetTokenBudget(n int) {
	r.budget = n
}

// CombinedPatch exposes combinedPatch for tests of the prompt's diff block.
func CombinedPatch(changed []review.ChangedFile) string {
	return combinedPatch(changed)
}

// HunkRanges exposes hunkRanges for tests of the prompt's anchor listing.
func HunkRanges(files []input.File) string {
	return hunkRanges(files)
}
