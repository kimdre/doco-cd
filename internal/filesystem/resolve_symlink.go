package filesystem

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// ResolveSymlinkWithin returns the path that path resolves to if path itself is a symlink, like
// the Docker daemon does with the source of a bind mount. It fails if the symlink cannot be
// resolved or points outside of root. Other paths, including missing ones, are returned
// unchanged, since symlinks in their parent directories are followed anyway.
func ResolveSymlinkWithin(root, path string) (string, error) {
	info, missing, err := lstatIfExists(path)
	if err != nil {
		return "", err
	}

	if missing || info.Mode().Type() != fs.ModeSymlink {
		return path, nil
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve symlink %s: %w", path, err)
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", root, err)
	}

	rel, err := filepath.Rel(realRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("symlink %s points outside of %s", path, root)
	}

	return resolved, nil
}
