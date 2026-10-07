package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	pathpkg "path"

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

// PathAtRef reports whether anything (file of any size, directory, symlink) is
// at path in owner/repo at ref.
func (c *Client) PathAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (exists bool, err error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return false, fmt.Errorf("stat %s of %s/%s at %s: %w", path, owner, repo, ref, err)
	}

	opts := &github.RepositoryContentGetOptions{Ref: ref}
	_, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
	switch {
	case err == nil, isTooLarge(err):
		return true, nil
	case !isNotFound(resp):
		return false, fmt.Errorf("stat %s of %s/%s at %s: %w", path, owner, repo, ref, err)
	}

	// A path under a file, symlink, or submodule reads as missing but can't be
	// created; the nearest existing parent decides. Only a directory lists
	// entries, which leaves file nil.
	for dir := pathpkg.Dir(path); dir != "." && dir != "/"; dir = pathpkg.Dir(dir) {
		file, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, dir, opts)
		switch {
		case err == nil:
			return file != nil, nil
		case !isNotFound(resp):
			return false, fmt.Errorf("stat %s of %s/%s at %s: %w", dir, owner, repo, ref, err)
		}
	}
	return false, nil
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
