package commitstatus

import (
	"context"
	"log/slog"
	"strings"

	"github.com/kimdre/doco-cd/internal/git"
)

// maxDescriptionLength is the maximum number of Unicode runes a commit status
// description may contain. Longer descriptions are truncated with a trailing
// ellipsis so they fit the limits enforced by SCM provider APIs.
const maxDescriptionLength = 140

// FailureDescription normalizes err into a single-line, Unicode-safe
// description suitable for a commit status. Internal whitespace (including
// newlines) is collapsed to single spaces, and the result is truncated to at
// most maxDescriptionLength runes. A nil error returns "Failed".
func FailureDescription(err error) string {
	if err == nil {
		return "Failed"
	}

	description := strings.Join(strings.Fields(err.Error()), " ")
	if len([]rune(description)) <= maxDescriptionLength {
		return description
	}

	truncated := []rune(description)

	return string(truncated[:maxDescriptionLength-3]) + "..."
}

// RequestParams holds the raw inputs needed to resolve a commit status Request.
// Callers (e.g. the source and stages packages) supply the relevant subset of
// their own application configuration and per-deployment context; RequestParams
// intentionally avoids depending on any higher-level config type to keep this
// package free of import cycles.
type RequestParams struct {
	// Enabled mirrors the application-level "post commit statuses" toggle
	// (e.g. app.Config.GitCommitStatus). When false, ResolveRequest always skips.
	Enabled bool
	// SourceIsGit must be true for commit statuses to be posted/read; OCI (or
	// any other non-Git) sources are always skipped.
	SourceIsGit bool
	// SourceURL is the repository URL used both for credential resolution and,
	// when PayloadWebURL/PayloadFullName are empty, as the repository identity.
	SourceURL string
	// CommitSHA is the resolved commit the status applies to. Empty skips.
	CommitSHA string
	// PayloadWebURL optionally overrides SourceURL as the repository URL (e.g.
	// the browsable web URL from a webhook payload).
	PayloadWebURL string
	// PayloadFullName optionally overrides the "owner/repo" full name derived
	// from the repository URL (e.g. from a webhook payload).
	PayloadFullName string
	// ProviderOverride is the raw configured SCM provider (e.g. app.Config.GitScmProvider).
	// Empty/"auto" auto-detects the provider from the repository URL.
	ProviderOverride string
	// APIBaseURL optionally overrides the inferred SCM API base URL (e.g. app.Config.GitScmApiUrl).
	APIBaseURL string
	// AccessToken is the configured fallback access token (e.g. app.Config.GitAccessToken),
	// used when no scoped/GitHub App credential is resolved for SourceURL.
	AccessToken string
	// ContextName is the commit status context label (e.g. "doco-cd/demo").
	// Empty defaults to DeployContext.
	ContextName string
}

// Request is a fully-resolved commit status request, ready to Post or Get.
type Request struct {
	Provider     Provider
	APIBaseURL   string
	RepoURL      string
	RepoFullName string
	CommitSHA    string
	Token        string
	Context      string
}

// ResolveRequest resolves a Request from params, applying credential
// precedence (a resolved scoped/GitHub App token takes priority over
// params.AccessToken), repository URL/full name fallback, and provider/API
// URL/context defaults. logger receives Debug/Warn diagnostics for skip
// reasons and token-resolution failures.
//
// ok is false when the caller should skip posting/getting a commit status:
// commit statuses are disabled, the source is not Git, no commit SHA is
// available, or no credentials are configured.
func ResolveRequest(logger *slog.Logger, params RequestParams) (Request, bool) {
	if !params.Enabled || !params.SourceIsGit {
		return Request{}, false
	}

	commitSHA := strings.TrimSpace(params.CommitSHA)
	if commitSHA == "" {
		logger.Debug("skipping commit status: no commit SHA available")

		return Request{}, false
	}

	resolved := git.ResolveAuthConfig(params.SourceURL, "", "", "")

	token, err := git.ResolveHTTPToken(params.SourceURL, resolved)
	if err != nil {
		logger.Warn("failed to resolve commit status token", slog.String("error", err.Error()))
	}

	if token == "" {
		token = params.AccessToken
	}

	if token == "" {
		logger.Debug("skipping commit status: no access token configured")

		return Request{}, false
	}

	repoURL := strings.TrimSpace(params.PayloadWebURL)
	if repoURL == "" {
		repoURL = params.SourceURL
	}

	repoFullName := strings.TrimSpace(params.PayloadFullName)
	if repoFullName == "" {
		repoFullName = git.GetFullName(repoURL)
	}

	provider, _ := ParseProvider(params.ProviderOverride)

	contextName := strings.TrimSpace(params.ContextName)
	if contextName == "" {
		contextName = DeployContext
	}

	return Request{
		Provider:     provider,
		APIBaseURL:   params.APIBaseURL,
		RepoURL:      repoURL,
		RepoFullName: repoFullName,
		CommitSHA:    commitSHA,
		Token:        token,
		Context:      contextName,
	}, true
}

// Post posts status using the resolved request's provider/repository/credentials.
// status.Context is overridden with the request's resolved Context.
func (r Request) Post(ctx context.Context, status Status) error {
	status.Context = r.Context

	return Post(ctx, r.Provider, r.APIBaseURL, r.RepoURL, r.RepoFullName, r.CommitSHA, r.Token, status)
}

// Get returns the latest commit status for the resolved request's context.
func (r Request) Get(ctx context.Context) (Status, bool, error) {
	return Get(ctx, r.Provider, r.APIBaseURL, r.RepoURL, r.RepoFullName, r.CommitSHA, r.Token, r.Context)
}
