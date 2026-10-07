package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

const userPageSize = 100

// AccessibleRepos returns the repositories the user can read on every
// installation of the app the user can access. An installation GitHub answers
// 404 for is skipped.
func (c *UserClient) AccessibleRepos(ctx context.Context, accessToken string) ([]auth.Repo, error) {
	var installs struct {
		Installations []struct {
			ID int64 `json:"id"`
		} `json:"installations"`
	}
	var ids []int64
	for page := 1; ; page++ {
		installs.Installations = nil
		found, err := c.getPage(ctx, accessToken, fmt.Sprintf("/user/installations?per_page=%d&page=%d", userPageSize, page), &installs)
		if err != nil {
			return nil, fmt.Errorf("list installations: %w", err)
		}
		if !found {
			break
		}
		for _, in := range installs.Installations {
			ids = append(ids, in.ID)
		}
		if len(installs.Installations) < userPageSize {
			break
		}
	}

	var repos []auth.Repo
	for _, id := range ids {
		var list struct {
			Repositories []struct {
				Name  string `json:"name"`
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		for page := 1; ; page++ {
			list.Repositories = nil
			found, err := c.getPage(ctx, accessToken, fmt.Sprintf("/user/installations/%d/repositories?per_page=%d&page=%d", id, userPageSize, page), &list)
			if err != nil {
				return nil, fmt.Errorf("list repositories of installation %d: %w", id, err)
			}
			if !found {
				break
			}
			for _, r := range list.Repositories {
				repos = append(repos, auth.Repo{Owner: r.Owner.Login, Name: r.Name, InstallationID: id})
			}
			if len(list.Repositories) < userPageSize {
				break
			}
		}
	}
	return repos, nil
}

// getPage decodes one GET into out. found is false on 404.
func (c *UserClient) getPage(ctx context.Context, accessToken, path string, out any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	status, body, err := c.do(req)
	if err != nil {
		return false, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return false, nil
	case http.StatusUnauthorized:
		return false, fmt.Errorf("GitHub refused the user token: %w", auth.ErrUnauthenticated)
	default:
		return false, fmt.Errorf("GitHub answered HTTP %d", status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("decode response: %w", err)
	}
	return true, nil
}
