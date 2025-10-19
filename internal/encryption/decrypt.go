package encryption

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getsops/sops/v3/cmd/sops/formats"
	"github.com/getsops/sops/v3/decrypt"

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
	format := GetFileFormat(path)

	return decrypt.File(path, format)
}

func DecryptContent(content []byte, format string) ([]byte, error) {
	return decrypt.Data(content, format)
}

// DecryptFilesInDirectory walks through the specified directory and decrypts all SOPS-encrypted files.
func DecryptFilesInDirectory(dirPath string) ([]string, error) {
	var decryptedFiles []string

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("failed to walk directory %s: %w", path, err)
		}

		dirPath = filepath.Dir(path)
		dirName := filepath.Base(dirPath)

		// Check if dirPath is part of the paths to ignore
		if slices.Contains(IgnoreDirs, dirName) {
			return filepath.SkipDir
		}

		if d.IsDir() {
			return nil
		}

		isEncrypted, err := IsEncryptedFile(path)
		if err != nil {
			return fmt.Errorf("failed to check if file is encrypted: %w", err)
		}

		if isEncrypted {
			if !SopsKeyIsSet() {
				return fmt.Errorf("SOPS secret key is not set, cannot decrypt file: %s", path)
			}

			decryptedContent, err := DecryptFile(path)
			if err != nil {
				return fmt.Errorf("failed to decrypt file %s: %w", path, err)
			}

			err = os.WriteFile(path, decryptedContent, filesystem.PermOwner)
			if err != nil {
				return fmt.Errorf("failed to write decrypted content to file %s: %w", path, err)
			}

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
