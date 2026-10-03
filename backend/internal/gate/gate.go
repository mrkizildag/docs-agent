// Package gate is the domain: deciding what check run a pull request gets.
package gate

import (
	"context"
	"fmt"
)

// PullRequest is the subset of a GitHub pull request the gate needs.
type PullRequest struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string
}

// Conclusion is a GitHub check run conclusion.
type Conclusion string

const ConclusionSuccess Conclusion = "success"

// CheckRun is a completed GitHub check run.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Conclusion Conclusion
	Title      string
	Summary    string
}

// GitHub creates check runs on behalf of an installation.
type GitHub interface {
	CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run CheckRun) error
}

const checkName = "docs-agent"

// PRState is what the gate remembers about one pull request between events.
type PRState struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string // head commit the gate last reported a check run for; "" if never
}

// Store persists PRState.
type Store interface {
	// LoadPR returns the zero-HeadSHA state (identity fields filled from the args) for a PR never saved.
	LoadPR(ctx context.Context, owner, repo string, number int) (PRState, error)
	SavePR(ctx context.Context, state PRState) error
}

// OnPush is the transition for a new head commit: pure, no I/O.
func OnPush(_ PRState, pr PullRequest) (PRState, CheckRun) {
	run := CheckRun{
		Name:       checkName,
		HeadSHA:    pr.HeadSHA,
		Conclusion: ConclusionSuccess,
		Title:      "docs-agent tracer",
		Summary:    "Analysis not implemented yet.",
	}

	return PRState(pr), run
}

// Service decides and reports the docs-agent check run for a pull request.
type Service struct {
	gh    GitHub
	store Store
}

// NewService returns a Service that reports check runs through gh and persists state through store.
func NewService(gh GitHub, store Store) *Service {
	return &Service{gh: gh, store: store}
}

// HandlePullRequest creates the tracer docs-agent check run for pr: it always
// succeeds until analysis (a later task) replaces it.
func (s *Service) HandlePullRequest(ctx context.Context, pr PullRequest) error {
	state, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: load state: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	next, run := OnPush(state, pr)

	if err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run); err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	if err := s.store.SavePR(ctx, next); err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: save state: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	return nil
}
