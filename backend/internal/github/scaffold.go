package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

var _ gate.ScaffoldGitHub = (*Client)(nil)

// DocsExist reports whether owner/repo has any entry named docs at ref: a
// directory, a file, or a submodule. Only a missing path is false.
func (c *Client) DocsExist(ctx context.Context, installationID int64, owner, repo, ref string) (bool, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return false, fmt.Errorf("look for docs in %s/%s at %s: %w", owner, repo, ref, err)
	}

	_, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, "docs", &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isNotFound(resp) {
			return false, nil
		}
		return false, fmt.Errorf("look for docs in %s/%s at %s: %w", owner, repo, ref, err)
	}
	return true, nil
}

// DefaultBranch returns the name and tip commit of owner/repo's default branch.
func (c *Client) DefaultBranch(ctx context.Context, installationID int64, owner, repo string) (string, string, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return "", "", fmt.Errorf("default branch of %s/%s: %w", owner, repo, err)
	}

	r, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", "", fmt.Errorf("default branch of %s/%s: get repository: %w", owner, repo, err)
	}
	name := r.GetDefaultBranch()
	b, _, err := client.Repositories.GetBranch(ctx, owner, repo, name, 0)
	if err != nil {
		return "", "", fmt.Errorf("default branch of %s/%s: get branch %s: %w", owner, repo, name, err)
	}
	return name, b.GetCommit().GetSHA(), nil
}

// CreateBranch creates branch at sha. It returns gate.ErrBranchExists when the
// branch already exists.
func (c *Client) CreateBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("create branch %s of %s/%s: %w", branch, owner, repo, err)
	}

	if _, _, err := client.Git.CreateRef(ctx, owner, repo, github.CreateRef{Ref: "refs/heads/" + branch, SHA: sha}); err != nil {
		var apiErr *github.ErrorResponse
		if errors.As(err, &apiErr) && apiErr.Response != nil && apiErr.Response.StatusCode == http.StatusUnprocessableEntity &&
			strings.Contains(apiErr.Message, "Reference already exists") {
			return fmt.Errorf("create branch %s of %s/%s: %w", branch, owner, repo, gate.ErrBranchExists)
		}
		return fmt.Errorf("create branch %s of %s/%s: %w", branch, owner, repo, err)
	}
	return nil
}

// ResetBranch force-moves an existing branch to sha.
func (c *Client) ResetBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("reset branch %s of %s/%s: %w", branch, owner, repo, err)
	}

	force := true
	if _, _, err := client.Git.UpdateRef(ctx, owner, repo, "refs/heads/"+branch, github.UpdateRef{SHA: sha, Force: &force}); err != nil {
		return fmt.Errorf("reset branch %s of %s/%s: %w", branch, owner, repo, err)
	}
	return nil
}

// BranchSHA returns the commit branch points at.
func (c *Client) BranchSHA(ctx context.Context, installationID int64, owner, repo, branch string) (string, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return "", fmt.Errorf("tip of %s/%s %s: %w", owner, repo, branch, err)
	}

	b, _, err := client.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return "", fmt.Errorf("tip of %s/%s %s: %w", owner, repo, branch, err)
	}
	return b.GetCommit().GetSHA(), nil
}

// CreatePullRequest opens a pull request from pr.Head into pr.Base.
func (c *Client) CreatePullRequest(ctx context.Context, installationID int64, owner, repo string, pr gate.NewPullRequest) (gate.ScaffoldPR, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.ScaffoldPR{}, fmt.Errorf("create pull request %s -> %s in %s/%s: %w", pr.Head, pr.Base, owner, repo, err)
	}

	created, _, err := client.PullRequests.Create(ctx, owner, repo, github.CreatePullRequest{Title: &pr.Title, Body: &pr.Body, Head: pr.Head, Base: pr.Base})
	if err != nil {
		return gate.ScaffoldPR{}, fmt.Errorf("create pull request %s -> %s in %s/%s: %w", pr.Head, pr.Base, owner, repo, err)
	}
	return gate.ScaffoldPR{Number: created.GetNumber(), URL: created.GetHTMLURL()}, nil
}

// FindPullRequest returns the pull request, open or not, opened from branch of
// owner/repo itself.
func (c *Client) FindPullRequest(ctx context.Context, installationID int64, owner, repo, branch string) (gate.ScaffoldPR, bool, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.ScaffoldPR{}, false, fmt.Errorf("find pull request from %s in %s/%s: %w", branch, owner, repo, err)
	}

	list, _, err := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{State: "all", Head: owner + ":" + branch, ListOptions: github.ListOptions{PerPage: 1}})
	if err != nil {
		return gate.ScaffoldPR{}, false, fmt.Errorf("find pull request from %s in %s/%s: %w", branch, owner, repo, err)
	}
	if len(list) == 0 {
		return gate.ScaffoldPR{}, false, nil
	}
	return gate.ScaffoldPR{Number: list[0].GetNumber(), URL: list[0].GetHTMLURL()}, true, nil
}
