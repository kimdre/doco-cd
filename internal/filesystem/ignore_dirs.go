package filesystem

import "github.com/kimdre/doco-cd/internal/utils/set"

// IgnoreDirs contains directory names that should be skipped during repository walks
// for internal maintenance tasks (for example decrypt scans and auto-discovery scans).
var IgnoreDirs = set.New(
	".git",
	".github",
	".vscode",
	".idea",
	"node_modules",
)

// IsIgnoredDir returns true when the directory name is in IgnoreDirs.
func IsIgnoredDir(name string) bool {
	return IgnoreDirs.Contains(name)
}
