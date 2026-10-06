package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

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

	name, err := defaultBranchName(ctx, client, owner, repo)
	if err != nil {
		return "", "", fmt.Errorf("default branch of %s/%s: %w", owner, repo, err)
	}
	b, _, err := client.Repositories.GetBranch(ctx, owner, repo, name, 0)
	if err != nil {
		return "", "", fmt.Errorf("default branch of %s/%s: get branch %s: %w", owner, repo, name, err)
	}
	return name, b.GetCommit().GetSHA(), nil
}

func defaultBranchName(ctx context.Context, client *github.Client, owner, repo string) (string, error) {
	r, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("get repository: %w", err)
	}
	return r.GetDefaultBranch(), nil
}

func branchRef(branch string) string {
	return "refs/heads/" + branch
}

// CreateBranch creates branch at sha. It returns gate.ErrBranchExists when the
// branch already exists.
func (c *Client) CreateBranch(ctx context.Context, installationID int64, owner, repo, branch, sha string) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("create branch %s of %s/%s: %w", branch, owner, repo, err)
	}

	if _, _, err := client.Git.CreateRef(ctx, owner, repo, github.CreateRef{Ref: branchRef(branch), SHA: sha}); err != nil {
		var apiErr *github.ErrorResponse
		if hasStatus(err, http.StatusUnprocessableEntity) && errors.As(err, &apiErr) &&
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

	if _, _, err := client.Git.UpdateRef(ctx, owner, repo, branchRef(branch), github.UpdateRef{SHA: sha, Force: new(true)}); err != nil {
		return fmt.Errorf("reset branch %s of %s/%s: %w", branch, owner, repo, err)
	}
	return nil
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

// FindPullRequest returns the pull request opened from branch of owner/repo
// itself, preferring this App's bot's open one, then the bot's, then any open
// one, over the rest. ByBot is set when its author is this App's bot user.
func (c *Client) FindPullRequest(ctx context.Context, installationID int64, owner, repo, branch string) (gate.ScaffoldPR, bool, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.ScaffoldPR{}, false, fmt.Errorf("find pull request from %s in %s/%s: %w", branch, owner, repo, err)
	}

	bot, err := c.appBotLogin(ctx)
	if err != nil {
		return gate.ScaffoldPR{}, false, fmt.Errorf("find pull request from %s in %s/%s: %w", branch, owner, repo, err)
	}

	var list []*github.PullRequest
	opts := &github.PullRequestListOptions{State: "all", Head: owner + ":" + branch, ListOptions: github.ListOptions{PerPage: 100}}
	for pr, err := range client.PullRequests.ListIter(ctx, owner, repo, opts) {
		if err != nil {
			return gate.ScaffoldPR{}, false, fmt.Errorf("find pull request from %s in %s/%s: %w", branch, owner, repo, err)
		}
		list = append(list, pr)
	}
	if len(list) == 0 {
		return gate.ScaffoldPR{}, false, nil
	}
	isBot := func(pr *github.PullRequest) bool { return pr.GetUser().GetLogin() == bot }
	isOpen := func(pr *github.PullRequest) bool { return pr.GetState() == "open" }
	found := list[0]
	for _, match := range []func(*github.PullRequest) bool{
		func(pr *github.PullRequest) bool { return isBot(pr) && isOpen(pr) }, isBot, isOpen,
	} {
		if i := slices.IndexFunc(list, match); i >= 0 {
			found = list[i]
			break
		}
	}
	return gate.ScaffoldPR{Number: found.GetNumber(), URL: found.GetHTMLURL(), ByBot: isBot(found), Open: isOpen(found)}, true, nil
}
