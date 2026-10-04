package llmrunner

import "time"

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
