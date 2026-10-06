package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

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

	trees := treeReader{client: client, owner: owner, repo: repo, rootSHA: parent.GetTree().GetSHA(), dirs: map[string]map[string]*github.TreeEntry{}}

	entries := make([]*github.TreeEntry, 0, len(files))
	for _, f := range files {
		mode := "100644"
		existing, err := trees.entry(ctx, f.Path)
		if err != nil {
			return "", fmt.Errorf("commit to %s/%s %s: look up %s: %w", owner, repo, branch, f.Path, err)
		}
		if existing != nil {
			switch existing.GetMode() {
			case "120000", "160000":
				return "", fmt.Errorf("commit to %s/%s %s: mode %s cannot be overwritten: %w", owner, repo, branch, existing.GetMode(), &gate.CommitRejectedError{Reason: f.Path + " is a symlink or submodule"})
			}
			mode = existing.GetMode()
		}
		blob, _, err := client.Git.CreateBlob(ctx, owner, repo, github.Blob{Content: new(f.Content), Encoding: new("utf-8")})
		if err != nil {
			return "", fmt.Errorf("commit to %s/%s %s: create blob for %s: %w", owner, repo, branch, f.Path, err)
		}
		entries = append(entries, &github.TreeEntry{Path: new(f.Path), Mode: new(mode), Type: new("blob"), SHA: blob.SHA})
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

	if _, _, err := client.Git.UpdateRef(ctx, owner, repo, branchRef(branch), github.UpdateRef{SHA: commit.GetSHA(), Force: new(false)}); err != nil {
		if hasStatus(err, http.StatusUnprocessableEntity) {
			tip, _, tipErr := client.Repositories.GetBranch(ctx, owner, repo, branch, 0)
			if tipErr != nil {
				return "", fmt.Errorf("commit to %s/%s %s: update ref: %w; read branch tip: %w", owner, repo, branch, err, tipErr)
			}
			if tip.GetCommit().GetSHA() == parentSHA {
				return "", fmt.Errorf("commit to %s/%s %s: update ref: %w: %w", owner, repo, branch, &gate.CommitRejectedError{Reason: "GitHub refused to update the branch (it may be protected)"}, err)
			}
			return "", fmt.Errorf("commit to %s/%s %s: update ref: %w", owner, repo, branch, gate.ErrBranchMoved)
		}
		return "", fmt.Errorf("commit to %s/%s %s: update ref: %w", owner, repo, branch, err)
	}
	return commit.GetSHA(), nil
}

// treeReader looks up single paths in a commit's tree one directory at a time,
// so a large repository is never read whole.
type treeReader struct {
	client  *github.Client
	owner   string
	repo    string
	rootSHA string
	dirs    map[string]map[string]*github.TreeEntry // nil value: directory does not exist
}

// entry returns the tree entry at path, or nil when the path does not exist.
func (r *treeReader) entry(ctx context.Context, path string) (*github.TreeEntry, error) {
	dir, name := ".", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}
	entries, err := r.dir(ctx, dir)
	if err != nil {
		return nil, err
	}
	return entries[name], nil
}

func (r *treeReader) dir(ctx context.Context, dir string) (map[string]*github.TreeEntry, error) {
	if entries, ok := r.dirs[dir]; ok {
		return entries, nil
	}
	sha := r.rootSHA
	if dir != "." {
		parent, err := r.entry(ctx, dir)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			r.dirs[dir] = nil
			return nil, nil
		}
		if parent.GetType() != "tree" {
			return nil, fmt.Errorf("look up %s: %w", dir, &gate.CommitRejectedError{Reason: dir + " is a file, not a directory"})
		}
		sha = parent.GetSHA()
	}
	tree, _, err := r.client.Git.GetTree(ctx, r.owner, r.repo, sha, false)
	if err != nil {
		return nil, fmt.Errorf("get tree of %s: %w", dir, err)
	}
	entries := make(map[string]*github.TreeEntry, len(tree.Entries))
	for _, e := range tree.Entries {
		entries[e.GetPath()] = e
	}
	r.dirs[dir] = entries
	return entries, nil
}

// BranchCommit returns the commit branch points at.
func (c *Client) BranchCommit(ctx context.Context, installationID int64, owner, repo, branch string) (gate.Commit, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit of %s/%s %s: %w", owner, repo, branch, err)
	}

	bot, err := c.appBotLogin(ctx)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit of %s/%s %s: %w", owner, repo, branch, err)
	}

	b, _, err := client.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit of %s/%s %s: %w", owner, repo, branch, err)
	}
	return toCommit(b.GetCommit(), bot), nil
}

// CommitAt returns the commit with the given SHA.
func (c *Client) CommitAt(ctx context.Context, installationID int64, owner, repo, sha string) (gate.Commit, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit %s of %s/%s: %w", sha, owner, repo, err)
	}

	bot, err := c.appBotLogin(ctx)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit %s of %s/%s: %w", sha, owner, repo, err)
	}

	rc, _, err := client.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return gate.Commit{}, fmt.Errorf("commit %s of %s/%s: %w", sha, owner, repo, err)
	}
	return toCommit(rc, bot), nil
}

func toCommit(rc *github.RepositoryCommit, bot string) gate.Commit {
	parents := make([]string, 0, len(rc.Parents))
	for _, p := range rc.Parents {
		parents = append(parents, p.GetSHA())
	}
	return gate.Commit{
		SHA:     rc.GetSHA(),
		Message: rc.GetCommit().GetMessage(),
		Parents: parents,
		Mine:    rc.GetAuthor().GetLogin() == bot,
	}
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
