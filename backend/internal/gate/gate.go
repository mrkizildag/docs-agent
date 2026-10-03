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

// Service decides and reports the docs-agent check run for a pull request.
type Service struct {
	gh GitHub
}

// NewService returns a Service that reports check runs through gh.
func NewService(gh GitHub) *Service {
	return &Service{gh: gh}
}

// HandlePullRequest creates the tracer docs-agent check run for pr: it always
// succeeds until analysis (a later task) replaces it.
func (s *Service) HandlePullRequest(ctx context.Context, pr PullRequest) error {
	run := CheckRun{
		Name:       checkName,
		HeadSHA:    pr.HeadSHA,
		Conclusion: ConclusionSuccess,
		Title:      "docs-agent tracer",
		Summary:    "Analysis not implemented yet.",
	}

	if err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run); err != nil {
		return fmt.Errorf("handle pull request %s/%s#%d: %w", pr.Owner, pr.Repo, pr.Number, err)
	}

	return nil
}
