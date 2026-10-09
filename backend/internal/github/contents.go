package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"

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
		if isNotFound(resp) || isTooLarge(err) {
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

// PathAtRef reports what is at path itself in owner/repo at ref: nothing, a
// directory, or anything else (a file of any size, symlink, submodule). It
// does not look at parent paths.
func (c *Client) PathAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (exists, dir bool, err error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return false, false, fmt.Errorf("stat %s of %s/%s at %s: %w", path, owner, repo, ref, err)
	}

	file, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	switch {
	case err == nil:
		// Only a directory lists entries, which leaves file nil.
		return true, file == nil, nil
	case isTooLarge(err):
		return true, false, nil
	case isNotFound(resp):
		return false, false, nil
	}
	return false, false, fmt.Errorf("stat %s of %s/%s at %s: %w", path, owner, repo, ref, err)
}

// isTooLarge reports whether the contents API refused a file over 100 MB, which
// it answers with 403 and the error code too_large.
func isTooLarge(err error) bool {
	var apiErr *github.ErrorResponse
	if !errors.As(err, &apiErr) || apiErr.Response == nil || apiErr.Response.StatusCode != http.StatusForbidden {
		return false
	}
	for _, e := range apiErr.Errors {
		if e.Code == "too_large" {
			return true
		}
	}
	return false
}
