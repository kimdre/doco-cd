package commitstatus

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

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

	if isAzureDevOps(host) {
		return ProviderAzureDevOps
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

// isAzureDevOps returns true for Azure DevOps SaaS hosts.
func isAzureDevOps(host string) bool {
	h := strings.ToLower(bareHost(host))

	return h == "dev.azure.com" || h == "ssh.dev.azure.com" || strings.HasSuffix(h, ".visualstudio.com")
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
