// Package github adapts the GitHub REST API to the gate package's GitHub,
// CommentGitHub and ScaffoldGitHub interfaces and the actions package's
// WorkflowAPI, authenticating as the pollux-agent GitHub App.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	// ghinstallation's InstallationTokenOptions is typed with go-github v88.
	githubv88 "github.com/google/go-github/v88/github"
	"github.com/google/go-github/v92/github"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
)

// Client implements gate.GitHub, gate.CommentGitHub, gate.ScaffoldGitHub and
// actions.WorkflowAPI, authenticating per installation as the pollux-agent
// GitHub App.
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

	blobs blobCache
}

// cloneKey identifies a token narrowed to one repository of an installation.
type cloneKey struct {
	installationID int64
	repo           string
}

var (
	_ gate.GitHub         = (*Client)(nil)
	_ gate.CommentGitHub  = (*Client)(nil)
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
		Output:  checkRunOutput(run),
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
		Output: checkRunOutput(run),
	}
	if run.Status != gate.StatusInProgress {
		opts.Conclusion = new(string(run.Conclusion))
	}

	if _, _, err := client.Checks.UpdateCheckRun(ctx, owner, repo, id, opts); err != nil {
		return fmt.Errorf("update check run %d of %s/%s: %w", id, owner, repo, err)
	}

	return nil
}

func checkRunOutput(run gate.CheckRun) *github.CheckRunOutput {
	return &github.CheckRunOutput{Title: &run.Title, Summary: &run.Summary}
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
		if isNotFound(resp) {
			return false, nil
		}
		return false, fmt.Errorf("check workflow %s/%s: %w", owner, repo, err)
	}

	return true, nil
}

// Dispatch runs the pollux-agent workflow on owner/repo's default branch and
// returns the ID of the run it started.
func (c *Client) Dispatch(ctx context.Context, installationID int64, owner, repo string, in actions.DispatchInputs) (int64, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return 0, fmt.Errorf("dispatch workflow %s/%s: %w", owner, repo, err)
	}

	docPaths := in.Docs
	if docPaths == nil {
		docPaths = []string{}
	}
	docsJSON, err := json.Marshal(docPaths)
	if err != nil {
		return 0, fmt.Errorf("dispatch workflow %s/%s: encode docs: %w", owner, repo, err)
	}

	defaultBranch, err := defaultBranchName(ctx, client, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("dispatch workflow %s/%s: %w", owner, repo, err)
	}

	details, _, err := client.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repo, path.Base(gate.WorkflowPath),
		github.CreateWorkflowDispatchEventRequest{
			Ref: defaultBranch,
			Inputs: map[string]any{
				actions.InputHeadSHA:  in.HeadSHA,
				actions.InputPRNumber: strconv.Itoa(in.PRNumber),
				actions.InputNonce:    in.Nonce,
				actions.InputDocs:     string(docsJSON),
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

// RunArtifact returns the zip of run runID's artifact called name; the caller
// closes it.
func (c *Client) RunArtifact(ctx context.Context, installationID int64, owner, repo string, runID int64, name string) (io.ReadCloser, error) {
	client, err := c.installationClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("fetch artifact of run %d of %s/%s: %w", runID, owner, repo, err)
	}

	list, _, err := client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("fetch artifact of run %d of %s/%s: list artifacts: %w", runID, owner, repo, err)
	}

	var artifactID int64
	for _, a := range list.Artifacts {
		if a.GetName() == name && !a.GetExpired() && a.GetWorkflowRun().GetID() == runID {
			artifactID = a.GetID()
			break
		}
	}
	if artifactID == 0 {
		return nil, fmt.Errorf("fetch artifact of run %d of %s/%s: no %s artifact", runID, owner, repo, name)
	}

	archiveURL, _, err := client.Actions.DownloadArtifact(ctx, owner, repo, artifactID, 1)
	if err != nil {
		return nil, fmt.Errorf("fetch artifact of run %d of %s/%s: locate artifact %d: %w", runID, owner, repo, artifactID, err)
	}

	// The link is pre-signed and takes no installation token.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch artifact of run %d of %s/%s: download artifact: %w", runID, owner, repo, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch artifact of run %d of %s/%s: download artifact: %w", runID, owner, repo, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("fetch artifact of run %d of %s/%s: download artifact: status %s", runID, owner, repo, resp.Status)
	}
	return resp.Body, nil
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

	client, err := c.apiClient(installationTransport)
	if err != nil {
		return nil, fmt.Errorf("create GitHub client for installation %d: %w", installationID, err)
	}

	c.installationClients[installationID] = client
	return client, nil
}

// apiClient returns a go-github client that authenticates through transport.
func (c *Client) apiClient(transport http.RoundTripper) (*github.Client, error) {
	opts := []github.ClientOptionsFunc{github.WithHTTPClient(&http.Client{Transport: transport, Timeout: c.httpClient.Timeout})}
	if c.baseURL != "" {
		opts = append(opts, github.WithURLs(&c.baseURL, &c.baseURL))
	}
	client, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("create github client: %w", err)
	}
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

// newAppsTransport returns an App-JWT transport of its own: ghinstallation's
// token refresh mutates it, so it must not be shared across goroutines.
func (c *Client) newAppsTransport() (*ghinstallation.AppsTransport, error) {
	appsTransport, err := ghinstallation.NewAppsTransport(c.transport, c.appID, c.privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("create GitHub App transport for app %d: %w", c.appID, err)
	}
	if c.baseURL != "" {
		appsTransport.BaseURL = c.baseURL
	}
	return appsTransport, nil
}

func (c *Client) newTransport(installationID int64) (*ghinstallation.Transport, error) {
	appsTransport, err := c.newAppsTransport()
	if err != nil {
		return nil, fmt.Errorf("installation %d: %w", installationID, err)
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

	appsTransport, err := c.newAppsTransport()
	if err != nil {
		return "", err
	}
	client, err := c.apiClient(appsTransport)
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
