package gate

import "time"

// WithRetryBackoff sets the wait before the first retry of a Collect or a
// comment post (doubling each retry) and returns s.
func (s *Service) WithRetryBackoff(d time.Duration) *Service {
	s.retryBackoff = d
	return s
}
