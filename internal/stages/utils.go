package stages

import (
	"net/url"
	"path"
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
)

// normalizeOwnerRepo cleans a path and returns "owner/repo" or empty string when not possible.
func normalizeOwnerRepo(p string) string {
	// Remove query or fragment if present in raw strings
	if idx := strings.IndexAny(p, "?#"); idx >= 0 {
		p = p[:idx]
	}

	// Trim trailing '.git'
	p = strings.TrimSuffix(p, ".git")

	// Clean path and split
	clean := path.Clean(p)

	parts := strings.Split(clean, "/")
	if len(parts) < 2 {
		// Not enough segments to form owner/repo
		return clean // safest fallback; avoids panic
	}

	owner := parts[len(parts)-2]
	repo := parts[len(parts)-1]

	return owner + "/" + repo
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

// getFullName extracts the full repository name without the domain part from the clone URL.
// E.g., "github.com/kimdre/doco-cd" becomes "kimdre/doco-cd"
// or "git.example.com/doco-cd" becomes "doco-cd".
func getFullName(cloneURL config.HttpUrl) string {
	repoName := GetRepoName(string(cloneURL))
	parts := strings.Split(repoName, "/")
	fullName := repoName

	if len(parts) > 2 {
		fullName = strings.Join(parts[1:], "/")
	} else if len(parts) == 2 {
		fullName = parts[1]
	}

	return fullName
}
