package github

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// hunkHeader matches `@@ -a,b +c,d @@`; the head side is c and d, and an
// omitted d means 1.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// GetPullRequest returns owner/repo#number's current base and head commits.
func (c *Client) GetPullRequest(ctx context.Context, installationID int64, owner, repo string, number int) (gate.PullRequest, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.PullRequest{}, fmt.Errorf("get pull request %s/%s#%d: %w", owner, repo, number, err)
	}

	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return gate.PullRequest{}, fmt.Errorf("get pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	return gate.PullRequest{
		InstallationID: installationID,
		Owner:          owner,
		Repo:           repo,
		Number:         number,
		BaseSHA:        pr.GetBase().GetSHA(),
		HeadSHA:        pr.GetHead().GetSHA(),
		HeadRef:        pr.GetHead().GetRef(),
		Fork:           pr.GetHead().GetRepo().GetFullName() != pr.GetBase().GetRepo().GetFullName(),
		Open:           pr.GetState() == "open",
	}, nil
}

// ListChangedFiles returns the files in owner/repo#number's diff. GitHub
// caps the listing at 3000 files, and omits the patch for large or binary files.
func (c *Client) ListChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("list changed files %s/%s#%d: %w", owner, repo, number, err)
	}

	var files []review.ChangedFile
	for f, err := range client.PullRequests.ListFilesIter(ctx, owner, repo, number, &github.ListOptions{PerPage: 100}) {
		if err != nil {
			return nil, fmt.Errorf("list changed files %s/%s#%d: %w", owner, repo, number, err)
		}
		hunks, err := parseHunks(f.GetPatch())
		if err != nil {
			return nil, fmt.Errorf("list changed files %s/%s#%d: parse patch of %s: %w", owner, repo, number, f.GetFilename(), err)
		}
		previous := ""
		if f.GetStatus() == "renamed" {
			previous = f.GetPreviousFilename()
		}
		files = append(files, review.ChangedFile{Path: f.GetFilename(), PreviousPath: previous, Removed: f.GetStatus() == "removed", Hunks: hunks, Patch: f.GetPatch(), Changes: f.GetChanges()})
	}
	return files, nil
}

// parseHunks returns the head-side line range of each hunk header in patch.
func parseHunks(patch string) ([]review.LineRange, error) {
	var hunks []review.LineRange
	for line := range strings.SplitSeq(patch, "\n") {
		if !strings.HasPrefix(line, "@@") {
			continue
		}
		m := hunkHeader.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("malformed hunk header %q", line)
		}
		start, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("hunk header %q: %w", line, err)
		}
		count := 1
		if m[2] != "" {
			count, err = strconv.Atoi(m[2])
			if err != nil {
				return nil, fmt.Errorf("hunk header %q: %w", line, err)
			}
		}
		if count == 0 {
			continue
		}
		hunks = append(hunks, review.LineRange{Start: start, End: start + count - 1})
	}
	return hunks, nil
}
