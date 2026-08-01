package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v69/github"
	"github.com/monsterxx03/tachi/pkg/httpx"
	"github.com/monsterxx03/tachi/pkg/logger"
	"golang.org/x/oauth2"
)

// GitHubClient wraps the go-github client with logging and rate-limit awareness.
type GitHubClient struct {
	client *github.Client
	logger *logger.Logger
	login  string // cached bot login (from GET /user)
}

// NewGitHubClient creates a new GitHub API client using a PAT.
// If proxyURL is non-empty, all HTTP requests are routed through that proxy.
// Supported proxy schemes: http, https, socks5.
func NewGitHubClient(ctx context.Context, token string, proxyURL string, log *logger.Logger) (*GitHubClient, error) {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := newOAuthClient(ctx, ts, proxyURL)
	client := github.NewClient(tc)

	return newClientFromHTTP(ctx, client, log)
}

// NewGitHubClientFromApp creates a new GitHub API client using GitHub App authentication.
// It generates a JWT, exchanges it for an installation access token, and uses that
// token for API calls. The token is valid for 1 hour; a new one is obtained on each call
// (the oauth2.TokenSource handles caching).
// If proxyURL is non-empty, all HTTP requests (including installation token exchange)
// are routed through that proxy. Supported proxy schemes: http, https, socks5.
func NewGitHubClientFromApp(ctx context.Context, appID int64, privateKeyPath string, installationID int64, proxyURL string, log *logger.Logger) (*GitHubClient, error) {
	key, err := parsePrivateKey(privateKeyPath)
	if err != nil {
		return nil, err
	}

	// Build a proxy-enabled HTTP client for installation token requests.
	// An invalid proxy config falls back to a plain client (with a warning
	// logged by httpx.NewClient).
	httpClient := httpx.NewClient(30*time.Second, proxyURL)

	// Create a token source that generates a fresh installation token when needed.
	ts := &appTokenSource{
		appID:          appID,
		key:            key,
		installationID: installationID,
		httpClient:     httpClient,
	}

	tc := newOAuthClient(ctx, ts, proxyURL)
	client := github.NewClient(tc)

	return newClientFromHTTP(ctx, client, log)
}

// appTokenSource implements oauth2.TokenSource for GitHub App installation tokens.
type appTokenSource struct {
	appID          int64
	key            *rsa.PrivateKey
	installationID int64
	httpClient     *http.Client // HTTP client with optional proxy support
}

// Token returns a valid installation access token.
func (s *appTokenSource) Token() (*oauth2.Token, error) {
	// Generate installation token using the shared helper.
	// We already have the parsed key, so we pass it directly.
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": s.appID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	jwtStr, err := token.SignedString(s.key)
	if err != nil {
		return nil, fmt.Errorf("github: sign JWT: %w", err)
	}

	result, err := requestInstallationToken(context.Background(), s.installationID, jwtStr, s.httpClient)
	if err != nil {
		return nil, err
	}

	return &oauth2.Token{
		AccessToken: result.Token,
		Expiry:      result.ExpiresAt,
	}, nil
}

// installationTokenResponse holds the response from the GitHub API for an installation token request.
type installationTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// newOAuthClient creates an *http.Client that adds OAuth2 tokens to requests
// and optionally routes through a proxy.
//
// When proxyURL is non-empty, all HTTP requests go through that proxy.
// Supported proxy schemes: http, https, socks5 (see pkg/httpx).
func newOAuthClient(ctx context.Context, ts oauth2.TokenSource, proxyURL string) *http.Client {
	baseTransport := http.DefaultTransport

	if proxyURL != "" {
		// httpx.NewClient falls back to a plain client when the proxy config
		// is invalid; its Transport is nil then, so the DefaultTransport
		// stays in effect (same behavior as the previous explicit check).
		if proxyClient := httpx.NewClient(30*time.Second, proxyURL); proxyClient.Transport != nil {
			baseTransport = proxyClient.Transport
		}
	}

	return &http.Client{
		Transport: &oauth2.Transport{
			Source: ts,
			Base:   baseTransport,
		},
	}
}

// newClientFromHTTP creates a GitHubClient from an existing http.Client.
func newClientFromHTTP(ctx context.Context, client *github.Client, log *logger.Logger) (*GitHubClient, error) {
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("github: verify token failed: %w", err)
	}

	gc := &GitHubClient{
		client: client,
		logger: log,
		login:  user.GetLogin(),
	}

	gc.logger.Info(ctx, "github: authenticated", "login", gc.login, "type", user.GetType())
	return gc, nil
}

// Login returns the bot's GitHub login name.
func (c *GitHubClient) Login() string {
	return c.login
}

// IsBotUser returns true if the given login is the bot itself.
func (c *GitHubClient) IsBotUser(login string) bool {
	return login == c.login
}

// Client returns the underlying go-github client for direct API access.
func (c *GitHubClient) Client() *github.Client {
	return c.client
}

// ListIssues lists open issues updated since the given time.
func (c *GitHubClient) ListIssues(ctx context.Context, owner, repo string, since time.Time) ([]*github.Issue, error) {
	opts := &github.IssueListByRepoOptions{
		Since:       since,
		Sort:        "updated",
		Direction:   "asc",
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	var all []*github.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list issues for %s/%s: %w", owner, repo, err)
		}
		for _, issue := range issues {
			if !issue.IsPullRequest() {
				all = append(all, issue)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// ListComments lists all comments on an issue.
func (c *GitHubClient) ListComments(ctx context.Context, owner, repo string, number int) ([]*github.IssueComment, error) {
	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []*github.IssueComment
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list comments for %s/%s#%d: %w", owner, repo, number, err)
		}
		all = append(all, comments...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// CreateComment creates a comment on an issue.
func (c *GitHubClient) CreateComment(ctx context.Context, owner, repo string, number int, body string) (*github.IssueComment, error) {
	comment, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{
		Body: github.Ptr(body),
	})
	if err != nil {
		return nil, fmt.Errorf("github: create comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return comment, nil
}

// CreatePR creates a pull request.
func (c *GitHubClient) CreatePR(ctx context.Context, owner, repo, title, body, head, base string, draft bool) (*github.PullRequest, error) {
	pr := &github.NewPullRequest{
		Title: github.Ptr(title),
		Body:  github.Ptr(body),
		Head:  github.Ptr(head),
		Base:  github.Ptr(base),
		Draft: github.Ptr(draft),
	}
	created, _, err := c.client.PullRequests.Create(ctx, owner, repo, pr)
	if err != nil {
		return nil, fmt.Errorf("github: create PR %s/%s: %w", owner, repo, err)
	}
	return created, nil
}

// GetIssueLabels fetches the labels on an issue.
func (c *GitHubClient) GetIssueLabels(ctx context.Context, owner, repo string, number int) ([]*github.Label, error) {
	labels, _, err := c.client.Issues.ListLabelsByIssue(ctx, owner, repo, number, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("github: list labels for %s/%s#%d: %w", owner, repo, number, err)
	}
	return labels, nil
}

// ListPullRequestReviewComments lists review comments on a pull request.
func (c *GitHubClient) ListPullRequestReviewComments(ctx context.Context, owner, repo string, prNumber int, since time.Time) ([]*github.PullRequestComment, error) {
	opts := &github.PullRequestListCommentsOptions{
		Sort:        "created",
		Direction:   "asc",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	if !since.IsZero() {
		opts.Since = since
	}

	var all []*github.PullRequestComment
	for {
		comments, resp, err := c.client.PullRequests.ListComments(ctx, owner, repo, prNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list PR review comments for %s/%s#%d: %w", owner, repo, prNumber, err)
		}
		all = append(all, comments...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// ParseRepo splits a "owner/repo" string into owner and repo parts.
func ParseRepo(name string) (owner, repo string, err error) {
	owner, repo, found := strings.Cut(name, "/")
	if !found || owner == "" || repo == "" {
		return "", "", fmt.Errorf("github: invalid repo name %q (expected format: owner/repo)", name)
	}
	return owner, repo, nil
}

// resolveInstallationToken returns the installation token for git push operations.
// This is a simpler path than the full oauth2 token source for git commands.
// requestInstallationToken exchanges a JWT for a GitHub App installation access token.
// Shared by appTokenSource.Token() and ResolveInstallationToken().
func requestInstallationToken(ctx context.Context, installationID int64, jwtToken string, httpClient *http.Client) (*installationTokenResponse, error) {
	url := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("github: create token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: request installation token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github: read token response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("github: get installation token: status=%d body=%s", resp.StatusCode, string(body))
	}

	var result installationTokenResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("github: parse token response: %w", err)
	}

	return &result, nil
}

// parsePrivateKey reads a PEM file and parses it as an RSA private key.
// Supports both PKCS1 and PKCS8 formats.
func parsePrivateKey(path string) (*rsa.PrivateKey, error) {
	keyBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("github: read private key: %w", err)
	}

	block, _ := pem.Decode(keyBytes)
	if block == nil {
		return nil, fmt.Errorf("github: no PEM data in private key")
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err == nil {
		return key, nil
	}

	// Try PKCS8
	key2, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err2 != nil {
		return nil, fmt.Errorf("github: parse private key: PKCS1: %v, PKCS8: %v", err, err2)
	}
	rsaKey, ok := key2.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github: private key is not RSA")
	}
	return rsaKey, nil
}

// generateAppJWT creates a signed JWT for GitHub App authentication.
func generateAppJWT(appID int64, key *rsa.PrivateKey) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"iat": now.Unix(),
		"exp": now.Add(10 * time.Minute).Unix(),
		"iss": appID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	jwtStr, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("github: sign JWT: %w", err)
	}
	return jwtStr, nil
}

// ResolveInstallationToken returns the installation token for git push operations.
// Uses the same shared helpers as appTokenSource.
func (c *Config) ResolveInstallationToken(ctx context.Context) (string, error) {
	if !c.HasGitHubApp() {
		return "", fmt.Errorf("github: GitHub App not configured")
	}
	app := c.GitHubApp

	key, err := parsePrivateKey(app.PrivateKeyPath)
	if err != nil {
		return "", err
	}

	jwtStr, err := generateAppJWT(app.AppID, key)
	if err != nil {
		return "", err
	}

	// Build a proxy-enabled HTTP client if configured. An invalid proxy
	// config falls back to a plain client (with a warning logged by
	// httpx.NewClient).
	var httpClient *http.Client
	if c.Proxy != "" {
		httpClient = httpx.NewClient(30*time.Second, c.Proxy)
	}

	result, err := requestInstallationToken(ctx, app.InstallationID, jwtStr, httpClient)
	if err != nil {
		return "", err
	}

	return result.Token, nil
}
