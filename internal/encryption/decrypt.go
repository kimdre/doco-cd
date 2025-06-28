package encryption

import (
	"io"
	"os"
	"strings"

	"github.com/getsops/sops/v3/decrypt"
)

// DecryptSopsFile decrypts a SOPS-encrypted file at the given path and returns its contents as a byte slice.
func DecryptSopsFile(path, format string) ([]byte, error) {
	return decrypt.File(path, format)
}

// IsSopsEncryptedFile checks if the file at the given path is a SOPS-encrypted file.
func IsSopsEncryptedFile(path string) (bool, error) {
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
	return strings.Contains(content, "sops") || strings.Contains(content, "ENC["), nil
}
