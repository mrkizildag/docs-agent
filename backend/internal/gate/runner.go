package gate

import (
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// Runners are the analysis runners a repo may use. A nil Runner means that
// runner is unavailable.
type Runners struct {
	Actions ActionsRunner
	Server  ServerRunner
}

// ActionsRunner is the runner that works in the repo's Actions workflow: it
// reviews PRs and writes scaffolds, both completing through a webhook.
type ActionsRunner interface {
	review.AsyncRunner
	review.AsyncScaffolder
}

// ServerRunner is the runner that works on the server: it reviews PRs and
// writes scaffolds in one call.
type ServerRunner interface {
	review.Runner
	review.Scaffolder
}

// runnerSelection names which runner HandlePullRequest uses.
type runnerSelection int

const (
	runnerNone runnerSelection = iota
	runnerActions
	runnerServer
)

// selectRunner picks the runner a repo uses: the Actions runner when the
// workflow is present, else the server runner, else none. A nil runner in
// the chosen slot counts as none; it never falls back to the other runner.
func selectRunner(hasWorkflow bool, runners Runners) runnerSelection {
	if hasWorkflow {
		if runners.Actions == nil {
			return runnerNone
		}
		return runnerActions
	}
	if runners.Server == nil {
		return runnerNone
	}
	return runnerServer
}
