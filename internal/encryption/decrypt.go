package encryption

import (
	"io"
	"os"
	"strings"

	"github.com/getsops/sops/v3/cmd/sops/formats"

	"github.com/getsops/sops/v3/decrypt"
)

// IgnoreDirs is a list of paths that should be ignored when checking for SOPS-encrypted files.
var IgnoreDirs = []string{
	".git",
	".github",
	".vscode",
	".idea",
	"node_modules",
}

// DecryptFile decrypts a SOPS-encrypted file at the given path and returns its contents as a byte slice.
func DecryptFile(path string) ([]byte, error) {
	format := "binary" // default
	if formats.IsYAMLFile(path) {
		format = "yaml"
	} else if formats.IsJSONFile(path) {
		format = "json"
	} else if formats.IsEnvFile(path) {
		format = "dotenv"
	} else if formats.IsIniFile(path) {
		format = "ini"
	}

	return decrypt.File(path, format)
}

// IsEncryptedFile checks if the file at the given path is a SOPS-encrypted file.
func IsEncryptedFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck

	buf := make([]byte, 4096)

	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return false, err
	}

	content := string(buf[:n])
	// Check for SOPS-specific markers
	return strings.Contains(content, "sops") && strings.Contains(content, "ENC["), nil
}
