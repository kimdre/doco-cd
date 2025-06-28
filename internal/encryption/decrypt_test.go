package encryption

import (
	"errors"
	"testing"

	"github.com/getsops/sops/v3"
)

func TestIsSopsEncryptedFile(t *testing.T) {
	files := []struct {
		path     string
		expected bool
	}{
		{"testdata/encrypted.yaml", true},
		{"testdata/encrypted.json", true},
		{"testdata/encrypted.env", true},
		{"testdata/unencrypted.yaml", false},
		{"testdata/unencrypted.json", false},
		{"testdata/unencrypted.env", false},
		{"testdata/empty.yaml", false},
	}

	SetupAgeKeyEnvVar(t)

	for _, file := range files {
		t.Run(file.path, func(t *testing.T) {
			isEncrypted, err := IsEncryptedFile(file.path)
			if err != nil {
				t.Fatalf("Error checking if file is encrypted: %v", err)
			}

			if isEncrypted != file.expected {
				t.Errorf("Expected %v for %s, got %v", file.expected, file.path, isEncrypted)
			}
		})
	}
}

func TestDecryptSopsFile(t *testing.T) {
	files := []struct {
		path     string
		expected string
		error    error
	}{
		{"testdata/encrypted.yaml", "this.is.encrypted: \"yes\"\n", nil},
		{"testdata/encrypted.env", "THIS_IS_ENCRYPTED=yes\n", nil},
		{"testdata/unencrypted.yaml", "this.is.encrypted: \"yes\"\n", sops.MetadataNotFound},
		{"testdata/unencrypted.env", "THIS_IS_ENCRYPTED=yes\n", errors.New("parsing time \"\" as \"2006-01-02T15:04:05Z07:00\": cannot parse \"\" as \"2006\"")},
		{"testdata/empty.yaml", "", sops.MetadataNotFound},
	}

	SetupAgeKeyEnvVar(t)

	for _, file := range files {
		t.Run(file.path, func(t *testing.T) {
			decryptedContent, err := DecryptFile(file.path)
			if err != nil {
				if file.error == nil {
					t.Fatalf("Unexpected error decrypting file %s: %v", file.path, err)
				}

				if err.Error() != file.error.Error() {
					t.Errorf("Expected error %v for %s, got %v", file.error, file.path, err)
				}

				return
			}

			if string(decryptedContent) != file.expected {
				t.Errorf("Expected %s for %s, got %s", file.expected, file.path, decryptedContent)
			}
		})
	}
}
