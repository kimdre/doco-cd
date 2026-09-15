package deploy

import (
	"path/filepath"
	"testing"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// TestLoadExternalSecretsFiles_SingleFile verifies that entries from a local
// external secrets file are loaded into Config.Internal.ExternalSecretsFromFiles.
func TestLoadExternalSecretsFiles_SingleFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	secretsFile := filepath.Join(tmpDir, "secrets.doco-cd.yaml")

	if err := createTestFile(t, secretsFile, "DB_PASSWORD: abc123\nLABEL_SECRET: def456\n"); err != nil {
		t.Fatalf("failed to create secrets file: %v", err)
	}

	cfg := &Config{
		ExternalSecretsFiles: []string{"secrets.doco-cd.yaml"},
	}

	if err := LoadExternalSecretsFiles(cfg, tmpDir); err != nil {
		t.Fatalf("LoadExternalSecretsFiles() returned an error: %v", err)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["DB_PASSWORD"].LegacyRef, "abc123"; got != want {
		t.Errorf("DB_PASSWORD = %q, want %q", got, want)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["LABEL_SECRET"].LegacyRef, "def456"; got != want {
		t.Errorf("LABEL_SECRET = %q, want %q", got, want)
	}

	if len(cfg.ExternalSecretsFiles) != 0 {
		t.Errorf("ExternalSecretsFiles = %v, want empty after processing local files", cfg.ExternalSecretsFiles)
	}
}

// TestLoadExternalSecretsFiles_RemoteFilesPreserved ensures files prefixed
// with "remote:" are left untouched (without the prefix) for later processing
// and are not read from the local filesystem.
func TestLoadExternalSecretsFiles_RemoteFilesPreserved(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cfg := &Config{
		ExternalSecretsFiles: []string{"remote:secrets.doco-cd.yaml"},
	}

	if err := LoadExternalSecretsFiles(cfg, tmpDir); err != nil {
		t.Fatalf("LoadExternalSecretsFiles() returned an error: %v", err)
	}

	if len(cfg.ExternalSecretsFiles) != 1 || cfg.ExternalSecretsFiles[0] != "secrets.doco-cd.yaml" {
		t.Errorf("ExternalSecretsFiles = %v, want [\"secrets.doco-cd.yaml\"]", cfg.ExternalSecretsFiles)
	}

	if len(cfg.Internal.ExternalSecretsFromFiles) != 0 {
		t.Errorf("Internal.ExternalSecretsFromFiles = %v, want empty", cfg.Internal.ExternalSecretsFromFiles)
	}
}

// TestLoadExternalSecretsFiles_MissingFile verifies that a missing file (no
// special-casing, unlike the default .env file) results in an error.
func TestLoadExternalSecretsFiles_MissingFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cfg := &Config{
		ExternalSecretsFiles: []string{"missing.yaml"},
	}

	if err := LoadExternalSecretsFiles(cfg, tmpDir); err == nil {
		t.Fatal("LoadExternalSecretsFiles() expected an error for a missing file, got nil")
	}
}

// TestMergeExternalSecretsFromFiles_InlineWins verifies that inline
// ExternalSecrets entries take precedence over file-sourced ones on key
// collision, while non-colliding keys from both sources are preserved.
func TestMergeExternalSecretsFromFiles_InlineWins(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
			"DB_PASSWORD": {LegacyRef: "inline-value"},
		},
	}
	cfg.Internal.ExternalSecretsFromFiles = map[string]secrettypes.ExternalSecretRef{
		"DB_PASSWORD":  {LegacyRef: "file-value"},
		"LABEL_SECRET": {LegacyRef: "file-only-value"},
	}

	MergeExternalSecretsFromFiles(cfg)

	if got, want := cfg.ExternalSecrets["DB_PASSWORD"].LegacyRef, "inline-value"; got != want {
		t.Errorf("DB_PASSWORD = %q, want %q (inline should win)", got, want)
	}

	if got, want := cfg.ExternalSecrets["LABEL_SECRET"].LegacyRef, "file-only-value"; got != want {
		t.Errorf("LABEL_SECRET = %q, want %q", got, want)
	}
}

// TestMergeExternalSecretsFromFiles_NoFiles verifies the merge is a no-op
// when no file-sourced secrets were accumulated.
func TestMergeExternalSecretsFromFiles_NoFiles(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
			"DB_PASSWORD": {LegacyRef: "inline-value"},
		},
	}

	MergeExternalSecretsFromFiles(cfg)

	if len(cfg.ExternalSecrets) != 1 || cfg.ExternalSecrets["DB_PASSWORD"].LegacyRef != "inline-value" {
		t.Errorf("ExternalSecrets = %v, want unchanged", cfg.ExternalSecrets)
	}
}
