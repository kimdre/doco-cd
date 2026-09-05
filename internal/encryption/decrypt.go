package encryption

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/cmd/sops/common"
	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/config"
	"github.com/getsops/sops/v3/decrypt"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

var errSopsKeyNotSet = errors.New("SOPS secret key is not set")

// probedFormats lists the formats that are probed for paths without a format-specific extension.
// Binary is probed first because its JSON envelope also parses as JSON and YAML,
// and JSON before YAML because the YAML parser accepts JSON as well.
var probedFormats = []formats.Format{formats.Binary, formats.Json, formats.Dotenv, formats.Ini, formats.Yaml}

// DecryptFile decrypts a SOPS-encrypted file at the given path and returns its contents as a byte slice.
func DecryptFile(path string) ([]byte, error) {
	if !SopsKeyIsSet() {
		return nil, errSopsKeyNotSet
	}

	content, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, err
	}

	format, _ := DetectFormat(content, path)

	return DecryptContent(content, format)
}

// DecryptContent decrypts SOPS-encrypted content using the supplied format.
// The format must be the one returned by DetectFormat for the same content.
func DecryptContent(content []byte, format formats.Format) ([]byte, error) {
	return decrypt.DataWithFormat(content, format)
}

// DecryptFilesInDirectory walks through the specified directory and decrypts all SOPS-encrypted files.
func DecryptFilesInDirectory(repoPath, dirPath string) ([]string, error) {
	return decryptFilesInDirectory(repoPath, dirPath, make(map[string]struct{}))
}

// decryptFilesInDirectory is the recursive implementation of DecryptFilesInDirectory.
// The visited set tracks already-processed real paths to prevent infinite recursion
// caused by symlink loops (e.g. a symlink pointing to an ancestor directory).
func decryptFilesInDirectory(repoPath, dirPath string, visited map[string]struct{}) ([]string, error) {
	if !filesystem.InBasePath(repoPath, dirPath) {
		return nil, fmt.Errorf("%w: %s is outside the repository root %s", filesystem.ErrPathTraversal, dirPath, repoPath)
	}

	// Resolve the real path so symlink loops are detected regardless of the path used to reach them.
	realPath, err := filepath.EvalSymlinks(dirPath)
	if err != nil {
		realPath = filepath.Clean(dirPath)
	}

	if _, ok := visited[realPath]; ok {
		return nil, nil
	}

	visited[realPath] = struct{}{}

	var decryptedFiles []string

	var ignoreMatcher gitignore.Matcher

	if _, err := os.Stat(filepath.Join(repoPath, ".gitignore")); err == nil {
		ps, err := gitignore.ReadPatterns(osfs.New(repoPath), nil)
		if err == nil {
			ignoreMatcher = gitignore.NewMatcher(ps)
		}
	}

	err = filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
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
		if filesystem.IsIgnoredDir(dirName) {
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
			_, err = decryptFilesInDirectory(repoPath, absTarget, visited)
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
	content, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return false, err
	}

	_, encrypted := DetectFormat(content, path)

	return encrypted, nil
}

// DetectFormat returns the SOPS format that parses the content as an encrypted document,
// and whether such a format was found.
// The format is derived from the path extension, falling back to probing every supported
// format when the extension does not identify one.
// Callers must decrypt with the returned format, as detecting and decrypting with different
// formats either fails or emits the content in the wrong format.
func DetectFormat(content []byte, path string) (formats.Format, bool) {
	if format := formats.FormatForPath(path); format != formats.Binary {
		return format, hasSopsMetadata(content, format)
	}

	for _, format := range probedFormats {
		if hasSopsMetadata(content, format) {
			return format, true
		}
	}

	return formats.Binary, false
}

// hasSopsMetadata parses the content with the supplied SOPS format and verifies
// that it contains valid SOPS metadata without attempting decryption.
func hasSopsMetadata(content []byte, format formats.Format) bool {
	store := common.StoreForFormat(format, config.NewStoresConfig())

	tree, err := store.LoadEncryptedFile(content)
	if err != nil || tree.Metadata.MasterKeyCount() == 0 {
		return false
	}

	// SOPS always stores an encrypted MAC, so a plaintext one marks a document that
	// merely mimics the metadata structure. SOPS exposes no predicate for this.
	if !strings.HasPrefix(tree.Metadata.MessageAuthenticationCode, "ENC[") {
		return false
	}

	// A single value store (binary) only emits its "data" key, so let the store reject
	// documents it could load but not emit, such as structured JSON.
	if singleValue, ok := store.(sops.SingleValueStore); ok && singleValue.IsSingleValueStore() {
		if _, err = store.EmitPlainFile(tree.Branches); err != nil {
			return false
		}
	}

	return true
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
