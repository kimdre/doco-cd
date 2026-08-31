package git

import (
	"net/url"
	"path"
	"strings"
)

// ConvertSSHUrl converts SSH URLs to the ssh:// format.
// e.g. convert git@github.com:user/repo.git to ssh://git@github.com/user/repo.git
func ConvertSSHUrl(url string) string {
	// Check if url starts with git@ and convert to ssh:// format
	if strings.HasPrefix(url, "git@") {
		// Replace the first ':' with '/' after the host
		if idx := strings.Index(url, ":"); idx != -1 {
			url = url[:idx] + "/" + url[idx+1:]
		}

		url = "ssh://" + url
	}

	return url
}

// GetRepoName returns the repository name in the form "<host>/<owner>/<repo>" from the given clone URL.
// Supports:
//   - https://github.com/owner/repo(.git)
//   - http://github.com/owner/repo(.git)
//   - ssh://github.com/owner/repo(.git)
//   - git@github.com:owner/repo(.git)
//   - token-injected https like https://oauth2:TOKEN@github.com/owner/repo(.git)
func GetRepoName(cloneURL string) string {
	u := strings.TrimSpace(cloneURL)
	if u == "" {
		return ""
	}

	// Handle classic SCP-like SSH: git@host:owner/repo(.git)
	if strings.Contains(u, "@") && strings.Contains(u, ":") && !strings.Contains(u, "://") {
		parts := strings.SplitN(u, "@", 2)
		if len(parts) == 2 {
			hostAndPath := parts[1]

			hostParts := strings.SplitN(hostAndPath, ":", 2)
			if len(hostParts) == 2 {
				host := hostParts[0]
				repoPath := strings.TrimPrefix(hostParts[1], "/")
				ownerRepo := normalizeOwnerRepo(repoPath)

				return host + "/" + ownerRepo
			}
		}
	}

	// Local filesystem repositories: use the absolute path (minus leading slash) as the
	// name, mirroring the "host/owner/repo" hierarchy used for remote URLs so that two
	// different local paths never collide.
	if strings.HasPrefix(u, "file://") {
		parsed, err := url.Parse(u)
		if err == nil {
			p := strings.TrimPrefix(parsed.Path, "/")

			return normalizeOwnerRepo(p)
		}
	}

	// For URLs with a scheme use net/url
	parsed, err := url.Parse(u)
	if err == nil && parsed.Host != "" {
		p := strings.TrimPrefix(parsed.Path, "/")
		ownerRepo := normalizeOwnerRepo(p)

		return parsed.Host + "/" + ownerRepo
	}

	// Fallback: attempt to normalize directly
	return normalizeOwnerRepo(u)
}

// normalizeOwnerRepo cleans a path and returns "owner/repo" or empty string when not possible.
func normalizeOwnerRepo(p string) string {
	// Remove query or fragment if present in raw strings
	if idx := strings.IndexAny(p, "?#"); idx >= 0 {
		p = p[:idx]
	}

	// Trim trailing '.git'
	p = strings.TrimSuffix(p, ".git")

	// Clean path
	return path.Clean(p)
}
