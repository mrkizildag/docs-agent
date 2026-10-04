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

	mu                     sync.Mutex
	installationClients    map[int64]*github.Client
	installationTransports map[int64]*ghinstallation.Transport
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
		transport:              transport,
		appID:                  appID,
		privateKeyPEM:          privateKeyPEM,
		httpClient:             httpClient,
		baseURL:                baseURL,
		installationClients:    make(map[int64]*github.Client),
		installationTransports: make(map[int64]*ghinstallation.Transport),
	}, nil
}

// InstallationToken returns a short-lived installation access token for
// installationID, authenticating a git clone or other call outside the
// go-github client.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	transport, err := c.installationTransport(installationID)
	if err != nil {
		return "", fmt.Errorf("installation token %d: %w", installationID, err)
	}

	token, err := transport.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("installation token %d: %w", installationID, err)
	}
	return token, nil
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

// workflowPath is the Actions workflow whose presence on the default branch
// means a repo runs analysis through Actions rather than the server.
const workflowPath = ".github/workflows/docs-agent.yml"

// WorkflowExists reports whether owner/repo's default branch has workflowPath.
func (c *Client) WorkflowExists(ctx context.Context, installationID int64, owner, repo string) (bool, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return false, fmt.Errorf("check workflow %s/%s: %w", owner, repo, err)
	}

	_, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, workflowPath, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("check workflow %s/%s: %w", owner, repo, err)
	}

	return true, nil
}

func (c *Client) installationClient(installationID int64) (*github.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if client, ok := c.installationClients[installationID]; ok {
		return client, nil
	}

	installationTransport, err := c.newInstallationTransportLocked(installationID)
	if err != nil {
		return nil, err
	}

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

func (c *Client) installationTransport(installationID int64) (*ghinstallation.Transport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if transport, ok := c.installationTransports[installationID]; ok {
		return transport, nil
	}
	return c.newInstallationTransportLocked(installationID)
}

// newInstallationTransportLocked creates and caches installationID's
// transport. Callers must hold c.mu.
func (c *Client) newInstallationTransportLocked(installationID int64) (*ghinstallation.Transport, error) {
	if transport, ok := c.installationTransports[installationID]; ok {
		return transport, nil
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

	c.installationTransports[installationID] = installationTransport
	return installationTransport, nil
}
