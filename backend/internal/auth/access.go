package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// AccessTTL is how long a session's repository list is reused before GitHub is asked again.
const AccessTTL = 5 * time.Minute

// ErrNoAccess means the user cannot read the repository, or pollux is not installed on it.
var ErrNoAccess = errors.New("no access to repository")

type accessEntry struct {
	repos     []Repo
	expiresAt time.Time
}

// accessCache holds each session's repositories, keyed by the session id hash.
type accessCache struct {
	mu      sync.Mutex
	entries map[string]accessEntry
}

// Repos returns the pollux-installed repositories the viewer can read, from a
// cache of AccessTTL, or ErrUnauthenticated when the viewer's tokens are no longer valid.
func (s *Service) Repos(ctx context.Context, v Viewer) ([]Repo, error) {
	key := string(v.idHash)
	now := s.accessNow()

	s.access.mu.Lock()
	entry, ok := s.access.entries[key]
	s.access.mu.Unlock()
	if ok && now.Before(entry.expiresAt) {
		return slices.Clone(entry.repos), nil
	}

	token, err := s.accessToken(ctx, v)
	if err != nil {
		return nil, fmt.Errorf("token for %s: %w", v.Login, err)
	}
	repos, err := s.github.AccessibleRepos(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("list repositories for %s: %w", v.Login, err)
	}

	s.access.mu.Lock()
	if s.access.entries == nil {
		s.access.entries = make(map[string]accessEntry)
	}
	s.access.entries[key] = accessEntry{repos: repos, expiresAt: now.Add(AccessTTL)}
	s.access.mu.Unlock()
	return slices.Clone(repos), nil
}

// RepoAccess returns the viewer's repository named owner/repo, matched
// case-insensitively, or ErrNoAccess.
func (s *Service) RepoAccess(ctx context.Context, v Viewer, owner, repo string) (Repo, error) {
	repos, err := s.Repos(ctx, v)
	if err != nil {
		return Repo{}, err
	}
	for _, r := range repos {
		if strings.EqualFold(r.Owner, owner) && strings.EqualFold(r.Name, repo) {
			return r, nil
		}
	}
	return Repo{}, fmt.Errorf("%s/%s for %s: %w", owner, repo, v.Login, ErrNoAccess)
}

// forgetAccess drops a session's cached repositories.
func (s *Service) forgetAccess(idHash []byte) {
	s.access.mu.Lock()
	delete(s.access.entries, string(idHash))
	s.access.mu.Unlock()
}

func (s *Service) accessNow() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}
