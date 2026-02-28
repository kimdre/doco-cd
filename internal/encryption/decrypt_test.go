package encryption

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getsops/sops/v3"

	"github.com/kimdre/doco-cd/internal/filesystem"
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
			t.Parallel()

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
		{"testdata/unencrypted.env", "THIS_IS_ENCRYPTED=yes\n", sops.MetadataNotFound},
		{"testdata/empty.yaml", "", sops.MetadataNotFound},
	}

	SetupAgeKeyEnvVar(t)

	for _, file := range files {
		t.Run(file.path, func(t *testing.T) {
			t.Parallel()

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
	// Ensure SOPS key is not set for both subtests.
	// This makes behavior deterministic regardless of other tests.
	testData, err := os.ReadFile("testdata/encrypted.yaml")
	if err != nil {
		t.Fatalf("Failed to read %s: %v", testData, err)
	}

	t.Setenv("SOPS_AGE_KEY", "")
	t.Setenv("SOPS_AGE_KEY_FILE", "")

	t.Run("ignores files matched by .gitignore", func(t *testing.T) {
		t.Parallel()

		repoDir := t.TempDir()

		// .gitignore that ignores a folder and an extension
		gitignoreContent := "ignored_dir/\n*.ignored\n"
		if err = os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(gitignoreContent), filesystem.PermOwner); err != nil {
			t.Fatalf("Failed to create .gitignore: %v", err)
		}

		// Create ignored directory + file inside
		ignoredDir := filepath.Join(repoDir, "ignored_dir")
		if err = os.MkdirAll(ignoredDir, filesystem.PermDir); err != nil {
			t.Fatalf("Failed to create ignored_dir: %v", err)
		}

		if err = os.WriteFile(filepath.Join(ignoredDir, "secret.yaml"), testData, filesystem.PermOwner); err != nil {
			t.Fatalf("Failed to write ignored secret: %v", err)
		}

		// Create a root-level file matching *.ignored
		if err = os.WriteFile(filepath.Join(repoDir, "file.ignored"), testData, filesystem.PermOwner); err != nil {
			t.Fatalf("Failed to write file.ignored: %v", err)
		}

		decryptedFiles, err := DecryptFilesInDirectory(repoDir, repoDir)
		if err != nil {
			t.Fatalf("DecryptFilesInDirectory failed (likely attempted to decrypt ignored file): %v", err)
		}

		if len(decryptedFiles) != 0 {
			t.Errorf("Expected 0 decrypted files, got %d: %v", len(decryptedFiles), decryptedFiles)
		}
	})
}
