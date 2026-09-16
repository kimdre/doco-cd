package deploy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/encryption"
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

// TestLoadExternalSecretsFiles_NoFiles verifies that LoadExternalSecretsFiles
// is a no-op (no error, no accumulated entries) when Config.ExternalSecretsFiles
// is empty/nil.
func TestLoadExternalSecretsFiles_NoFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cfg := &Config{}

	if err := LoadExternalSecretsFiles(cfg, tmpDir); err != nil {
		t.Fatalf("LoadExternalSecretsFiles() returned an error: %v", err)
	}

	if len(cfg.Internal.ExternalSecretsFromFiles) != 0 {
		t.Errorf("Internal.ExternalSecretsFromFiles = %v, want empty", cfg.Internal.ExternalSecretsFromFiles)
	}

	if len(cfg.ExternalSecretsFiles) != 0 {
		t.Errorf("ExternalSecretsFiles = %v, want empty", cfg.ExternalSecretsFiles)
	}
}

// TestLoadExternalSecretsFiles_MultipleFiles verifies that entries from
// multiple local files accumulate into Internal.ExternalSecretsFromFiles, and
// that on key collision across files, the later file in ExternalSecretsFiles
// wins (matches the underlying maps.Copy loop-order semantics).
func TestLoadExternalSecretsFiles_MultipleFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	firstFile := filepath.Join(tmpDir, "first.yaml")
	secondFile := filepath.Join(tmpDir, "second.yaml")

	if err := createTestFile(t, firstFile, "DB_PASSWORD: first-value\nONLY_IN_FIRST: first-only\n"); err != nil {
		t.Fatalf("failed to create first.yaml: %v", err)
	}

	if err := createTestFile(t, secondFile, "DB_PASSWORD: second-value\nONLY_IN_SECOND: second-only\n"); err != nil {
		t.Fatalf("failed to create second.yaml: %v", err)
	}

	cfg := &Config{
		ExternalSecretsFiles: []string{"first.yaml", "second.yaml"},
	}

	if err := LoadExternalSecretsFiles(cfg, tmpDir); err != nil {
		t.Fatalf("LoadExternalSecretsFiles() returned an error: %v", err)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["DB_PASSWORD"].LegacyRef, "second-value"; got != want {
		t.Errorf("DB_PASSWORD = %q, want %q (later file should win)", got, want)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["ONLY_IN_FIRST"].LegacyRef, "first-only"; got != want {
		t.Errorf("ONLY_IN_FIRST = %q, want %q", got, want)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["ONLY_IN_SECOND"].LegacyRef, "second-only"; got != want {
		t.Errorf("ONLY_IN_SECOND = %q, want %q", got, want)
	}
}

// TestLoadExternalSecretsFiles_EncryptedFile verifies SOPS-encrypted external
// secrets files are decrypted and parsed correctly.
func TestLoadExternalSecretsFiles_EncryptedFile(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "secrets.doco-cd.yaml")

	src, err := os.ReadFile("testdata/encrypted_external_secrets.yaml")
	if err != nil {
		t.Fatalf("failed to read encrypted test fixture: %v", err)
	}

	if err := createTestFile(t, dst, string(src)); err != nil {
		t.Fatalf("failed to copy encrypted test fixture: %v", err)
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
}

// TestLoadExternalSecretsFiles_InvalidYAML verifies that malformed YAML
// content in an external secrets file results in a parse error.
func TestLoadExternalSecretsFiles_InvalidYAML(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	secretsFile := filepath.Join(tmpDir, "secrets.doco-cd.yaml")

	if err := createTestFile(t, secretsFile, "DB_PASSWORD: [unterminated\n"); err != nil {
		t.Fatalf("failed to create secrets file: %v", err)
	}

	cfg := &Config{
		ExternalSecretsFiles: []string{"secrets.doco-cd.yaml"},
	}

	if err := LoadExternalSecretsFiles(cfg, tmpDir); err == nil {
		t.Fatal("LoadExternalSecretsFiles() expected a parse error for invalid YAML, got nil")
	}
}

// TestLoadExternalSecretsFiles_StructuredRef verifies that a file using the
// structured secret reference form (store_ref/remote_ref, webhook-style)
// parses correctly, not just the legacy scalar form.
func TestLoadExternalSecretsFiles_StructuredRef(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	secretsFile := filepath.Join(tmpDir, "secrets.doco-cd.yaml")

	content := "MY_SECRET:\n  store_ref: bitwarden-login\n  remote_ref:\n    key: my-item-id\n    property: password\n"
	if err := createTestFile(t, secretsFile, content); err != nil {
		t.Fatalf("failed to create secrets file: %v", err)
	}

	cfg := &Config{
		ExternalSecretsFiles: []string{"secrets.doco-cd.yaml"},
	}

	if err := LoadExternalSecretsFiles(cfg, tmpDir); err != nil {
		t.Fatalf("LoadExternalSecretsFiles() returned an error: %v", err)
	}

	ref, ok := cfg.Internal.ExternalSecretsFromFiles["MY_SECRET"]
	if !ok {
		t.Fatal("expected MY_SECRET to be present in Internal.ExternalSecretsFromFiles")
	}

	if got, want := ref.StoreRef, "bitwarden-login"; got != want {
		t.Errorf("StoreRef = %q, want %q", got, want)
	}

	if got, want := ref.RemoteRef["key"], "my-item-id"; got != want {
		t.Errorf("RemoteRef[key] = %v, want %q", got, want)
	}

	if got, want := ref.RemoteRef["property"], "password"; got != want {
		t.Errorf("RemoteRef[property] = %v, want %q", got, want)
	}
}

// TestLoadExternalSecretsFiles_LocalThenRemoteRoundTrip simulates the real
// two-phase flow used by all call sites: LoadExternalSecretsFiles is first
// called against a local base path (a local file loads immediately, a
// remote:-prefixed file is deferred without the prefix), and then called
// again against a second base path simulating the checked-out remote
// repository (the deferred file now loads and accumulates alongside the
// entries from the first call).
func TestLoadExternalSecretsFiles_LocalThenRemoteRoundTrip(t *testing.T) {
	t.Parallel()

	localDir := t.TempDir()
	remoteDir := t.TempDir()

	localFile := filepath.Join(localDir, "local.yaml")
	if err := createTestFile(t, localFile, "LOCAL_SECRET: local-value\n"); err != nil {
		t.Fatalf("failed to create local.yaml: %v", err)
	}

	remoteFile := filepath.Join(remoteDir, "remote.yaml")
	if err := createTestFile(t, remoteFile, "REMOTE_SECRET: remote-value\n"); err != nil {
		t.Fatalf("failed to create remote.yaml: %v", err)
	}

	cfg := &Config{
		ExternalSecretsFiles: []string{"local.yaml", "remote:remote.yaml"},
	}

	if err := LoadExternalSecretsFiles(cfg, localDir); err != nil {
		t.Fatalf("LoadExternalSecretsFiles() (local pass) returned an error: %v", err)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["LOCAL_SECRET"].LegacyRef, "local-value"; got != want {
		t.Errorf("after local pass: LOCAL_SECRET = %q, want %q", got, want)
	}

	if _, ok := cfg.Internal.ExternalSecretsFromFiles["REMOTE_SECRET"]; ok {
		t.Error("after local pass: REMOTE_SECRET should not be loaded yet")
	}

	if len(cfg.ExternalSecretsFiles) != 1 || cfg.ExternalSecretsFiles[0] != "remote.yaml" {
		t.Fatalf("after local pass: ExternalSecretsFiles = %v, want [\"remote.yaml\"]", cfg.ExternalSecretsFiles)
	}

	if err := LoadExternalSecretsFiles(cfg, remoteDir); err != nil {
		t.Fatalf("LoadExternalSecretsFiles() (remote pass) returned an error: %v", err)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["LOCAL_SECRET"].LegacyRef, "local-value"; got != want {
		t.Errorf("after remote pass: LOCAL_SECRET = %q, want %q (should still be present)", got, want)
	}

	if got, want := cfg.Internal.ExternalSecretsFromFiles["REMOTE_SECRET"].LegacyRef, "remote-value"; got != want {
		t.Errorf("after remote pass: REMOTE_SECRET = %q, want %q", got, want)
	}

	if len(cfg.ExternalSecretsFiles) != 0 {
		t.Errorf("after remote pass: ExternalSecretsFiles = %v, want empty", cfg.ExternalSecretsFiles)
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
