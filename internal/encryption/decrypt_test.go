package encryption

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/getsops/sops/v3"
)

// SetAgeKeyFileEnvVar sets the SOPS_AGE_KEY_FILE environment variable to the path of the test key file.
func setupAgeKeyFileEnvVar(t *testing.T) {
	t.Helper()

	const envVarName = "SOPS_AGE_KEY_FILE"

	t.Logf("Set %s environment variable for testing", envVarName)

	// Set the environment variable to the test key file
	currentDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current working directory: %v", err)
	}

	err = os.Setenv(envVarName, filepath.Join(currentDir, "testdata/age-key.txt"))
	if err != nil {
		t.Fatalf("Failed to set %s environment variable: %v", envVarName, err)
	}

	t.Cleanup(func() {
		// Clean up the environment variable after the test
		t.Logf("Unset %s environment variable after test", envVarName)

		err = os.Unsetenv(envVarName)
		if err != nil {
			t.Errorf("Failed to unset %s environment variable: %v", envVarName, err)
		}
	})
}

func TestIsSopsEncryptedFile(t *testing.T) {
	files := []struct {
		path     string
		expected bool
	}{
		{"testdata/encrypted.yaml", true},
		{"testdata/encrypted.env", true},
		{"testdata/unencrypted.yaml", false},
		{"testdata/unencrypted.env", false},
		{"testdata/empty.yaml", false},
	}

	setupAgeKeyFileEnvVar(t)

	for _, file := range files {
		t.Run(file.path, func(t *testing.T) {
			isEncrypted, err := IsSopsEncryptedFile(file.path)
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
		format   string
		expected string
		error    error
	}{
		{"testdata/encrypted.yaml", "yaml", "this.is.encrypted: \"yes\"\n", nil},
		{"testdata/encrypted.env", "dotenv", "THIS_IS_ENCRYPTED=yes\n", nil},
		{"testdata/unencrypted.yaml", "yaml", "this.is.encrypted: \"yes\"\n", sops.MetadataNotFound},
		{"testdata/unencrypted.env", "dotenv", "THIS_IS_ENCRYPTED=yes\n", errors.New("parsing time \"\" as \"2006-01-02T15:04:05Z07:00\": cannot parse \"\" as \"2006\"")},
		{"testdata/empty.yaml", "yaml", "", sops.MetadataNotFound},
	}

	setupAgeKeyFileEnvVar(t)

	for _, file := range files {
		t.Run(file.path, func(t *testing.T) {
			decryptedContent, err := DecryptSopsFile(file.path, file.format)
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
