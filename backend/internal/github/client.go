// Package github adapts the GitHub REST API to the gate package's GitHub
// interface, authenticating as the docs-agent GitHub App.
package github

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
)

// Client creates GitHub check runs, authenticating per installation as the
// docs-agent GitHub App.
type Client struct {
	transport     http.RoundTripper
	appID         int64
	privateKeyPEM []byte
	httpClient    *http.Client
	baseURL       string

	mu                  sync.Mutex
	installationClients map[int64]*github.Client
}

var _ gate.GitHub = (*Client)(nil)

// NewClient returns a Client that signs GitHub App JWTs with privateKeyPEM
// and authenticates installation requests through httpClient's transport. An
// empty baseURL targets the public GitHub API; tests pass an httptest URL.
func NewClient(httpClient *http.Client, appID int64, privateKeyPEM []byte, baseURL string) (*Client, error) {
	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	// Validate the key eagerly so a bad key fails at startup, not on first use.
	if _, err := ghinstallation.NewAppsTransport(transport, appID, privateKeyPEM); err != nil {
		return nil, fmt.Errorf("create GitHub App transport for app %d: %w", appID, err)
	}

	return &Client{
		transport:           transport,
		appID:               appID,
		privateKeyPEM:       privateKeyPEM,
		httpClient:          httpClient,
		baseURL:             baseURL,
		installationClients: make(map[int64]*github.Client),
	}, nil
}

// CreateCheckRun creates a completed check run on head_sha in owner/repo,
// authenticating as installationID.
func (c *Client) CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run gate.CheckRun) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("create check run %s/%s: %w", owner, repo, err)
	}

	_, _, err = client.Checks.CreateCheckRun(ctx, owner, repo, github.CreateCheckRunOptions{
		Name:       run.Name,
		HeadSHA:    run.HeadSHA,
		Status:     new("completed"),
		Conclusion: new(string(run.Conclusion)),
		Output: &github.CheckRunOutput{
			Title:   &run.Title,
			Summary: &run.Summary,
		},
	})
	if err != nil {
		return fmt.Errorf("create check run %s/%s: %w", owner, repo, err)
	}

	return nil
}

func (c *Client) installationClient(installationID int64) (*github.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if client, ok := c.installationClients[installationID]; ok {
		return client, nil
	}

	// ghinstallation.refreshToken mutates the AppsTransport it wraps, so each
	// installation needs its own rather than sharing one across goroutines.
	appsTransport, err := ghinstallation.NewAppsTransport(c.transport, c.appID, c.privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport for installation %d: %w", installationID, err)
	}
	if c.baseURL != "" {
		appsTransport.BaseURL = c.baseURL
	}

	installationTransport := ghinstallation.NewFromAppsTransport(appsTransport, installationID)
	installationTransport.Client = c.httpClient

	httpClient := &http.Client{
		Transport: installationTransport,
		Timeout:   c.httpClient.Timeout,
	}

	opts := []github.ClientOptionsFunc{github.WithHTTPClient(httpClient)}
	if c.baseURL != "" {
		opts = append(opts, github.WithURLs(&c.baseURL, &c.baseURL))
	}

	client, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("create GitHub client for installation %d: %w", installationID, err)
	}

	c.installationClients[installationID] = client
	return client, nil
}
