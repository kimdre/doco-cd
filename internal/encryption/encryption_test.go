package encryption

import (
	"errors"
	"path/filepath"
	"testing"
)

var (
	encryptedFile   = "encrypted.yaml"
	unencryptedFile = "unencrypted.yaml"
	nonexistentFile = "nonexistent.yaml"
	expectedData    = "key: foo\nnumber: 1234\nlist:\n    - hello\n    - world\nboolean: true\n"
)

func TestIsEncrypted(t *testing.T) {
	tests := []struct {
		file          string
		expected      bool
		expectedError error
	}{
		{file: encryptedFile, expected: true, expectedError: nil},
		{file: unencryptedFile, expected: false, expectedError: nil},
		{file: nonexistentFile, expected: false, expectedError: ErrFileReadFailed},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			filePath := filepath.Join("testdata", tt.file)
			encrypted, err := IsEncrypted(filePath)
			if err != nil && !errors.Is(err, tt.expectedError) {
				t.Errorf("expected error %v, got %v", tt.expectedError, err)
			}
			if encrypted != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, encrypted)
			}
		})
	}
}

func TestDecryptFile(t *testing.T) {
	tests := []struct {
		file          string
		expected      string
		expectedError error
	}{
		{file: encryptedFile, expected: expectedData, expectedError: nil},
		{file: unencryptedFile, expected: "", expectedError: ErrFileDecryptFailed},
		{file: nonexistentFile, expected: "", expectedError: ErrFileReadFailed},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			filePath := filepath.Join("testdata", tt.file)
			decryptedData, err := DecryptFile(filePath)
			if err != nil && !errors.Is(err, tt.expectedError) {
				t.Errorf("expected error %v, got %v", tt.expectedError, err)
			}

			if string(decryptedData) != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, string(decryptedData))
			}
		})
	}
}
