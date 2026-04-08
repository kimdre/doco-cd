package filesystem

import (
	"errors"
	"fmt"
	"io/fs"
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

	if !InBasePath(trustedRoot, absPath) {
		return absPath, fmt.Errorf("%w: %s is outside of trusted root %s", ErrPathTraversal, absPath, trustedRoot)
	}

	absPath = strings.ReplaceAll(absPath, ":", "_")

	return absPath, nil
}

// InBasePath checks if the given path is within the base path,
// preventing path traversal attacks.
func InBasePath(basePath, path string) bool {
	basePath = filepath.Clean(basePath)
	path = filepath.Clean(path)

	rel, err := filepath.Rel(basePath, path)
	if err != nil {
		return false
	}

	return !strings.HasPrefix(rel, "..")
}

// IsSocket checks if the given path is a socket file.
func IsSocket(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.Mode().Type() == fs.ModeSocket
}

// IsDir checks if the given path is a directory.
func IsDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.IsDir()
}

// IsSymlink checks if the given path is a symbolic link.
func IsSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}

	return info.Mode().Type() == fs.ModeSymlink
}

// IsFile checks if the given path is a regular file without any mode bits set (like symlink, socket, named pipe, etc.).
func IsFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}

	return info.Mode().IsRegular()
}
