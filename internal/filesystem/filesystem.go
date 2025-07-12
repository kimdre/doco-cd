package filesystem

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidFilePath = errors.New("invalid file path")
	ErrPathTraversal   = errors.New("path traversal detected")
)

const (
	PermDir      = 0o755 // Directory permission
	PermOwner    = 0o600 // Owner permission
	PermGroup    = 0o640 // Group permission
	PermPublic   = 0o644 // Public permission
	PermReadOnly = 0o444 // Read-only permission
)

// VerifyAndSanitizePath checks if a file path is valid and sanitizes it.
func VerifyAndSanitizePath(path, trustedRoot string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidFilePath, err)
	}

	trustedRoot = filepath.Clean(trustedRoot) + string(os.PathSeparator)

	if !strings.HasPrefix(absPath, trustedRoot) {
		return absPath, ErrPathTraversal
	}

	return absPath, nil
}
