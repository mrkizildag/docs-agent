package auth

import "context"

// AccessToken exposes accessToken to the external tests.
func (s *Service) AccessToken(ctx context.Context, v Viewer) (string, error) {
	return s.accessToken(ctx, v)
}

// SessionLocks is how many per-session refresh locks the Service holds.
func (s *Service) SessionLocks() int {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	return len(s.locks)
}
