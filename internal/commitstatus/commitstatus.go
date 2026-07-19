package commitstatus

import (
	"context"
	"fmt"
	"strings"
)

// State represents the commit status state sent to the Git provider.
type State string

const (
	StatePending State = "pending"
	StateSuccess State = "success"
	StateFailure State = "failure"
	StateError   State = "error"

	BaseContext   = "doco-cd"
	DeployContext = BaseContext + "/deploy"
)

func ContextForStack(target, stack string) string {
	target = strings.TrimSpace(target)
	stack = strings.TrimSpace(stack)

	if stack == "" {
		return BaseContext
	}

	if target != "" {
		return BaseContext + "/" + target + "/" + stack
	}

	return BaseContext + "/" + stack
}

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
	// ProviderAzureDevOps targets the Azure DevOps Git statuses API.
	ProviderAzureDevOps Provider = "azuredevops"
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
	case ProviderAzureDevOps:
		return ProviderAzureDevOps, nil
	default:
		return ProviderAuto, fmt.Errorf("unknown SCM provider %q: must be one of auto, github, gitlab, gitea, forgejo, azuredevops", s)
	}
}

// Status holds the information to post as a commit status.
type Status struct {
	State       State
	Description string
	// Context is the label shown in the Git UI (e.g. "doco-cd/demo").
	// Defaults to BaseContext when empty.
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
		status.Context = BaseContext
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
	case ProviderAzureDevOps:
		return postAzureDevOps(ctx, baseURL, host, repoURL, repoFullName, commitSHA, token, status)
	default: // ProviderGitea
		return postGitHubCompatible(ctx, baseURL, host, repoFullName, commitSHA, token, status)
	}
}

// Get returns the latest commit status for the requested context.
// When provider is ProviderAuto ("") the provider is detected from repoURL.
// Returns found=false when token, commitSHA, or matching status is missing.
func Get(ctx context.Context, provider Provider, repoURL, repoFullName, commitSHA, token, contextName string) (Status, bool, error) {
	token = strings.TrimSpace(token)
	commitSHA = strings.TrimSpace(commitSHA)
	contextName = strings.TrimSpace(contextName)

	if token == "" || commitSHA == "" {
		return Status{}, false, nil
	}

	if contextName == "" {
		contextName = BaseContext
	}

	host, scheme, err := parseHostAndScheme(repoURL)
	if err != nil {
		return Status{}, false, fmt.Errorf("failed to parse repository URL: %w", err)
	}

	baseURL := scheme + "://" + host
	resolved := resolveProvider(provider, host)

	switch resolved {
	case ProviderGitLab:
		return getGitLab(ctx, baseURL, repoFullName, commitSHA, token, contextName)
	case ProviderGitHub:
		return getGitHub(ctx, baseURL, host, repoFullName, commitSHA, token, contextName)
	case ProviderAzureDevOps:
		return getAzureDevOps(ctx, baseURL, host, repoURL, repoFullName, commitSHA, token, contextName)
	default:
		return getGitHubCompatible(ctx, baseURL, repoFullName, commitSHA, token, contextName)
	}
}
