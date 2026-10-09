package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// cache of AccessTTL, or ErrUnauthenticated when the viewer's tokens are no
// longer valid. GitHub rejecting the access token (the user revoked the app)
// deletes the session. Concurrent misses for one session fetch once.
func (s *Service) Repos(ctx context.Context, v Viewer) ([]Repo, error) {
	if repos, ok := s.cachedRepos(v.idHash); ok {
		return repos, nil
	}

	defer s.lockSession(v.idHash)()
	if repos, ok := s.cachedRepos(v.idHash); ok {
		return repos, nil
	}

	token, err := s.currentToken(ctx, v.idHash)
	if err != nil {
		return nil, fmt.Errorf("token for %s: %w", v.Login, err)
	}
	repos, err := s.github.AccessibleRepos(ctx, token)
	if errors.Is(err, ErrUnauthenticated) {
		return nil, fmt.Errorf("list repositories for %s: %w", v.Login, errors.Join(err, s.deleteSession(ctx, v.idHash)))
	}
	if err != nil {
		return nil, fmt.Errorf("list repositories for %s: %w", v.Login, err)
	}

	now := s.opts.Now()
	s.access.mu.Lock()
	for k, e := range s.access.entries {
		if !now.Before(e.expiresAt) {
			delete(s.access.entries, k)
		}
	}
	s.access.entries[string(v.idHash)] = accessEntry{repos: repos, expiresAt: now.Add(AccessTTL)}
	s.access.mu.Unlock()
	return slices.Clone(repos), nil
}

func (s *Service) cachedRepos(idHash []byte) ([]Repo, bool) {
	s.access.mu.Lock()
	defer s.access.mu.Unlock()
	entry, ok := s.access.entries[string(idHash)]
	if !ok || !s.opts.Now().Before(entry.expiresAt) {
		return nil, false
	}
	return slices.Clone(entry.repos), true
}

// RepoAccess returns the viewer's repository named owner/repo, matched
// ASCII case-insensitively, or ErrNoAccess. Handlers must use the returned
// Repo's Owner and Name, never the request's path values.
func (s *Service) RepoAccess(ctx context.Context, v Viewer, owner, repo string) (Repo, error) {
	repos, err := s.Repos(ctx, v)
	if err != nil {
		return Repo{}, err
	}
	for _, r := range repos {
		if equalASCIIFold(r.Owner, owner) && equalASCIIFold(r.Name, repo) {
			return r, nil
		}
	}
	return Repo{}, fmt.Errorf("%s/%s for %s: %w", owner, repo, v.Login, ErrNoAccess)
}

// equalASCIIFold compares bytewise, folding only A-Z, so Unicode look-alikes
// (the Kelvin sign for k) never match.
func equalASCIIFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

// forgetAccess drops a session's cached repositories.
func (s *Service) forgetAccess(idHash []byte) {
	s.access.mu.Lock()
	delete(s.access.entries, string(idHash))
	s.access.mu.Unlock()
}
