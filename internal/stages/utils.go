package stages

import (
	"strings"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/git"
)

// getFullName extracts the full repository name without the domain part from the clone URL.
// E.g., "github.com/kimdre/doco-cd" becomes "kimdre/doco-cd"
// or "git.example.com/doco-cd" becomes "doco-cd".
func getFullName(cloneURL config.HttpUrl) string {
	repoName := git.GetRepoName(string(cloneURL))
	parts := strings.Split(repoName, "/")
	fullName := repoName

	if len(parts) > 2 {
		fullName = strings.Join(parts[1:], "/")
	} else if len(parts) == 2 {
		fullName = parts[1]
	}

	return fullName
}
