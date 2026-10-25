package git

import (
	"fmt"
	"net"
	"strings"
)

// ValidateDomainPattern checks one GIT_AUTH_DOMAINS domain pattern.
// A valid pattern is an exact hostname, an IP address, or a wildcard such as *.example.com.
// URLs, paths, ports, user info and a bare "*" are rejected, because they never match a host.
func ValidateDomainPattern(pattern string) error {
	domain := normalizeHost(pattern)
	if domain == "" {
		return fmt.Errorf("%w: domain must not be empty", ErrInvalidAuthDomain)
	}

	if net.ParseIP(domain) != nil {
		return nil
	}

	if suffix, ok := wildcardSuffix(domain); ok {
		domain = suffix
	}

	labels := strings.SplitSeq(domain, ".")
	for label := range labels {
		if !isHostnameLabel(label) {
			return fmt.Errorf("%w: %q must be a hostname or a wildcard such as *.example.com", ErrInvalidAuthDomain, pattern)
		}
	}

	return nil
}

// HasHTTPCredential reports whether a GIT_AUTH_DOMAINS entry defines its own HTTP(S) credential.
// Entries without one inherit the global HTTP(S) credential for their domains.
func (s ScopedAuthConfig) HasHTTPCredential() bool {
	if strings.TrimSpace(s.GitAccessToken) != "" {
		return true
	}

	return strings.TrimSpace(s.GitHubAppID) != "" && strings.TrimSpace(s.GitHubAppPrivateKey) != ""
}

func isHostnameLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}

	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}

	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}

	return true
}
