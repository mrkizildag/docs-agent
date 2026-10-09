package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/auth"
)

const (
	defaultOAuthURL = "https://github.com"
	defaultAPIURL   = "https://api.github.com"
	maxUserBody     = 1 << 20
)

var _ auth.GitHubUser = (*UserClient)(nil)

// UserClient speaks to GitHub on behalf of a signed-in user: the OAuth code
// exchange, and user-token API calls.
type UserClient struct {
	httpClient   *http.Client
	clientID     string
	clientSecret string
	oauthURL     string
	apiURL       string
}

// NewUserClient returns a UserClient for the GitHub App's OAuth credentials.
// Empty oauthURL and apiURL target github.com; tests pass httptest URLs.
func NewUserClient(httpClient *http.Client, clientID, clientSecret, oauthURL, apiURL string) *UserClient {
	if oauthURL == "" {
		oauthURL = defaultOAuthURL
	}
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	return &UserClient{
		httpClient:   httpClient,
		clientID:     clientID,
		clientSecret: clientSecret,
		oauthURL:     strings.TrimRight(oauthURL, "/"),
		apiURL:       strings.TrimRight(apiURL, "/"),
	}
}

type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
}

// Exchange trades an authorization code for the user's tokens. GitHub reports
// most failures as HTTP 200 with an "error" field, and some as 4xx; both wrap
// auth.ErrInvalidLogin.
func (c *UserClient) Exchange(ctx context.Context, code, verifier, redirectURL string) (auth.Tokens, error) {
	return c.requestTokens(ctx, "exchange code", auth.ErrInvalidLogin, func(string) bool { return true }, url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURL},
		"code_verifier": {verifier},
	})
}

// Refresh trades a refresh token for a new token pair; GitHub rotates both.
// Only bad_refresh_token wraps auth.ErrRefreshRefused; every other failure is
// a plain error, so a transient one does not end the session.
func (c *UserClient) Refresh(ctx context.Context, refreshToken string) (auth.Tokens, error) {
	return c.requestTokens(ctx, "refresh token", auth.ErrRefreshRefused, func(code string) bool { return code == "bad_refresh_token" }, url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

// requestTokens posts form to the token endpoint. A failure wraps refused when
// isRefusal reports true for GitHub's error code ("" when it sent none).
func (c *UserClient) requestTokens(ctx context.Context, op string, refused error, isRefusal func(code string) bool, form url.Values) (auth.Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauthURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return auth.Tokens{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return auth.Tokens{}, fmt.Errorf("%s: send token request: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUserBody))
	if err != nil {
		return auth.Tokens{}, fmt.Errorf("%s: read token response: %w", op, err)
	}
	status := resp.StatusCode
	var parsed tokenResponse
	decodeErr := json.Unmarshal(body, &parsed)

	switch {
	case parsed.Error != "" && isRefusal(parsed.Error):
		return auth.Tokens{}, fmt.Errorf("%s: GitHub answered %q: %w", op, parsed.Error, refused)
	case parsed.Error != "":
		return auth.Tokens{}, fmt.Errorf("%s: GitHub answered %q", op, parsed.Error)
	case status >= 400 && status < 500 && isRefusal(""):
		return auth.Tokens{}, fmt.Errorf("%s: GitHub answered HTTP %d: %w", op, status, refused)
	case status != http.StatusOK:
		return auth.Tokens{}, fmt.Errorf("%s: GitHub answered HTTP %d", op, status)
	case decodeErr != nil:
		return auth.Tokens{}, fmt.Errorf("decode token response: %w", decodeErr)
	case parsed.AccessToken == "" && isRefusal(""):
		return auth.Tokens{}, fmt.Errorf("%s: no access token in the response: %w", op, refused)
	case parsed.AccessToken == "":
		return auth.Tokens{}, fmt.Errorf("%s: no access token in the response", op)
	}

	now := time.Now()
	tokens := auth.Tokens{Access: parsed.AccessToken, Refresh: parsed.RefreshToken}
	if parsed.ExpiresIn > 0 {
		tokens.AccessExpiresAt = now.Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	if parsed.RefreshTokenExpiresIn > 0 {
		tokens.RefreshExpiresAt = now.Add(time.Duration(parsed.RefreshTokenExpiresIn) * time.Second)
	}
	return tokens, nil
}

// AuthorizeURL is where GitHub's OAuth authorization page lives.
func (c *UserClient) AuthorizeURL() string {
	return c.oauthURL + "/login/oauth/authorize"
}

// String and LogValue keep the client secret out of logs and error text.
func (c UserClient) String() string {
	return fmt.Sprintf("UserClient{clientID: %s, clientSecret: [REDACTED]}", c.clientID)
}

func (c UserClient) GoString() string { return c.String() }

func (c UserClient) LogValue() slog.Value {
	return slog.GroupValue(slog.String("client_id", c.clientID), slog.String("client_secret", "[REDACTED]"))
}

// apiClient returns a go-github client that authenticates with the user token.
func (c *UserClient) apiClient(accessToken string) (*github.Client, error) {
	client, err := github.NewClient(github.WithHTTPClient(c.httpClient), github.WithURLs(&c.apiURL, &c.apiURL), github.WithAuthToken(accessToken))
	if err != nil {
		return nil, fmt.Errorf("create GitHub client: %w", err)
	}
	return client, nil
}

// Revoke revokes one access token. The whole grant is left alone: it spans the
// user's other sessions, and this session's refresh token dies with its row.
func (c *UserClient) Revoke(ctx context.Context, accessToken string) error {
	basic := &github.BasicAuthTransport{Username: c.clientID, Password: c.clientSecret, Transport: c.httpClient.Transport}
	client, err := github.NewClient(
		github.WithHTTPClient(&http.Client{Transport: basic, Timeout: c.httpClient.Timeout}),
		github.WithURLs(&c.apiURL, &c.apiURL),
	)
	if err != nil {
		return fmt.Errorf("create GitHub client: %w", err)
	}
	if _, err := client.Authorizations.Revoke(ctx, c.clientID, accessToken); err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

// User returns the user the access token belongs to.
func (c *UserClient) User(ctx context.Context, accessToken string) (auth.Profile, error) {
	client, err := c.apiClient(accessToken)
	if err != nil {
		return auth.Profile{}, err
	}
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return auth.Profile{}, userAPIError("get user", err)
	}
	if user.GetLogin() == "" {
		return auth.Profile{}, errors.New("get user: no login in the response")
	}
	return auth.Profile{Login: user.GetLogin(), AvatarURL: user.GetAvatarURL()}, nil
}

// userAPIError wraps auth.ErrUnauthenticated when GitHub answered 401.
func userAPIError(op string, err error) error {
	if apiErrorStatus(err) == http.StatusUnauthorized {
		return fmt.Errorf("%s: %w: %w", op, auth.ErrUnauthenticated, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func apiErrorStatus(err error) int {
	var apiErr *github.ErrorResponse
	if errors.As(err, &apiErr) && apiErr.Response != nil {
		return apiErr.Response.StatusCode
	}
	return 0
}
