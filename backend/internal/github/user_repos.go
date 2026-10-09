package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

const userPageSize = 100

// AccessibleRepos returns the repositories the user can read on every
// installation of the app the user can access. An installation that answers
// 403 or 404 (uninstalled, or an SSO org the token is not authorized for) is
// skipped.
func (c *UserClient) AccessibleRepos(ctx context.Context, accessToken string) ([]auth.Repo, error) {
	client, err := c.apiClient(accessToken)
	if err != nil {
		return nil, err
	}
	opts := &github.ListOptions{PerPage: userPageSize}

	var ids []int64
	for in, err := range client.Apps.ListUserInstallationsIter(ctx, opts) {
		if err != nil {
			return nil, userAPIError("list installations", err)
		}
		ids = append(ids, in.GetID())
	}

	var repos []auth.Repo
	for _, id := range ids {
		installRepos, err := listInstallationRepos(ctx, client, id, opts)
		if err != nil {
			return nil, err
		}
		repos = append(repos, installRepos...)
	}
	return repos, nil
}

func listInstallationRepos(ctx context.Context, client *github.Client, id int64, opts *github.ListOptions) ([]auth.Repo, error) {
	var repos []auth.Repo
	for r, err := range client.Apps.ListUserReposIter(ctx, id, opts) {
		if err != nil {
			switch apiErrorStatus(err) {
			case http.StatusForbidden, http.StatusNotFound:
				return nil, nil
			}
			return nil, userAPIError(fmt.Sprintf("list repositories of installation %d", id), err)
		}
		repos = append(repos, auth.Repo{Owner: r.GetOwner().GetLogin(), Name: r.GetName(), InstallationID: id})
	}
	return repos, nil
}
