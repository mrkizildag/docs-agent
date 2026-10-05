package github

import (
	"context"
	"fmt"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
)

// FileAtRef returns the content of the file at path in owner/repo at ref. ok
// is false when the path is not a file at ref or the file exceeds docs.MaxDocBytes.
func (c *Client) FileAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (content []byte, ok bool, err error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, false, fmt.Errorf("read %s of %s/%s at %s: %w", path, owner, repo, ref, err)
	}

	file, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isNotFound(resp) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s of %s/%s at %s: %w", path, owner, repo, ref, err)
	}
	if file == nil || file.GetSize() > docs.MaxDocBytes {
		return nil, false, nil
	}

	text, err := file.GetContent()
	if err != nil {
		return nil, false, fmt.Errorf("read %s of %s/%s at %s: decode content: %w", path, owner, repo, ref, err)
	}
	return []byte(text), true, nil
}
