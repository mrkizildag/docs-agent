package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	return c.requestTokens(ctx, "exchange code", auth.ErrInvalidLogin, url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURL},
		"code_verifier": {verifier},
	})
}

// Refresh trades a refresh token for a new token pair; GitHub rotates both.
// Refusals, such as bad_refresh_token, wrap auth.ErrUnauthenticated.
func (c *UserClient) Refresh(ctx context.Context, refreshToken string) (auth.Tokens, error) {
	return c.requestTokens(ctx, "refresh token", auth.ErrUnauthenticated, url.Values{
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

// requestTokens posts form to the token endpoint; refusals wrap refused.
func (c *UserClient) requestTokens(ctx context.Context, op string, refused error, form url.Values) (auth.Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauthURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return auth.Tokens{}, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	status, body, err := c.do(req)
	if err != nil {
		return auth.Tokens{}, fmt.Errorf("%s: %w", op, err)
	}
	var resp tokenResponse
	decodeErr := json.Unmarshal(body, &resp)

	switch {
	case resp.Error != "":
		return auth.Tokens{}, fmt.Errorf("%s: GitHub answered %q: %w", op, resp.Error, refused)
	case status >= 400 && status < 500:
		return auth.Tokens{}, fmt.Errorf("%s: GitHub answered HTTP %d: %w", op, status, refused)
	case status != http.StatusOK:
		return auth.Tokens{}, fmt.Errorf("%s: GitHub answered HTTP %d", op, status)
	case decodeErr != nil:
		return auth.Tokens{}, fmt.Errorf("decode token response: %w", decodeErr)
	case resp.AccessToken == "":
		return auth.Tokens{}, fmt.Errorf("%s: no access token in the response: %w", op, refused)
	}

	now := time.Now()
	tokens := auth.Tokens{Access: resp.AccessToken, Refresh: resp.RefreshToken}
	if resp.ExpiresIn > 0 {
		tokens.AccessExpiresAt = now.Add(time.Duration(resp.ExpiresIn) * time.Second)
	}
	if resp.RefreshTokenExpiresIn > 0 {
		tokens.RefreshExpiresAt = now.Add(time.Duration(resp.RefreshTokenExpiresIn) * time.Second)
	}
	return tokens, nil
}

// Revoke invalidates an access token at GitHub.
func (c *UserClient) Revoke(ctx context.Context, accessToken string) error {
	payload, err := json.Marshal(map[string]string{"access_token": accessToken})
	if err != nil {
		return fmt.Errorf("encode revoke request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiURL+"/applications/"+url.PathEscape(c.clientID)+"/token", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build revoke request: %w", err)
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	status, _, err := c.do(req)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("revoke token: GitHub answered HTTP %d", status)
	}
	return nil
}

// User returns the user the access token belongs to.
func (c *UserClient) User(ctx context.Context, accessToken string) (auth.Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+"/user", nil)
	if err != nil {
		return auth.Profile{}, fmt.Errorf("build user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	status, body, err := c.do(req)
	if err != nil {
		return auth.Profile{}, fmt.Errorf("get user: %w", err)
	}
	if status != http.StatusOK {
		return auth.Profile{}, fmt.Errorf("get user: GitHub answered HTTP %d", status)
	}
	var user struct {
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return auth.Profile{}, fmt.Errorf("decode user: %w", err)
	}
	if user.Login == "" {
		return auth.Profile{}, errors.New("get user: no login in the response")
	}
	return auth.Profile{Login: user.Login, AvatarURL: user.AvatarURL}, nil
}

func (c *UserClient) do(req *http.Request) (int, []byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("send %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUserBody))
	if err != nil {
		return 0, nil, fmt.Errorf("read %s %s response: %w", req.Method, req.URL.Path, err)
	}
	return resp.StatusCode, body, nil
}
