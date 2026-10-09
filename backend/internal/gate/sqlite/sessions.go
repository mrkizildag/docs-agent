package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

// unixOrZero stores a zero time as 0, meaning "never expires".
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

// CreateLogin stores a pending login attempt under its state hash.
func (s *Store) CreateLogin(ctx context.Context, stateHash []byte, login auth.Login) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO login_attempts (state_hash, verifier, binding_hash, expires_at) VALUES (?, ?, ?, ?)`,
		stateHash, login.Verifier, login.BindingHash, login.ExpiresAt.Unix())
	if err != nil {
		return fmt.Errorf("insert login attempt: %w", err)
	}
	return nil
}

// TakeLogin deletes the login attempt for stateHash and returns it if it had not expired.
func (s *Store) TakeLogin(ctx context.Context, stateHash []byte, now time.Time) (auth.Login, bool, error) {
	var login auth.Login
	var expiresAt int64
	err := s.db.QueryRowContext(ctx,
		`DELETE FROM login_attempts WHERE state_hash = ? RETURNING verifier, binding_hash, expires_at`,
		stateHash).Scan(&login.Verifier, &login.BindingHash, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Login{}, false, nil
	}
	if err != nil {
		return auth.Login{}, false, fmt.Errorf("take login attempt: %w", err)
	}
	login.ExpiresAt = time.Unix(expiresAt, 0)
	if !now.Before(login.ExpiresAt) {
		return auth.Login{}, false, nil
	}
	return login, true, nil
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, session auth.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id_hash, login, avatar_url, sealed_tokens, access_expires_at, refresh_expires_at, last_used_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		session.IDHash, session.Profile.Login, session.Profile.AvatarURL, session.SealedTokens,
		unixOrZero(session.AccessExpiresAt), unixOrZero(session.RefreshExpiresAt), session.LastUsedAt.Unix())
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// Session returns the session for idHash.
func (s *Store) Session(ctx context.Context, idHash []byte) (auth.Session, bool, error) {
	session := auth.Session{IDHash: idHash}
	var accessExpiresAt, refreshExpiresAt, lastUsedAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT login, avatar_url, sealed_tokens, access_expires_at, refresh_expires_at, last_used_at, version FROM sessions WHERE id_hash = ?`,
		idHash).Scan(&session.Profile.Login, &session.Profile.AvatarURL, &session.SealedTokens, &accessExpiresAt, &refreshExpiresAt, &lastUsedAt, &session.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.Session{}, false, nil
	}
	if err != nil {
		return auth.Session{}, false, fmt.Errorf("select session: %w", err)
	}
	session.AccessExpiresAt = timeOrZero(accessExpiresAt)
	session.RefreshExpiresAt = timeOrZero(refreshExpiresAt)
	session.LastUsedAt = time.Unix(lastUsedAt, 0)
	return session, true, nil
}

// DeleteSession removes the session for idHash; an unknown hash is not an error.
func (s *Store) DeleteSession(ctx context.Context, idHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// TouchSession records that the session was used at the given time.
func (s *Store) TouchSession(ctx context.Context, idHash []byte, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_used_at = ? WHERE id_hash = ?`, at.Unix(), idHash); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// SwapTokens replaces the session's tokens if its version is still prevVersion.
func (s *Store) SwapTokens(ctx context.Context, idHash []byte, prevVersion int64, sealed []byte, accessExp, refreshExp time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET sealed_tokens = ?, access_expires_at = ?, refresh_expires_at = ?, version = version + 1
		 WHERE id_hash = ? AND version = ?`,
		sealed, unixOrZero(accessExp), unixOrZero(refreshExp), idHash, prevVersion)
	if err != nil {
		return false, fmt.Errorf("swap session tokens: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count swapped sessions: %w", err)
	}
	return n == 1, nil
}

// DeleteExpired removes expired login attempts and sessions idle since before
// idleBefore or past their refresh token's expiry, and returns how many rows it removed.
func (s *Store) DeleteExpired(ctx context.Context, idleBefore, now time.Time) (int64, error) {
	logins, err := s.db.ExecContext(ctx, `DELETE FROM login_attempts WHERE expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("delete expired login attempts: %w", err)
	}
	sessions, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE last_used_at < ? OR (refresh_expires_at != 0 AND refresh_expires_at <= ?)`,
		idleBefore.Unix(), now.Unix())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	var total int64
	for _, res := range []sql.Result{logins, sessions} {
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("count deleted rows: %w", err)
		}
		total += n
	}
	return total, nil
}
