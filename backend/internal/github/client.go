// Package github adapts the GitHub REST API to the gate package's GitHub
// interface, authenticating as the docs-agent GitHub App.
package github

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	githubv88 "github.com/google/go-github/v88/github"
	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/docs-agent/backend/internal/gate"
	"github.com/mrkizildag/docs-agent/backend/internal/review/actions"
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
	cloneTransports        map[cloneKey]*ghinstallation.Transport
	botLogin               string // "<app-slug>[bot]"; "" until resolved
}

// cloneKey identifies a token narrowed to one repository of an installation.
type cloneKey struct {
	installationID int64
	repo           string
}

var (
	_ gate.GitHub         = (*Client)(nil)
	_ actions.WorkflowAPI = (*Client)(nil)
)

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
		cloneTransports:        make(map[cloneKey]*ghinstallation.Transport),
	}, nil
}

// InstallationToken returns a short-lived installation access token for
// installationID, narrowed to contents:read on repo (a name without owner),
// to authenticate a git clone outside the go-github client.
func (c *Client) InstallationToken(ctx context.Context, installationID int64, repo string) (string, error) {
	transport, err := c.cloneTransport(installationID, repo)
	if err != nil {
		return "", fmt.Errorf("installation token %d for %s: %w", installationID, repo, err)
	}

	token, err := transport.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("installation token %d for %s: %w", installationID, repo, err)
	}
	return token, nil
}

// CreateCheckRun creates a check run on head_sha in owner/repo, authenticating
// as installationID, and returns its ID. An in-progress run carries no conclusion.
func (c *Client) CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run gate.CheckRun) (int64, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return 0, fmt.Errorf("create check run %s/%s: %w", owner, repo, err)
	}

	opts := github.CreateCheckRunOptions{
		Name:    run.Name,
		HeadSHA: run.HeadSHA,
		Status:  new(checkStatus(run)),
		Output: &github.CheckRunOutput{
			Title:   &run.Title,
			Summary: &run.Summary,
		},
	}
	if run.Status != gate.StatusInProgress {
		opts.Conclusion = new(string(run.Conclusion))
	}

	created, _, err := client.Checks.CreateCheckRun(ctx, owner, repo, opts)
	if err != nil {
		return 0, fmt.Errorf("create check run %s/%s: %w", owner, repo, err)
	}

	return created.GetID(), nil
}

// UpdateCheckRun replaces check run id's status, conclusion and output.
func (c *Client) UpdateCheckRun(ctx context.Context, installationID int64, owner, repo string, id int64, run gate.CheckRun) error {
	client, err := c.installationClient(installationID)
	if err != nil {
		return fmt.Errorf("update check run %d of %s/%s: %w", id, owner, repo, err)
	}

	opts := github.UpdateCheckRunOptions{
		Name:   run.Name,
		Status: new(checkStatus(run)),
		Output: &github.CheckRunOutput{
			Title:   &run.Title,
			Summary: &run.Summary,
		},
	}
	if run.Status != gate.StatusInProgress {
		opts.Conclusion = new(string(run.Conclusion))
	}

	if _, _, err := client.Checks.UpdateCheckRun(ctx, owner, repo, id, opts); err != nil {
		return fmt.Errorf("update check run %d of %s/%s: %w", id, owner, repo, err)
	}

	return nil
}

func checkStatus(run gate.CheckRun) string {
	if run.Status == gate.StatusInProgress {
		return string(gate.StatusInProgress)
	}
	return string(gate.StatusCompleted)
}

// WorkflowExists reports whether owner/repo's default branch has gate.WorkflowPath.
func (c *Client) WorkflowExists(ctx context.Context, installationID int64, owner, repo string) (bool, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return false, fmt.Errorf("check workflow %s/%s: %w", owner, repo, err)
	}

	_, _, resp, err := client.Repositories.GetContents(ctx, owner, repo, gate.WorkflowPath, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("check workflow %s/%s: %w", owner, repo, err)
	}

	return true, nil
}

const (
	resultArtifactName = "docs-agent-result"
	resultFileName     = "result.json"
	maxArtifactBytes   = 10 << 20
)

// Dispatch runs the docs-agent workflow on owner/repo's default branch and
// returns the ID of the run it started.
func (c *Client) Dispatch(ctx context.Context, installationID int64, owner, repo string, in actions.DispatchInputs) (int64, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return 0, fmt.Errorf("dispatch workflow %s/%s: %w", owner, repo, err)
	}

	r, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("dispatch workflow %s/%s: get repository: %w", owner, repo, err)
	}

	details, _, err := client.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repo, path.Base(gate.WorkflowPath),
		github.CreateWorkflowDispatchEventRequest{
			Ref: r.GetDefaultBranch(),
			Inputs: map[string]any{
				"head_sha":  in.HeadSHA,
				"pr_number": strconv.Itoa(in.PRNumber),
				"nonce":     in.Nonce,
			},
			ReturnRunDetails: new(true),
		})
	if err != nil {
		return 0, fmt.Errorf("dispatch workflow %s/%s: %w", owner, repo, err)
	}
	if details.GetWorkflowRunID() == 0 {
		return 0, fmt.Errorf("dispatch workflow %s/%s: response has no workflow_run_id", owner, repo)
	}

	return details.GetWorkflowRunID(), nil
}

// ResultArtifact returns the result.json inside run runID's result artifact.
func (c *Client) ResultArtifact(ctx context.Context, installationID int64, owner, repo string, runID int64) ([]byte, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("fetch result of run %d of %s/%s: %w", runID, owner, repo, err)
	}

	list, _, err := client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("fetch result of run %d of %s/%s: list artifacts: %w", runID, owner, repo, err)
	}

	var artifactID int64
	for _, a := range list.Artifacts {
		if a.GetName() == resultArtifactName && !a.GetExpired() && a.GetWorkflowRun().GetID() == runID {
			artifactID = a.GetID()
			break
		}
	}
	if artifactID == 0 {
		return nil, fmt.Errorf("fetch result of run %d of %s/%s: no %s artifact", runID, owner, repo, resultArtifactName)
	}

	archiveURL, _, err := client.Actions.DownloadArtifact(ctx, owner, repo, artifactID, 1)
	if err != nil {
		return nil, fmt.Errorf("fetch result of run %d of %s/%s: locate artifact %d: %w", runID, owner, repo, artifactID, err)
	}

	result, err := c.downloadResult(ctx, archiveURL.String())
	if err != nil {
		return nil, fmt.Errorf("fetch result of run %d of %s/%s: %w", runID, owner, repo, err)
	}
	return result, nil
}

// downloadResult fetches the zip at archiveURL, a pre-signed link that takes
// no installation token, and returns its result.json.
func (c *Client) downloadResult(ctx context.Context, archiveURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download artifact: status %s", resp.Status)
	}

	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download artifact: %w", err)
	}
	if len(archive) > maxArtifactBytes {
		return nil, fmt.Errorf("download artifact: larger than %d bytes", maxArtifactBytes)
	}

	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open artifact zip: %w", err)
	}
	file, err := zr.Open(resultFileName)
	if err != nil {
		return nil, fmt.Errorf("open %s in artifact: %w", resultFileName, err)
	}
	defer func() { _ = file.Close() }()

	result, err := io.ReadAll(io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s in artifact: %w", resultFileName, err)
	}
	if len(result) > maxArtifactBytes {
		return nil, fmt.Errorf("read %s in artifact: larger than %d bytes", resultFileName, maxArtifactBytes)
	}
	return result, nil
}

func (c *Client) installationClient(installationID int64) (*github.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if client, ok := c.installationClients[installationID]; ok {
		return client, nil
	}

	installationTransport, err := c.installationTransportLocked(installationID)
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

func (c *Client) cloneTransport(installationID int64, repo string) (*ghinstallation.Transport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := cloneKey{installationID: installationID, repo: repo}
	if transport, ok := c.cloneTransports[key]; ok {
		return transport, nil
	}

	transport, err := c.newTransport(installationID)
	if err != nil {
		return nil, err
	}
	transport.InstallationTokenOptions = &githubv88.InstallationTokenOptions{
		Repositories: []string{repo},
		Permissions:  &githubv88.InstallationPermissions{Contents: new("read")},
	}

	c.cloneTransports[key] = transport
	return transport, nil
}

// installationTransportLocked returns installationID's full-scope transport,
// creating and caching it on first use. Callers must hold c.mu.
func (c *Client) installationTransportLocked(installationID int64) (*ghinstallation.Transport, error) {
	if transport, ok := c.installationTransports[installationID]; ok {
		return transport, nil
	}

	transport, err := c.newTransport(installationID)
	if err != nil {
		return nil, err
	}

	c.installationTransports[installationID] = transport
	return transport, nil
}

func (c *Client) newTransport(installationID int64) (*ghinstallation.Transport, error) {
	// ghinstallation.refreshToken mutates the AppsTransport it wraps, so each
	// transport needs its own rather than sharing one across goroutines.
	appsTransport, err := ghinstallation.NewAppsTransport(c.transport, c.appID, c.privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport for installation %d: %w", installationID, err)
	}
	if c.baseURL != "" {
		appsTransport.BaseURL = c.baseURL
	}

	transport := ghinstallation.NewFromAppsTransport(appsTransport, installationID)
	transport.Client = c.httpClient
	return transport, nil
}

// appBotLogin returns the login GitHub gives the App's bot user, resolving the
// App's slug once with an App JWT.
func (c *Client) appBotLogin(ctx context.Context) (string, error) {
	c.mu.Lock()
	login := c.botLogin
	c.mu.Unlock()
	if login != "" {
		return login, nil
	}

	appsTransport, err := ghinstallation.NewAppsTransport(c.transport, c.appID, c.privateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("create GitHub App transport for app %d: %w", c.appID, err)
	}
	if c.baseURL != "" {
		appsTransport.BaseURL = c.baseURL
	}
	opts := []github.ClientOptionsFunc{github.WithHTTPClient(&http.Client{Transport: appsTransport, Timeout: c.httpClient.Timeout})}
	if c.baseURL != "" {
		opts = append(opts, github.WithURLs(&c.baseURL, &c.baseURL))
	}
	client, err := github.NewClient(opts...)
	if err != nil {
		return "", fmt.Errorf("create GitHub App client for app %d: %w", c.appID, err)
	}

	app, _, err := client.Apps.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("get GitHub App %d: %w", c.appID, err)
	}
	if app.GetSlug() == "" {
		return "", fmt.Errorf("get GitHub App %d: response has no slug", c.appID)
	}

	login = app.GetSlug() + "[bot]"
	c.mu.Lock()
	c.botLogin = login
	c.mu.Unlock()
	return login, nil
}
