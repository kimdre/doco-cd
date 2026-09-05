package encryption

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getsops/sops/v3"
	"github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/cmd/sops/formats"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestIsSopsEncryptedFile(t *testing.T) {
	files := []struct {
		path      string
		encrypted bool
	}{
		{"testdata/encrypted.yaml", true},
		{"testdata/encrypted.json", true},
		{"testdata/encrypted.env", true},
		{"testdata/encrypted", true},
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

			if isEncrypted != file.encrypted {
				t.Errorf("Expected %v for %s, got %v", file.encrypted, file.path, isEncrypted)
			}
		})
	}
}

func TestDetectFormat(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		content   string
		format    formats.Format
		encrypted bool
	}{
		{
			name:      "SOPS encrypted YAML",
			content:   readTestFile(t, "testdata/encrypted.yaml"),
			format:    formats.Yaml,
			encrypted: true,
		},
		{
			name:      "SOPS encrypted JSON",
			content:   readTestFile(t, "testdata/encrypted.json"),
			format:    formats.Json,
			encrypted: true,
		},
		{
			name:      "SOPS encrypted dotenv",
			content:   readTestFile(t, "testdata/encrypted.env"),
			format:    formats.Dotenv,
			encrypted: true,
		},
		{
			name:      "SOPS encrypted binary",
			content:   readTestFile(t, "testdata/encrypted"),
			format:    formats.Binary,
			encrypted: true,
		},
		{
			name:      "extension takes precedence over content",
			path:      "secrets.env",
			content:   readTestFile(t, "testdata/encrypted.yaml"),
			format:    formats.Dotenv,
			encrypted: false,
		},
		{
			name:      "plaintext containing legacy markers",
			content:   "description: sops-compatible value ENC[not-encrypted]\n",
			format:    formats.Binary,
			encrypted: false,
		},
		{
			name: "invalid SOPS metadata",
			content: `value: ENC[not-encrypted]
sops:
  lastmodified: invalid
  mac: ENC[not-encrypted]
`,
			format:    formats.Binary,
			encrypted: false,
		},
		{
			name: "SOPS-shaped plaintext without encryption metadata",
			content: `value: plaintext
sops:
  lastmodified: "2025-06-28T18:23:51Z"
  mac: plaintext
  version: 3.9.0
`,
			format:    formats.Binary,
			encrypted: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			format, encrypted := DetectFormat([]byte(test.content), test.path)
			if encrypted != test.encrypted {
				t.Errorf("Expected encrypted=%v, got %v", test.encrypted, encrypted)
			}

			if format != test.format {
				t.Errorf("Expected format %v, got %v", test.format, format)
			}
		})
	}
}

func TestDetectFormat_PathExtension(t *testing.T) {
	tests := []struct {
		path   string
		format formats.Format
	}{
		{"config.yaml", formats.Yaml},
		{"config.yml", formats.Yaml},
		{"config.json", formats.Json},
		{"config.env", formats.Dotenv},
		{"config.ini", formats.Ini},
		{"config", formats.Binary},
		{"config.txt", formats.Binary},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			format, encrypted := DetectFormat([]byte("plaintext\n"), test.path)
			if encrypted {
				t.Error("Expected plaintext not to be detected as SOPS-encrypted")
			}

			if format != test.format {
				t.Errorf("Expected format %v, got %v", test.format, format)
			}
		})
	}
}

// TestDecryptSopsFile_UnknownExtension ensures SOPS files are detected and decrypted
// with the format of their content when the extension does not identify one.
func TestDecryptSopsFile_UnknownExtension(t *testing.T) {
	SetupAgeKeyEnvVar(t)

	tests := []struct {
		fixture  string
		expected string
	}{
		{"testdata/encrypted.yaml", "this.is.encrypted: \"yes\"\n"},
		{"testdata/encrypted.env", "THIS_IS_ENCRYPTED=yes\n"},
		{"testdata/encrypted", "binary-secret\n"},
	}

	for _, test := range tests {
		t.Run(test.fixture, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.sops")
			if err := os.WriteFile(path, []byte(readTestFile(t, test.fixture)), filesystem.PermOwner); err != nil {
				t.Fatalf("Failed to write encrypted fixture: %v", err)
			}

			encrypted, err := IsEncryptedFile(path)
			if err != nil {
				t.Fatalf("Failed to check encrypted fixture: %v", err)
			}

			if !encrypted {
				t.Fatal("Expected SOPS-encrypted content to be detected without a known extension")
			}

			decrypted, err := DecryptFile(path)
			if err != nil {
				t.Fatalf("Failed to decrypt fixture: %v", err)
			}

			if string(decrypted) != test.expected {
				t.Errorf("Expected %q, got %q", test.expected, decrypted)
			}
		})
	}
}

func TestIsSopsEncryptedFile_RecognizedExtension(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encrypted.env")
	if err := os.WriteFile(path, []byte(readTestFile(t, "testdata/encrypted.yaml")), filesystem.PermOwner); err != nil {
		t.Fatalf("Failed to write encrypted fixture: %v", err)
	}

	encrypted, err := IsEncryptedFile(path)
	if err != nil {
		t.Fatalf("Failed to check encrypted fixture: %v", err)
	}

	if encrypted {
		t.Error("Expected YAML content with an .env extension not to be detected as SOPS-encrypted dotenv")
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read %s: %v", path, err)
	}

	return string(content)
}

func TestDecryptSopsFile(t *testing.T) {
	files := []struct {
		path     string
		expected string
		error    error
	}{
		{"testdata/encrypted.yaml", "this.is.encrypted: \"yes\"\n", nil},
		{"testdata/encrypted.env", "THIS_IS_ENCRYPTED=yes\n", nil},
		{"testdata/encrypted", "binary-secret\n", nil},
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

	t.Setenv(age.SopsAgeKeyEnv, "")
	t.Setenv(age.SopsAgeKeyFileEnv, "")

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

		// #nosec G703
		if err = os.WriteFile(filepath.Join(ignoredDir, "secret.yaml"), testData, filesystem.PermOwner); err != nil {
			t.Fatalf("Failed to write ignored secret: %v", err)
		}

		// Create a root-level file matching *.ignored
		// #nosec G703
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
