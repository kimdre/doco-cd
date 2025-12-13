package stages

import (
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
)

// getRepoName extracts the repository name from the clone URL.
func getRepoName(cloneURL config.HttpUrl) string {
	repoName := strings.SplitAfter(string(cloneURL), "://")[1]

	if strings.Contains(repoName, "@") {
		repoName = strings.SplitAfter(repoName, "@")[1]
	}

	return strings.TrimSuffix(repoName, ".git")
}

// getFullName extracts the full repository name without the domain part from the clone URL.
// E.g., "github.com/kimdre/doco-cd" becomes "kimdre/doco-cd"
// or "git.example.com/doco-cd" becomes "doco-cd".
func getFullName(cloneURL config.HttpUrl) string {
	repoName := getRepoName(cloneURL)
	parts := strings.Split(repoName, "/")
	fullName := repoName

	if len(parts) > 2 {
		fullName = strings.Join(parts[1:], "/")
	} else if len(parts) == 2 {
		fullName = parts[1]
	}

	return fullName
}
