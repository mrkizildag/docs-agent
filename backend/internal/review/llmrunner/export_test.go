package llmrunner

import (
	"log/slog"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// SetRemote overrides the clone's remote URL. Tests use it to clone a local
// git repository instead of a real GitHub repo.
func (r *Runner) SetRemote(remote string) {
	r.remote = remote
}

// SetTimeout overrides the analysis and scaffold deadlines.
func (r *Runner) SetTimeout(d time.Duration) {
	r.analysisLimits.timeout = d
	r.scaffoldLimits.timeout = d
}

// SetTokenBudget overrides the analysis and scaffold token budgets.
func (r *Runner) SetTokenBudget(n int) {
	r.analysisLimits.tokens = n
	r.scaffoldLimits.tokens = n
}

// SetLogger overrides the logger.
func (r *Runner) SetLogger(l *slog.Logger) {
	r.log = l
}

// Failed exposes the runner's error classification.
func Failed(err error) *review.FailedError {
	return failed(err)
}
