package commitstatus

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
)

// State represents the commit status state sent to the Git provider.
type State string

const (
	StatePending State = "pending"
	StateSuccess State = "success"
	StateFailure State = "failure"
	StateError   State = "error"

	DefaultContext = "doco-cd/deploy"
)

// Provider identifies which SCM API to use when posting a commit status.
// The zero value ("") means auto-detect from the repository URL.
type Provider string

const (
	// ProviderAuto detects the provider from the repository URL (default).
	ProviderAuto Provider = ""
	// ProviderGitHub targets the GitHub REST API.
	// For github.com the public API is used; for any other host the GitHub
	// Enterprise Server endpoint (/api/v3) is used instead.
	ProviderGitHub Provider = "github"
	// ProviderGitLab targets the GitLab API v4.
	ProviderGitLab Provider = "gitlab"
	// ProviderGitea targets the Gitea/Forgejo API v1.
	ProviderGitea Provider = "gitea"
)

// ParseProvider converts a string (e.g. from an env var) to a Provider.
// Returns an error when the value is non-empty, not "auto", and not a known provider.
func ParseProvider(s string) (Provider, error) {
	switch Provider(strings.ToLower(strings.TrimSpace(s))) {
	case ProviderAuto, "auto":
		return ProviderAuto, nil
	case ProviderGitHub:
		return ProviderGitHub, nil
	case ProviderGitLab:
		return ProviderGitLab, nil
	case ProviderGitea, "forgejo":
		return ProviderGitea, nil
	default:
		return ProviderAuto, fmt.Errorf("unknown SCM provider %q: must be one of auto, github, gitlab, gitea, forgejo", s)
	}
}

// Status holds the information to post as a commit status.
type Status struct {
	State       State
	Description string
	// Context is the label shown in the Git UI (e.g. "doco-cd/deploy").
	// Defaults to DefaultContext when empty.
	Context   string
	TargetURL string // optional link to deployment logs
}

// Post posts a commit status to the appropriate Git provider.
// When provider is ProviderAuto ("") the provider is detected from repoURL.
// Returns nil (silently no-op) when token or commitSHA is empty.
func Post(ctx context.Context, provider Provider, repoURL, repoFullName, commitSHA, token string, status Status) error {
	token = strings.TrimSpace(token)
	commitSHA = strings.TrimSpace(commitSHA)

	if token == "" || commitSHA == "" {
		return nil
	}

	if status.Context == "" {
		status.Context = DefaultContext
	}

	host, scheme, err := parseHostAndScheme(repoURL)
	if err != nil {
		return fmt.Errorf("failed to parse repository URL: %w", err)
	}

	baseURL := scheme + "://" + host

	resolved := resolveProvider(provider, host)

	switch resolved {
	case ProviderGitLab:
		return postGitLab(ctx, baseURL, repoFullName, commitSHA, token, status)
	case ProviderGitHub:
		return postGitHub(ctx, baseURL, host, repoFullName, commitSHA, token, status)
	default: // ProviderGitea
		return postGitHubCompatible(ctx, baseURL, host, repoFullName, commitSHA, token, status)
	}
}

// resolveProvider returns the effective provider to use, falling back to
// URL-based detection when provider is ProviderAuto.
func resolveProvider(provider Provider, host string) Provider {
	if provider != ProviderAuto {
		return provider
	}

	if isGitLab(host) {
		return ProviderGitLab
	}

	if isGitHub(host) {
		return ProviderGitHub
	}

	return ProviderGitea
}

// isGitHub returns true for github.com (the SaaS product only; GitHub Enterprise
// cannot be reliably detected by hostname and must be set explicitly).
func isGitHub(host string) bool {
	return bareHost(host) == "github.com"
}

// isGitLab returns true when the host is exactly gitlab.com.
// Self-hosted GitLab at a custom domain requires GIT_SCM_PROVIDER=gitlab.
// The host argument may include a port (e.g. "gitlab.com:443").
func isGitLab(host string) bool {
	return strings.ToLower(bareHost(host)) == "gitlab.com"
}

// bareHost strips the port suffix from a host string (e.g. "example.com:8080" → "example.com").
// IPv6 addresses in brackets are handled correctly.
func bareHost(host string) string {
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i > 0 && strings.LastIndex(h, "]") < i {
		return h[:i]
	}

	return h
}

// parseHostAndScheme extracts the hostname and scheme from a git repository URL.
// Handles HTTPS, SSH (ssh://), and SCP-style git URLs (git@host:...).
func parseHostAndScheme(rawURL string) (string, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", "", errors.New("empty URL")
	}

	// SCP-style SSH: git@host:owner/repo.git
	if strings.Contains(rawURL, "@") && strings.Contains(rawURL, ":") && !strings.Contains(rawURL, "://") {
		parts := strings.SplitN(rawURL, "@", 2)
		if len(parts) == 2 {
			hostPath := strings.SplitN(parts[1], ":", 2)
			if len(hostPath) == 2 {
				return hostPath[0], "https", nil
			}
		}
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("invalid URL %q: %w", rawURL, err)
	}

	if parsed.Host == "" {
		return "", "", fmt.Errorf("no host in URL %q", rawURL)
	}

	scheme := parsed.Scheme
	if scheme == "" || scheme == "ssh" || scheme == "git" {
		scheme = "https"
	}

	return parsed.Host, scheme, nil
}

// postGitHub posts a commit status using the GitHub REST API.
// For github.com the public API (api.github.com) is used.
// For any other host (GitHub Enterprise Server) the /api/v3 endpoint is used.
func postGitHub(ctx context.Context, baseURL, host, repoFullName, commitSHA, token string, status Status) error {
	var apiURL string

	if bareHost(host) == "github.com" {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/statuses/%s", repoFullName, commitSHA)
	} else {
		// GitHub Enterprise Server: https://{hostname}/api/v3/repos/{owner}/{repo}/statuses/{sha}
		apiURL = fmt.Sprintf("%s/api/v3/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)
	}

	type githubRequest struct {
		State       string `json:"state"`
		Description string `json:"description"`
		Context     string `json:"context"`
		TargetURL   string `json:"target_url,omitempty"`
	}

	body := githubRequest{
		State:       string(status.State),
		Description: status.Description,
		Context:     status.Context,
		TargetURL:   status.TargetURL,
	}

	return doPost(ctx, apiURL, token, body)
}

// postGitHubCompatible posts a commit status using the GitHub-compatible API.
// This covers Gitea, Forgejo, and Gogs which share the same /api/v1 endpoint shape.
func postGitHubCompatible(ctx context.Context, baseURL, _ /* host */, repoFullName, commitSHA, token string, status Status) error {
	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/statuses/%s", baseURL, repoFullName, commitSHA)

	type githubRequest struct {
		State       string `json:"state"`
		Description string `json:"description"`
		Context     string `json:"context"`
		TargetURL   string `json:"target_url,omitempty"`
	}

	body := githubRequest{
		State:       string(status.State),
		Description: status.Description,
		Context:     status.Context,
		TargetURL:   status.TargetURL,
	}

	return doPost(ctx, apiURL, token, body)
}

// postGitLab posts a commit status using the GitLab API.
func postGitLab(ctx context.Context, baseURL, repoFullName, commitSHA, token string, status Status) error {
	// GitLab requires the namespace/path to be URL-encoded with %2F separating path components.
	encodedPath := strings.ReplaceAll(repoFullName, "/", "%2F")
	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/statuses/%s", baseURL, encodedPath, commitSHA)

	// Map our states to GitLab pipeline states.
	gitlabState := string(status.State)

	switch status.State {
	case StateFailure, StateError:
		gitlabState = "failed"
	case StatePending:
		gitlabState = "running"
	}

	type gitlabRequest struct {
		State       string `json:"state"`
		Name        string `json:"name"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url,omitempty"`
	}

	body := gitlabRequest{
		State:       gitlabState,
		Name:        status.Context,
		Description: status.Description,
		TargetURL:   status.TargetURL,
	}

	return doPost(ctx, apiURL, token, body)
}

func doPost(ctx context.Context, apiURL, token string, body any) error {
	jsonData, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewBuffer(jsonData)) // #nosec G107
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to post commit status: %w", err)
	}

	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("commit status API returned %s for %s", resp.Status, apiURL)
	}

	return nil
}
