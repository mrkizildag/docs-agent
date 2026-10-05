package github

import (
	"context"
	"errors"
	"fmt"
)

// MergeBase returns the merge base commit of base and head in owner/repo.
func (c *Client) MergeBase(ctx context.Context, installationID int64, owner, repo, base, head string) (string, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return "", fmt.Errorf("merge base %s/%s %s...%s: %w", owner, repo, base, head, err)
	}

	cmp, _, err := client.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return "", fmt.Errorf("merge base %s/%s %s...%s: %w", owner, repo, base, head, err)
	}
	sha := cmp.GetMergeBaseCommit().GetSHA()
	if sha == "" {
		return "", fmt.Errorf("merge base %s/%s %s...%s: %w", owner, repo, base, head, errors.New("response has no merge base commit"))
	}
	return sha, nil
}
