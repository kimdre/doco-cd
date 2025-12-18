package encryption

import (
	"errors"
	"os"
	"path/filepath"
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

func TestDecryptFilesInDirectory_GitIgnore(t *testing.T) {
	// Create a temporary directory for the test repository
	repoDir := t.TempDir()

	// Create a .gitignore file
	gitignoreContent := "ignored_dir/\n*.ignored\n"
	err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(gitignoreContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create .gitignore: %v", err)
	}

	// Create directories
	ignoredDir := filepath.Join(repoDir, "ignored_dir")
	err = os.MkdirAll(ignoredDir, 0755)
	if err != nil {
		t.Fatalf("Failed to create ignored_dir: %v", err)
	}

	// Content that triggers IsEncryptedFile check
	dummyEncryptedContent := []byte("sops ENC[")

	// 1. Create a file in the ignored directory.
	// If the ignore logic fails, DecryptFilesInDirectory will attempt to decrypt this.
	// Since we haven't set up any SOPS keys (SetupAgeKeyEnvVar is not called),
	// it will return an error: "SOPS secret key is not set...".
	err = os.WriteFile(filepath.Join(ignoredDir, "secret.yaml"), dummyEncryptedContent, 0644)
	if err != nil {
		t.Fatalf("Failed to write ignored secret: %v", err)
	}

	// 2. Create a file matching the ignore pattern in the root.
	err = os.WriteFile(filepath.Join(repoDir, "file.ignored"), dummyEncryptedContent, 0644)
	if err != nil {
		t.Fatalf("Failed to write file.ignored: %v", err)
	}

	// Run DecryptFilesInDirectory
	decryptedFiles, err := DecryptFilesInDirectory(repoDir, repoDir)
	if err != nil {
		t.Fatalf("DecryptFilesInDirectory failed (likely attempted to decrypt ignored file): %v", err)
	}

	// Ensure no files were returned (since they should have been ignored)
	if len(decryptedFiles) != 0 {
		t.Errorf("Expected 0 decrypted files, got %d: %v", len(decryptedFiles), decryptedFiles)
	}
}
