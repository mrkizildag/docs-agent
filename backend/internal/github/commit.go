package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

// CommitFiles commits files on top of parentSHA and moves branch to the new
// commit with a non-force ref update. It returns gate.ErrBranchMoved when
// GitHub rejects the update because branch is no longer at parentSHA.
func (c *Client) CommitFiles(ctx context.Context, installationID int64, owner, repo, branch, parentSHA string, files []gate.FileChange, message string) (string, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return "", fmt.Errorf("commit to %s/%s %s: %w", owner, repo, branch, err)
	}

	parent, _, err := client.Git.GetCommit(ctx, owner, repo, parentSHA)
	if err != nil {
		return "", fmt.Errorf("commit to %s/%s %s: get parent %s: %w", owner, repo, branch, parentSHA, err)
	}

	entries := make([]*github.TreeEntry, 0, len(files))
	for _, f := range files {
		blob, _, err := client.Git.CreateBlob(ctx, owner, repo, github.Blob{Content: new(f.Content), Encoding: new("utf-8")})
		if err != nil {
			return "", fmt.Errorf("commit to %s/%s %s: create blob for %s: %w", owner, repo, branch, f.Path, err)
		}
		entries = append(entries, &github.TreeEntry{Path: new(f.Path), Mode: new("100644"), Type: new("blob"), SHA: blob.SHA})
	}

	tree, _, err := client.Git.CreateTree(ctx, owner, repo, parent.GetTree().GetSHA(), entries)
	if err != nil {
		return "", fmt.Errorf("commit to %s/%s %s: create tree: %w", owner, repo, branch, err)
	}

	commit, _, err := client.Git.CreateCommit(ctx, owner, repo, github.Commit{
		Message: new(message),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: new(parentSHA)}},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("commit to %s/%s %s: create commit: %w", owner, repo, branch, err)
	}

	if _, _, err := client.Git.UpdateRef(ctx, owner, repo, "heads/"+branch, github.UpdateRef{SHA: commit.GetSHA(), Force: new(false)}); err != nil {
		var apiErr *github.ErrorResponse
		if errors.As(err, &apiErr) && apiErr.Response != nil && apiErr.Response.StatusCode == http.StatusUnprocessableEntity {
			return "", fmt.Errorf("commit to %s/%s %s: update ref: %w", owner, repo, branch, gate.ErrBranchMoved)
		}
		return "", fmt.Errorf("commit to %s/%s %s: update ref: %w", owner, repo, branch, err)
	}
	return commit.GetSHA(), nil
}

// BranchCommit returns the commit branch points at.
func (c *Client) BranchCommit(ctx context.Context, installationID int64, owner, repo, branch string) (gate.Commit, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit of %s/%s %s: %w", owner, repo, branch, err)
	}

	b, _, err := client.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit of %s/%s %s: %w", owner, repo, branch, err)
	}
	parents := make([]string, 0, len(b.GetCommit().Parents))
	for _, p := range b.GetCommit().Parents {
		parents = append(parents, p.GetSHA())
	}
	return gate.Commit{SHA: b.GetCommit().GetSHA(), Message: b.GetCommit().GetCommit().GetMessage(), Parents: parents}, nil
}

// Permission reports whether user has admin, maintain or write access to owner/repo.
func (c *Client) Permission(ctx context.Context, installationID int64, owner, repo, user string) (bool, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return false, fmt.Errorf("permission of %s on %s/%s: %w", user, owner, repo, err)
	}

	level, _, err := client.Repositories.GetPermissionLevel(ctx, owner, repo, user)
	if err != nil {
		return false, fmt.Errorf("permission of %s on %s/%s: %w", user, owner, repo, err)
	}
	switch level.GetPermission() {
	case "admin", "maintain", "write":
		return true, nil
	default:
		return false, nil
	}
}
