package encryption

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/getsops/sops/v3/decrypt"
)

var (
	ErrFileDecryptFailed = errors.New("failed to decrypt file")
	ErrFileReadFailed    = errors.New("failed to read file")
)

// IsEncrypted checks if a file is encrypted using SOPS.
func IsEncrypted(filePath string) (bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrFileReadFailed, err)
	}

	// Check if the file contains the string "sops"
	return strings.Contains(string(data), "sops:"), nil
}

// DecryptFile decrypts a SOPS-encrypted file and returns its contents.
func DecryptFile(filePath string) ([]byte, error) {
	// Read the encrypted file
	encryptedData, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFileReadFailed, err)
	}

	// Decrypt the file using the SOPS package
	decryptedData, err := decrypt.Data(encryptedData, "yaml")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFileDecryptFailed, err)
	}

	return decryptedData, nil
}
