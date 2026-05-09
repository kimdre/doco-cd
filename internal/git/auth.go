package git

import (
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/kimdre/doco-cd/internal/git/ssh"
)

// ScopedAuthConfig maps credentials to one or more Git host/domain patterns.
// Patterns support exact hosts (e.g. github.com) and wildcard subdomains (e.g. *.example.com).
type ScopedAuthConfig struct {
	Domains                 []string `yaml:"domains"`
	GitAccessToken          string   `yaml:"git_access_token"`
	SSHPrivateKey           string   `yaml:"ssh_private_key"`
	SSHPrivateKeyPassphrase string   `yaml:"ssh_private_key_passphrase"`
}

type authResolver struct {
	scoped              []ScopedAuthConfig
	globalPrivateKey    string
	globalKeyPassphrase string
	globalToken         string
}

var (
	authResolverMu     sync.RWMutex
	configuredResolver = authResolver{}
)

// ConfigureAuthResolver configures domain-scoped and global Git credentials.
// This should be called when application config is loaded or updated.
func ConfigureAuthResolver(scoped []ScopedAuthConfig, globalPrivateKey, globalKeyPassphrase, globalToken string) {
	authResolverMu.Lock()
	defer authResolverMu.Unlock()

	configuredResolver = authResolver{
		scoped:              append([]ScopedAuthConfig(nil), scoped...),
		globalPrivateKey:    globalPrivateKey,
		globalKeyPassphrase: globalKeyPassphrase,
		globalToken:         globalToken,
	}
}

// ResolveScopedCredentials resolves credentials for a repository URL using exact domain matches,
// then the most specific wildcard suffix, and finally global fallback credentials.
func ResolveScopedCredentials(url, privateKey, keyPassphrase, token string) (string, string, string) {
	authResolverMu.RLock()

	resolver := configuredResolver

	authResolverMu.RUnlock()

	host := parseGitHost(url)
	if host == "" {
		return privateKey, keyPassphrase, token
	}

	resolvedGlobalPrivateKey := strings.TrimSpace(resolver.globalPrivateKey)
	resolvedGlobalPassphrase := resolver.globalKeyPassphrase
	resolvedGlobalToken := strings.TrimSpace(resolver.globalToken)

	if resolvedGlobalPrivateKey != "" && strings.TrimSpace(privateKey) == "" {
		privateKey = resolvedGlobalPrivateKey
		keyPassphrase = resolvedGlobalPassphrase
	}

	if resolvedGlobalToken != "" && strings.TrimSpace(token) == "" {
		token = resolvedGlobalToken
	}

	if len(resolver.scoped) == 0 {
		return privateKey, keyPassphrase, token
	}

	// Exact domain matches always win.
	for _, entry := range resolver.scoped {
		for _, domain := range entry.Domains {
			if normalizeHost(domain) == host {
				return pickCredentials(entry, resolver, privateKey, keyPassphrase, token)
			}
		}
	}

	// Then choose the wildcard with the longest suffix (most specific).
	bestIdx := -1
	bestSuffixLen := -1

	for i, entry := range resolver.scoped {
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

	if bestIdx >= 0 {
		return pickCredentials(resolver.scoped[bestIdx], resolver, privateKey, keyPassphrase, token)
	}

	return privateKey, keyPassphrase, token
}

func pickCredentials(entry ScopedAuthConfig, resolver authResolver, privateKey, keyPassphrase, token string) (string, string, string) {
	resolvedPrivateKey := strings.TrimSpace(entry.SSHPrivateKey)
	resolvedPassphrase := entry.SSHPrivateKeyPassphrase
	resolvedToken := strings.TrimSpace(entry.GitAccessToken)

	if resolvedPrivateKey == "" {
		resolvedPrivateKey = strings.TrimSpace(resolver.globalPrivateKey)
		resolvedPassphrase = resolver.globalKeyPassphrase
	}

	if resolvedToken == "" {
		resolvedToken = strings.TrimSpace(resolver.globalToken)
	}

	if resolvedPrivateKey == "" {
		resolvedPrivateKey = privateKey
		resolvedPassphrase = keyPassphrase
	}

	if resolvedToken == "" {
		resolvedToken = token
	}

	return resolvedPrivateKey, resolvedPassphrase, resolvedToken
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

// GetAuthMethod determines the appropriate authentication method based on the URL and provided credentials.
func GetAuthMethod(url, privateKey, keyPassphrase, token string) (transport.AuthMethod, error) {
	privateKey, keyPassphrase, token = ResolveScopedCredentials(url, privateKey, keyPassphrase, token)

	if IsSSH(url) {
		return SSHAuth(privateKey, keyPassphrase)
	} else if token != "" {
		return HttpTokenAuth(token), nil
	}

	return nil, nil
}

// IsSSH checks if a given URL is an SSH URL.
func IsSSH(url string) bool {
	return strings.HasPrefix(url, "git@") || strings.HasPrefix(url, "ssh://")
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

	// auth.HostKeyCallback = ssh2.InsecureIgnoreHostKey()

	return auth, nil
}

// HttpTokenAuth returns an AuthMethod for HTTP Basic Auth using a token.
func HttpTokenAuth(token string) transport.AuthMethod {
	if token == "" {
		return nil
	}

	return &githttp.BasicAuth{
		Username: "oauth2", // can be anything except an empty string
		Password: token,
	}
}
