package encryption

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/decrypt"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

// IgnoreDirs is a list of directories that will be ignored when checking for SOPS-encrypted files.
// These directories typically contain configuration or metadata files that are not relevant for encryption.
var IgnoreDirs = []string{
	".git",
	".github",
	".vscode",
	".idea",
	"node_modules",
}

var ErrSopsKeyNotSet = errors.New("SOPS secret key is not set")

func GetFileFormat(path string) string {
	var format string

	switch {
	case formats.IsYAMLFile(path):
		format = "yaml"
	case formats.IsJSONFile(path):
		format = "json"
	case formats.IsEnvFile(path):
		format = "dotenv"
	case formats.IsIniFile(path):
		format = "ini"
	default:
		format = "binary"
	}

	return format
}

// DecryptFile decrypts a SOPS-encrypted file at the given path and returns its contents as a byte slice.
func DecryptFile(path string) ([]byte, error) {
	if !SopsKeyIsSet() {
		return nil, ErrSopsKeyNotSet
	}

	format := GetFileFormat(path)

	return decrypt.File(path, format)
}

func DecryptContent(content []byte, format string) ([]byte, error) {
	return decrypt.Data(content, format)
}

// DecryptFilesInDirectory walks through the specified directory and decrypts all SOPS-encrypted files.
func DecryptFilesInDirectory(repoPath, dirPath string) ([]string, error) {
	if !filesystem.InBasePath(repoPath, dirPath) {
		return nil, fmt.Errorf("%w: %s is outside the repository root %s", filesystem.ErrPathTraversal, dirPath, repoPath)
	}

	var decryptedFiles []string

	var ignoreMatcher gitignore.Matcher

	if _, err := os.Stat(filepath.Join(repoPath, ".gitignore")); err == nil {
		ps, err := gitignore.ReadPatterns(osfs.New(repoPath), nil)
		if err == nil {
			ignoreMatcher = gitignore.NewMatcher(ps)
		}
	}

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("failed to walk directory %s: %w", path, err)
		}

		if ignoreMatcher != nil {
			relPath, err := filepath.Rel(repoPath, path)
			if err == nil {
				pathComponents := strings.Split(relPath, string(filepath.Separator))
				if ignoreMatcher.Match(pathComponents, d.IsDir()) {
					if d.IsDir() {
						return filepath.SkipDir
					}

					return nil
				}
			}
		}

		dirName := filepath.Base(filepath.Dir(path))

		// Check if dirName is part of the paths to ignore
		if slices.Contains(IgnoreDirs, dirName) {
			return filepath.SkipDir
		}

		// Follow symlinks
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("failed to read symlink %s: %w", path, err)
			}

			absTarget := target
			if !filepath.IsAbs(target) {
				absTarget = filepath.Join(filepath.Dir(path), target)
			}

			// Recursively walk the symlink target
			_, err = DecryptFilesInDirectory(repoPath, absTarget)
			if errors.Is(err, filesystem.ErrPathTraversal) {
				return nil
			}

			return err
		}

		if d.IsDir() {
			return nil
		}

		decrypted, err := DecryptFileInPlace(path)
		if err != nil {
			return fmt.Errorf("failed to decrypt file %s: %w", path, err)
		}

		if decrypted {
			decryptedFiles = append(decryptedFiles, path)
		}

		return nil
	})

	return decryptedFiles, err
}

// IsEncryptedFile checks if the file at the given path is a SOPS-encrypted file.
func IsEncryptedFile(path string) (bool, error) {
	bytes, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return false, err
	}

	return IsEncryptedContent(string(bytes)), nil
}

// IsEncryptedContent checks the given content for SOPS-specific markers to determine if it is a SOPS-encrypted file.
func IsEncryptedContent(content string) bool {
	return strings.Contains(content, "sops") && strings.Contains(content, "ENC[")
}

// DecryptFileInPlace decrypts a SOPS-encrypted file at the given path and overwrites it with the decrypted content.
// If the file is encrypted and successfully decrypted, it returns true. If the file is not encrypted, it returns false without modifying the file.
// The repoPath parameter is used to ensure that the file being decrypted is within the trusted repository root, preventing potential security issues with symlinks or path traversal.
func DecryptFileInPlace(path string) (bool, error) {
	path = filepath.Clean(path)

	if !filepath.IsAbs(path) {
		return false, fmt.Errorf("%w: path must be absolute: %s", filesystem.ErrInvalidFilePath, path)
	}

	// Skip if the path is not a regular file (like socket, named pipe, etc.)
	if !filesystem.IsFile(path) {
		return false, nil
	}

	lock := acquireFileLock(path)
	defer releaseFileLock(path, lock)

	isEncrypted, err := IsEncryptedFile(path)
	if err != nil {
		return false, fmt.Errorf("failed to check if file is encrypted: %w", err)
	}

	if !isEncrypted {
		return false, nil
	}

	decryptedContent, err := DecryptFile(path)
	if err != nil {
		return false, fmt.Errorf("failed to decrypt file %s: %w", path, err)
	}

	err = os.WriteFile(path, decryptedContent, filesystem.PermOwner)
	if err != nil {
		return false, fmt.Errorf("failed to write file %s: %w", path, err)
	}

	return true, nil
}
