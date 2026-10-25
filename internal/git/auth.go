package git

import (
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/kimdre/doco-cd/internal/git/githubapp"
	"github.com/kimdre/doco-cd/internal/git/ssh"
)

// ScopedAuthConfig maps credentials to one or more Git host/domain patterns.
// Patterns support exact hosts (e.g. github.com) and wildcard subdomains (e.g. *.example.com).
type ScopedAuthConfig struct {
	Domains                 []string `yaml:"domains"`
	GitAccessToken          string   `yaml:"git_access_token"`
	GitAccessTokenUser      string   `yaml:"git_access_token_user"` // GitAccessTokenUser is the username paired with GitAccessToken (e.g. a GitLab deploy token username). Empty defaults to DefaultHTTPAuthUser.
	SSHPrivateKey           string   `yaml:"ssh_private_key"`
	SSHPrivateKeyPassphrase string   `yaml:"ssh_private_key_passphrase"`
	GitHubAppID             string   `yaml:"github_app_id"`
	GitHubAppPrivateKey     string   `yaml:"github_app_private_key"`
	GitHubAppInstallationID int64    `yaml:"github_app_installation_id"`
}

// GitHubAppConfig contains credentials used to mint short-lived GitHub App installation tokens.
type GitHubAppConfig struct {
	ID             string
	PrivateKey     string
	InstallationID int64
}

// ResolvedAuthConfig contains the final credentials selected for a given repository URL.
// It holds at most one HTTP(S) credential: an access token or a GitHub App.
// The HTTP(S) credential is bound to the host it was resolved for.
type ResolvedAuthConfig struct {
	SSHPrivateKey           string
	SSHPrivateKeyPassphrase string
	GitAccessToken          string
	HTTPAuthUser            string // HTTPAuthUser is the Basic Auth username for token auth; empty defaults to DefaultHTTPAuthUser.
	GitHubApp               GitHubAppConfig

	// httpHost is the normalized host that the HTTP(S) credential belongs to.
	httpHost string
}

// HTTPAuthMethod identifies the HTTP(S) credential selected for a repository host.
type HTTPAuthMethod int

const (
	// HTTPAuthNone means that no HTTP(S) credential is scoped to the host.
	HTTPAuthNone HTTPAuthMethod = iota
	// HTTPAuthToken means that an access token is used.
	HTTPAuthToken
	// HTTPAuthGitHubApp means that a GitHub App installation token is minted.
	HTTPAuthGitHubApp
)

// HTTPMethod reports which HTTP(S) credential was selected.
func (r ResolvedAuthConfig) HTTPMethod() HTTPAuthMethod {
	switch {
	case r.GitAccessToken != "":
		return HTTPAuthToken
	case r.GitHubApp.complete():
		return HTTPAuthGitHubApp
	default:
		return HTTPAuthNone
	}
}

func (c GitHubAppConfig) complete() bool {
	return c.ID != "" && c.PrivateKey != ""
}

type authResolver struct {
	scoped              []ScopedAuthConfig
	globalPrivateKey    string
	globalKeyPassphrase string
	globalToken         string
	globalHTTPAuthUser  string
	globalGitHubApp     GitHubAppConfig
	allowAnyHost        bool
}

var (
	authResolverMu     sync.RWMutex
	configuredResolver = authResolver{}
)

// AuthResolverOption changes optional behavior of the Git credential resolver.
type AuthResolverOption func(*authResolver)

// WithAllowAnyHost sends the global HTTP(S) credential also to hosts without a matching GIT_AUTH_DOMAINS entry.
// This is insecure: a URL from a webhook, an API call, or a repository can then receive the global credential.
func WithAllowAnyHost(allow bool) AuthResolverOption {
	return func(r *authResolver) {
		r.allowAnyHost = allow
	}
}

// ConfigureAuthResolver configures domain-scoped and global Git credentials.
// This should be called when application config is loaded or updated.
func ConfigureAuthResolver(scoped []ScopedAuthConfig,
	globalPrivateKey, globalKeyPassphrase, globalToken, globalHTTPAuthUser string,
	globalGitHubApp GitHubAppConfig,
	opts ...AuthResolverOption,
) {
	authResolverMu.Lock()
	defer authResolverMu.Unlock()

	configuredResolver = authResolver{
		scoped:              append([]ScopedAuthConfig(nil), scoped...),
		globalPrivateKey:    globalPrivateKey,
		globalKeyPassphrase: globalKeyPassphrase,
		globalToken:         globalToken,
		globalHTTPAuthUser:  strings.TrimSpace(globalHTTPAuthUser),
		globalGitHubApp: GitHubAppConfig{
			ID:             strings.TrimSpace(globalGitHubApp.ID),
			PrivateKey:     strings.TrimSpace(globalGitHubApp.PrivateKey),
			InstallationID: globalGitHubApp.InstallationID,
		},
	}

	for _, opt := range opts {
		opt(&configuredResolver)
	}
}

var githubAppTokenProvider = resolveGitHubAppInstallationToken

func resolveGitHubAppInstallationToken(repoURL string, cfg GitHubAppConfig) (string, error) {
	return githubapp.ResolveInstallationToken(repoURL, githubapp.Config{
		ID:             cfg.ID,
		PrivateKey:     cfg.PrivateKey,
		InstallationID: cfg.InstallationID,
	})
}

// SwapGitHubAppTokenProviderForTest replaces the GitHub App token provider and returns a restore function.
func SwapGitHubAppTokenProviderForTest(provider func(string, GitHubAppConfig) (string, error)) func() {
	authResolverMu.Lock()
	old := githubAppTokenProvider
	githubAppTokenProvider = provider
	authResolverMu.Unlock()

	return func() {
		authResolverMu.Lock()
		githubAppTokenProvider = old
		authResolverMu.Unlock()
	}
}

// ResolveAuthConfig resolves credentials for a repository URL.
//
// SSH keys come from the matching GIT_AUTH_DOMAINS entry, then the global key, then privateKey.
// Without a matching entry, privateKey wins over the global key.
//
// HTTP(S) credentials are only selected when the host matches a GIT_AUTH_DOMAINS entry.
// Exact domains win over wildcards, and the longest wildcard suffix wins over shorter ones.
// For the matching entry the order is: entry token, entry GitHub App, global token, global GitHub App.
// Hosts without a matching entry get no HTTP(S) credential, so a token cannot reach an unknown host.
// WithAllowAnyHost disables this protection: hosts without a matching entry then get the global credential.
// GitHub App credentials are never used for gist URLs, because installation tokens cannot access gists.
func ResolveAuthConfig(url, privateKey, keyPassphrase string) ResolvedAuthConfig {
	authResolverMu.RLock()

	resolver := configuredResolver

	authResolverMu.RUnlock()

	host := parseGitHost(url)

	resolved := ResolvedAuthConfig{
		SSHPrivateKey:           privateKey,
		SSHPrivateKeyPassphrase: keyPassphrase,
	}

	if host == "" {
		return resolved
	}

	if strings.TrimSpace(privateKey) == "" && strings.TrimSpace(resolver.globalPrivateKey) != "" {
		resolved.SSHPrivateKey = strings.TrimSpace(resolver.globalPrivateKey)
		resolved.SSHPrivateKeyPassphrase = resolver.globalKeyPassphrase
	}

	entry, ok := resolver.match(host)
	if !ok {
		if resolver.allowAnyHost {
			// An empty entry has no own credential, so only the global credential can be selected.
			resolver.selectHTTPCredential(ScopedAuthConfig{}, isGistURL(url), &resolved)

			if resolved.HTTPMethod() != HTTPAuthNone {
				resolved.httpHost = host
			}
		}

		return resolved
	}

	// A matched entry uses its own SSH key, then the global key.
	switch {
	case strings.TrimSpace(entry.SSHPrivateKey) != "":
		resolved.SSHPrivateKey = strings.TrimSpace(entry.SSHPrivateKey)
		resolved.SSHPrivateKeyPassphrase = entry.SSHPrivateKeyPassphrase
	case strings.TrimSpace(resolver.globalPrivateKey) != "":
		resolved.SSHPrivateKey = strings.TrimSpace(resolver.globalPrivateKey)
		resolved.SSHPrivateKeyPassphrase = resolver.globalKeyPassphrase
	}

	resolver.selectHTTPCredential(entry, isGistURL(url), &resolved)

	if resolved.HTTPMethod() != HTTPAuthNone {
		resolved.httpHost = host
	}

	return resolved
}

// match returns the GIT_AUTH_DOMAINS entry for a normalized host.
// An exact domain match wins. Otherwise the wildcard with the longest suffix wins.
func (r authResolver) match(host string) (ScopedAuthConfig, bool) {
	for _, entry := range r.scoped {
		for _, domain := range entry.Domains {
			if normalizeHost(domain) == host {
				return entry, true
			}
		}
	}

	bestIdx := -1
	bestSuffixLen := -1

	for i, entry := range r.scoped {
		for _, domain := range entry.Domains {
			suffix, ok := wildcardSuffix(domain)
			if !ok {
				continue
			}

			if wildcardMatches(host, suffix) && len(suffix) > bestSuffixLen {
				bestIdx = i
				bestSuffixLen = len(suffix)
			}
		}
	}

	if bestIdx < 0 {
		return ScopedAuthConfig{}, false
	}

	return r.scoped[bestIdx], true
}

// selectHTTPCredential sets exactly one HTTP(S) credential for a matching entry.
// The entry's own credential wins over the inherited global credential.
// GitHub Apps are skipped for gists, so a gist falls back to a token or anonymous access.
func (r authResolver) selectHTTPCredential(entry ScopedAuthConfig, gist bool, resolved *ResolvedAuthConfig) {
	user := strings.TrimSpace(entry.GitAccessTokenUser)
	if user == "" {
		user = r.globalHTTPAuthUser
	}

	entryApp := GitHubAppConfig{
		ID:             strings.TrimSpace(entry.GitHubAppID),
		PrivateKey:     strings.TrimSpace(entry.GitHubAppPrivateKey),
		InstallationID: entry.GitHubAppInstallationID,
	}

	switch {
	case strings.TrimSpace(entry.GitAccessToken) != "":
		resolved.GitAccessToken = strings.TrimSpace(entry.GitAccessToken)
		resolved.HTTPAuthUser = user
	case entryApp.complete() && !gist:
		resolved.GitHubApp = entryApp
	case strings.TrimSpace(r.globalToken) != "":
		resolved.GitAccessToken = strings.TrimSpace(r.globalToken)
		resolved.HTTPAuthUser = user
	case r.globalGitHubApp.complete() && !gist:
		resolved.GitHubApp = r.globalGitHubApp
	}
}

// isGistURL reports whether an HTTP(S) URL points to a GitHub gist, served from a
// gist.* host or, on GitHub Enterprise Server without subdomain isolation, under /gist/.
func isGistURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return false
	}

	if strings.HasPrefix(normalizeHost(parsed.Hostname()), "gist.") {
		return true
	}

	firstSegment, _, _ := strings.Cut(strings.TrimPrefix(parsed.Path, "/"), "/")

	return strings.EqualFold(firstSegment, "gist")
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))

	return strings.TrimSuffix(host, ".")
}

func parseGitHost(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}

	if strings.Contains(u, "@") && strings.Contains(u, ":") && !strings.Contains(u, "://") {
		parts := strings.SplitN(u, "@", 2)
		if len(parts) != 2 {
			return ""
		}

		hostPath := strings.SplitN(parts[1], ":", 2)
		if len(hostPath) != 2 {
			return ""
		}

		return normalizeHost(hostPath[0])
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}

	if parsed.Host == "" {
		return ""
	}

	return normalizeHost(parsed.Hostname())
}

func wildcardSuffix(domain string) (string, bool) {
	d := normalizeHost(domain)

	after, ok := strings.CutPrefix(d, "*.")
	if !ok || after == "" {
		return "", false
	}

	return after, true
}

func wildcardMatches(host, suffix string) bool {
	// Wildcards for subdomains must not match the apex domain.
	if host == suffix {
		return false
	}

	return strings.HasSuffix(host, "."+suffix)
}

// GetAuthMethod determines the authentication method for a repository URL.
// HTTP(S) credentials come only from the configured resolver, see ResolveAuthConfig.
func GetAuthMethod(url, privateKey, keyPassphrase string) (transport.AuthMethod, error) {
	// Local and native Git transports cannot use HTTP or SSH credentials.
	if IsLocalFile(url) || IsNativeGit(url) {
		return nil, nil
	}

	resolved := ResolveAuthConfig(url, privateKey, keyPassphrase)

	if IsSSH(url) {
		return SSHAuth(resolved.SSHPrivateKey, resolved.SSHPrivateKeyPassphrase)
	}

	// Fail instead of cloning anonymously when an HTTP(S) URL has no usable host.
	if isHTTPURL(url) && parseGitHost(url) == "" {
		return nil, ErrInvalidRepositoryHost
	}

	httpToken, err := ResolveHTTPToken(url, resolved)
	if err != nil {
		return nil, err
	}

	if httpToken == "" {
		return nil, nil
	}

	return HttpTokenAuth(resolved.HTTPAuthUser, httpToken), nil
}

// ResolveHTTPToken returns the access token for HTTP(S) requests to a repository URL.
// This covers Git transport and SCM API calls such as commit status posting.
// It uses resolved.GitAccessToken, or mints a short-lived GitHub App installation token.
// It returns an empty string and no error when no HTTP(S) credential was selected.
// The URL host must be the host that ResolveAuthConfig resolved the credential for.
// This stops a credential for one host from being sent to another host.
func ResolveHTTPToken(url string, resolved ResolvedAuthConfig) (string, error) {
	method := resolved.HTTPMethod()
	if method == HTTPAuthNone {
		return "", nil
	}

	if host := parseGitHost(url); host == "" || resolved.httpHost == "" || host != resolved.httpHost {
		return "", ErrCredentialHostMismatch
	}

	if method == HTTPAuthToken {
		return resolved.GitAccessToken, nil
	}

	installationToken, err := githubAppTokenProvider(url, resolved.GitHubApp)
	if err != nil {
		return "", fmt.Errorf("failed to resolve GitHub App installation token: %w", err)
	}

	return installationToken, nil
}

// isHTTPURL reports whether a URL uses the http or https scheme.
func isHTTPURL(rawURL string) bool {
	scheme, _, ok := strings.Cut(strings.TrimSpace(rawURL), "://")

	return ok && (strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https"))
}

// IsSSH checks if a given URL is an SSH URL.
func IsSSH(url string) bool {
	return strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
}

// IsLocalFile checks if a given URL points to a local filesystem Git repository (file:// scheme).
func IsLocalFile(url string) bool {
	return strings.HasPrefix(url, "file://")
}

// nativeGitScheme is the URL prefix of the native Git protocol.
const nativeGitScheme = "git://"

// IsNativeGit checks if a given URL uses the native Git protocol (git:// scheme).
// The scheme check ignores case because URL parsers and go-git accept schemes such as GIT://.
func IsNativeGit(url string) bool {
	return len(url) >= len(nativeGitScheme) && strings.EqualFold(url[:len(nativeGitScheme)], nativeGitScheme)
}

// SSHAuth creates an SSH authentication method using the provided private key.
func SSHAuth(privateKey, keyPassphrase string) (transport.AuthMethod, error) {
	if strings.TrimSpace(privateKey) == "" {
		return nil, ErrSSHKeyRequired
	}

	auth, err := gitssh.NewPublicKeys(ssh.DefaultGitSSHUser, []byte(privateKey), keyPassphrase)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH public keys: %w", err)
	}

	return auth, nil
}

// DefaultHTTPAuthUser is the Basic Auth username used when none is configured.
// Most providers accept any non-empty username for token auth, but GitLab
// deploy tokens require their specific username (e.g. gitlab+deploy-token-123),
// which callers supply via GIT_ACCESS_TOKEN_USER / GIT_AUTH_DOMAINS git_access_token_user.
const DefaultHTTPAuthUser = "oauth2"

// HttpTokenAuth returns an AuthMethod for HTTP Basic Auth using a token.
// An empty username falls back to DefaultHTTPAuthUser.
func HttpTokenAuth(username, token string) transport.AuthMethod {
	if token == "" {
		return nil
	}

	if strings.TrimSpace(username) == "" {
		username = DefaultHTTPAuthUser
	}

	return &githttp.BasicAuth{
		Username: username,
		Password: token,
	}
}
