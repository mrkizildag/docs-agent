// Package auth is the dashboard's sign-in domain: login attempts with PKCE,
// server-side sessions, and sealed GitHub user tokens. It imports only the
// standard library; storage and GitHub are reached through its interfaces.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"
)

const (
	// LoginTTL is how long a login attempt stays valid after BeginLogin.
	LoginTTL = 10 * time.Minute
	// SessionIdle is how long a session may go unused before it stops authenticating.
	SessionIdle = 7 * 24 * time.Hour

	// touchInterval bounds how often a session's last use is written.
	touchInterval = time.Minute
	// refreshMargin is how close to expiry an access token is refreshed.
	refreshMargin = time.Minute

	// loginSweepInterval bounds how often BeginLogin deletes expired rows.
	loginSweepInterval = time.Minute
	// rotateTimeout bounds one detached refresh and token swap.
	rotateTimeout = 15 * time.Second
	// revokeTimeout bounds the GitHub revoke at logout.
	revokeTimeout = 10 * time.Second
	// logoutTimeout bounds the detached session load and delete at logout.
	logoutTimeout = 10 * time.Second
	// persistAttempts is how many times a refreshed token pair is sealed and stored.
	persistAttempts = 3

	randomBytes = 32
)

var (
	// ErrUnauthenticated means the session id is missing, unknown, or expired.
	ErrUnauthenticated = errors.New("not signed in")
	// ErrInvalidLogin means the callback's state, binding, or code was refused.
	ErrInvalidLogin = errors.New("invalid login")
	// ErrRefreshRefused means GitHub rejected the refresh token itself
	// (bad_refresh_token); the session can never refresh again.
	ErrRefreshRefused = errors.New("refresh token refused")
	// ErrRevokeFailed means Logout deleted the session but GitHub did not
	// revoke the token. The user is signed out; callers log it and answer success.
	ErrRevokeFailed = errors.New("revoke GitHub token failed")
)

// Tokens are a GitHub user's tokens. Zero expiry times mean the token does not expire.
type Tokens struct {
	Access           string
	Refresh          string
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

func (Tokens) String() string { return "auth.Tokens{<redacted>}" }

func (Tokens) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Profile is the GitHub user a session belongs to.
type Profile struct {
	Login     string
	AvatarURL string
}

// Login is a pending sign-in attempt, stored by the hash of its state.
type Login struct {
	Verifier    string
	BindingHash []byte
	ExpiresAt   time.Time
}

// Session is a signed-in browser, stored by the hash of its cookie id.
type Session struct {
	IDHash           []byte
	Profile          Profile
	SealedTokens     []byte
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	LastUsedAt       time.Time
	// Version counts token swaps; SwapTokens compares against it.
	Version int64
}

// Store persists login attempts and sessions.
type Store interface {
	CreateLogin(ctx context.Context, stateHash []byte, login Login) error
	// TakeLogin returns and deletes the unexpired login for stateHash, so it works once.
	TakeLogin(ctx context.Context, stateHash []byte, now time.Time) (Login, bool, error)
	CreateSession(ctx context.Context, session Session) error
	Session(ctx context.Context, idHash []byte) (Session, bool, error)
	// TouchSession sets the session's last use to at.
	TouchSession(ctx context.Context, idHash []byte, at time.Time) error
	// SwapTokens replaces the sealed tokens and their expiries and bumps the
	// version, only if the session is still at prevVersion. It reports whether it did.
	SwapTokens(ctx context.Context, idHash []byte, prevVersion int64, sealed []byte, accessExp, refreshExp time.Time) (bool, error)
	DeleteSession(ctx context.Context, idHash []byte) error
	// DeleteExpired removes expired login attempts and sessions that are idle
	// since before idleBefore or whose refresh token expired, and returns how many.
	DeleteExpired(ctx context.Context, idleBefore, now time.Time) (int64, error)
}

// Repo is a repository pollux is installed on that a user can read.
type Repo struct {
	Owner          string
	Name           string
	InstallationID int64
}

// GitHubUser is GitHub's OAuth and user API. Exchange wraps ErrInvalidLogin
// when GitHub refuses the code; Refresh wraps ErrRefreshRefused only when
// GitHub rejects the refresh token itself, and returns a plain error for
// transient failures; AccessibleRepos wraps ErrUnauthenticated when GitHub
// rejects the access token.
type GitHubUser interface {
	Exchange(ctx context.Context, code, verifier, redirectURL string) (Tokens, error)
	Refresh(ctx context.Context, refreshToken string) (Tokens, error)
	Revoke(ctx context.Context, accessToken string) error
	User(ctx context.Context, accessToken string) (Profile, error)
	AccessibleRepos(ctx context.Context, accessToken string) ([]Repo, error)
}

// Options configures a Service.
type Options struct {
	ClientID     string
	AuthorizeURL string
	RedirectURL  string
	// Key is the 32-byte AES-256 key tokens are sealed with.
	Key [32]byte
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logger receives persistence failures the caller cannot see; nil discards them.
	Logger *slog.Logger
}

func (o Options) String() string {
	return fmt.Sprintf("auth.Options{ClientID: %q, AuthorizeURL: %q, RedirectURL: %q, Key: <redacted>}", o.ClientID, o.AuthorizeURL, o.RedirectURL)
}

func (o Options) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("client_id", o.ClientID),
		slog.String("authorize_url", o.AuthorizeURL),
		slog.String("redirect_url", o.RedirectURL),
		slog.String("key", "[redacted]"),
	)
}

// Service signs users in with GitHub and tracks their sessions.
type Service struct {
	store  Store
	github GitHubUser
	opts   Options
	access accessCache

	// locksMu guards locks, one mutex per session id hash, so a session's
	// rotating refresh token is used by one request at a time.
	locksMu sync.Mutex
	locks   map[string]*sessionLock

	// sweepMu guards lastSweep, when BeginLogin last deleted expired rows.
	sweepMu   sync.Mutex
	lastSweep time.Time
}

func NewService(store Store, github GitHubUser, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		store: store, github: github, opts: opts,
		access: accessCache{entries: map[string]accessEntry{}},
		locks:  map[string]*sessionLock{},
	}
}

// Viewer is the authenticated user of a request.
type Viewer struct {
	Profile
	idHash []byte
	tokens Tokens
}

func (v Viewer) String() string { return fmt.Sprintf("auth.Viewer{Login: %q}", v.Login) }

func (v Viewer) LogValue() slog.Value { return slog.GroupValue(slog.String("login", v.Login)) }

// BeginLogin records a login attempt and returns GitHub's authorize URL and the
// binding value the caller must set as a cookie and present at CompleteLogin.
// It also deletes expired rows, at most once a minute, so repeated calls cannot
// grow the login table without bound.
func (s *Service) BeginLogin(ctx context.Context) (authorizeURL, binding string, err error) {
	s.sweepExpired(ctx)
	state, verifier, binding := randomToken(), randomToken(), randomToken()
	err = s.store.CreateLogin(ctx, hash(state), Login{
		Verifier:    verifier,
		BindingHash: hash(binding),
		ExpiresAt:   s.opts.Now().Add(LoginTTL),
	})
	if err != nil {
		return "", "", fmt.Errorf("create login: %w", err)
	}

	challenge := sha256.Sum256([]byte(verifier))
	u, err := url.Parse(s.opts.AuthorizeURL)
	if err != nil {
		return "", "", fmt.Errorf("parse authorize URL: %w", err)
	}
	u.RawQuery = url.Values{
		"client_id":             {s.opts.ClientID},
		"redirect_uri":          {s.opts.RedirectURL},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}.Encode()
	return u.String(), binding, nil
}

// sweepExpired deletes expired logins and sessions at most once per
// loginSweepInterval; a failure must not block the login.
func (s *Service) sweepExpired(ctx context.Context) {
	now := s.opts.Now()
	s.sweepMu.Lock()
	due := now.Sub(s.lastSweep) >= loginSweepInterval
	s.sweepMu.Unlock()
	if !due {
		return
	}
	if _, err := s.store.DeleteExpired(ctx, now.Add(-SessionIdle), now); err != nil {
		s.opts.Logger.Error("delete expired logins and sessions", "err", err)
		return
	}
	s.sweepMu.Lock()
	s.lastSweep = now
	s.sweepMu.Unlock()
}

// CompleteLogin consumes the login attempt for state, exchanges code, and
// returns the new session id for the cookie. The attempt is spent even when
// binding does not match.
func (s *Service) CompleteLogin(ctx context.Context, state, binding, code string) (string, error) {
	login, ok, err := s.store.TakeLogin(ctx, hash(state), s.opts.Now())
	if err != nil {
		return "", fmt.Errorf("take login: %w", err)
	}
	if !ok || subtle.ConstantTimeCompare(login.BindingHash, hash(binding)) != 1 {
		return "", fmt.Errorf("%w: unknown, expired, or foreign state", ErrInvalidLogin)
	}

	tokens, err := s.github.Exchange(ctx, code, login.Verifier, s.opts.RedirectURL)
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	profile, err := s.github.User(ctx, tokens.Access)
	if err != nil {
		return "", fmt.Errorf("fetch GitHub user: %w", err)
	}

	now := s.opts.Now()
	id := randomToken()
	idHash := hash(id)
	sealed, err := seal(s.opts.Key, idHash, tokens)
	if err != nil {
		return "", err
	}
	err = s.store.CreateSession(ctx, Session{
		IDHash:           idHash,
		Profile:          profile,
		SealedTokens:     sealed,
		AccessExpiresAt:  tokens.AccessExpiresAt,
		RefreshExpiresAt: tokens.RefreshExpiresAt,
		LastUsedAt:       now,
	})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// Authenticate returns the viewer for a session id, or ErrUnauthenticated. It
// records the use, at most once a minute. A session whose tokens cannot be
// opened (rotated key, corrupt row) is deleted.
func (s *Service) Authenticate(ctx context.Context, sessionID string) (Viewer, error) {
	session, ok, err := s.store.Session(ctx, hash(sessionID))
	if err != nil {
		return Viewer{}, fmt.Errorf("load session: %w", err)
	}
	now := s.opts.Now()
	if !ok || now.Sub(session.LastUsedAt) > SessionIdle || refreshExpired(session.RefreshExpiresAt, now) {
		return Viewer{}, ErrUnauthenticated
	}
	tokens, err := open(s.opts.Key, session.IDHash, session.SealedTokens)
	if err != nil {
		return Viewer{}, s.endSession(ctx, session.IDHash)
	}
	if now.Sub(session.LastUsedAt) >= touchInterval {
		if err := s.store.TouchSession(ctx, session.IDHash, now); err != nil {
			return Viewer{}, fmt.Errorf("touch session: %w", err)
		}
	}
	return Viewer{Profile: session.Profile, idHash: session.IDHash, tokens: tokens}, nil
}

// accessToken returns a valid GitHub access token for the viewer, refreshing it
// first when it is expired or about to be. GitHub rotates both tokens on every
// refresh, so refreshes are serialized per session and re-read under the lock.
// A refused refresh token or unreadable tokens delete the session; any failure
// returns ErrUnauthenticated.
func (s *Service) accessToken(ctx context.Context, v Viewer) (string, error) {
	if !s.needsRefresh(v.tokens) {
		return v.tokens.Access, nil
	}
	defer s.lockSession(v.idHash)()
	return s.currentToken(ctx, v.idHash)
}

// currentToken reloads the session and returns its access token, refreshing it
// when due. The caller holds the session lock.
func (s *Service) currentToken(ctx context.Context, idHash []byte) (string, error) {
	for {
		session, ok, err := s.store.Session(ctx, idHash)
		if err != nil {
			return "", fmt.Errorf("reload session: %w", err)
		}
		if !ok {
			return "", ErrUnauthenticated
		}
		tokens, err := open(s.opts.Key, session.IDHash, session.SealedTokens)
		if err != nil {
			return "", s.endSession(ctx, session.IDHash)
		}
		if !s.needsRefresh(tokens) {
			return tokens.Access, nil
		}
		if refreshExpired(tokens.RefreshExpiresAt, s.opts.Now()) {
			return "", ErrUnauthenticated
		}

		fresh, swapped, err := s.rotate(ctx, session, tokens)
		if errors.Is(err, ErrRefreshRefused) {
			return "", s.endSession(ctx, session.IDHash)
		}
		if err != nil {
			return "", err
		}
		if swapped {
			return fresh.Access, nil
		}
	}
}

// rotate refreshes tokens and stores the result. It is detached from ctx's
// cancellation: once GitHub has rotated the tokens, a client disconnect must
// not lose the new ones.
func (s *Service) rotate(ctx context.Context, session Session, tokens Tokens) (Tokens, bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rotateTimeout)
	defer cancel()

	fresh, err := s.github.Refresh(ctx, tokens.Refresh)
	if errors.Is(err, ErrRefreshRefused) {
		return Tokens{}, false, fmt.Errorf("refresh: %w", err)
	}
	if err != nil {
		return Tokens{}, false, fmt.Errorf("refresh GitHub token: %w: %w", ErrUnauthenticated, err)
	}
	var persistErr error
	for range persistAttempts {
		var swapped bool
		swapped, persistErr = s.persist(ctx, session, fresh)
		if persistErr == nil {
			return fresh, swapped, nil
		}
	}
	s.opts.Logger.Error("store refreshed tokens", "err", persistErr, "attempts", persistAttempts)
	return Tokens{}, false, fmt.Errorf("store refreshed tokens: %w: %w", ErrUnauthenticated, persistErr)
}

func (s *Service) persist(ctx context.Context, session Session, fresh Tokens) (bool, error) {
	sealed, err := seal(s.opts.Key, session.IDHash, fresh)
	if err != nil {
		return false, err
	}
	swapped, err := s.store.SwapTokens(ctx, session.IDHash, session.Version, sealed, fresh.AccessExpiresAt, fresh.RefreshExpiresAt)
	if err != nil {
		return false, fmt.Errorf("swap tokens: %w", err)
	}
	return swapped, nil
}

// endSession deletes the session and returns ErrUnauthenticated, or the delete error.
func (s *Service) endSession(ctx context.Context, idHash []byte) error {
	if err := s.deleteSession(ctx, idHash); err != nil {
		return err
	}
	return ErrUnauthenticated
}

// deleteSession deletes the session, then drops its cached access, so a cache
// write that raced the delete is dropped too.
func (s *Service) deleteSession(ctx context.Context, idHash []byte) error {
	if err := s.store.DeleteSession(ctx, idHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	s.forgetAccess(idHash)
	return nil
}

func (s *Service) needsRefresh(t Tokens) bool {
	return !t.AccessExpiresAt.IsZero() && s.opts.Now().Add(refreshMargin).After(t.AccessExpiresAt)
}

func refreshExpired(expiresAt, now time.Time) bool {
	return !expiresAt.IsZero() && now.After(expiresAt)
}

// sessionLock serializes one session's refreshes; waiters counts the holders
// and queued callers so the entry is dropped once nobody needs it.
type sessionLock struct {
	mu      sync.Mutex
	waiters int
}

// lockSession locks idHash's session and returns its unlock.
func (s *Service) lockSession(idHash []byte) (unlock func()) {
	key := string(idHash)
	s.locksMu.Lock()
	l, ok := s.locks[key]
	if !ok {
		l = &sessionLock{}
		s.locks[key] = l
	}
	l.waiters++
	s.locksMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.locksMu.Lock()
		if l.waiters--; l.waiters == 0 {
			delete(s.locks, key)
		}
		s.locksMu.Unlock()
	}
}

// Logout deletes the session and its cached access, then revokes its GitHub
// access token; an unknown id is not an error. When only the revoke fails it returns
// ErrRevokeFailed wrapping the cause: the session is already gone, so callers
// log it and still answer success.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	idHash := hash(sessionID)
	defer s.lockSession(idHash)()

	// A client disconnect must not leave the session alive.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), logoutTimeout)
	defer cancel()
	session, ok, err := s.store.Session(ctx, idHash)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}
	if !ok {
		return nil
	}
	if err := s.deleteSession(ctx, idHash); err != nil {
		return err
	}
	tokens, err := open(s.opts.Key, idHash, session.SealedTokens)
	if err != nil {
		return nil
	}
	ctx, cancel = context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	if err := s.github.Revoke(ctx, tokens.Access); err != nil {
		return fmt.Errorf("%w: %w", ErrRevokeFailed, err)
	}
	return nil
}

func randomToken() string {
	b := make([]byte, randomBytes)
	// crypto/rand.Read never returns an error since Go 1.24.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
