package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLocalDotEnv_CascadingSelfReference reproduces the scenario from
// https://github.com/kimdre/doco-cd/issues/1722: a broader-scope .env file
// defines the real value for a variable, while more specific (deeper) .env
// files re-declare the same variable as a self-referencing placeholder
// (VAR=${VAR}) intended to simply "pass through" whatever value was already
// resolved. The placeholder must resolve using the already-accumulated
// environment, not to an empty string.
func TestLoadLocalDotEnv_CascadingSelfReference(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	rootEnv := filepath.Join(tmpDir, ".env")
	subDir := filepath.Join(tmpDir, "tests")
	subEnv := filepath.Join(subDir, ".env")
	subSubDir := filepath.Join(subDir, "sched")
	subSubEnv := filepath.Join(subSubDir, ".env")

	if err := os.MkdirAll(subSubDir, 0o750); err != nil {
		t.Fatalf("failed to create test directories: %v", err)
	}

	if err := createTestFile(t, rootEnv, "TEST_PATH=/tmp/doco-cd-scheduler-test\n"); err != nil {
		t.Fatalf("failed to create root .env: %v", err)
	}

	if err := createTestFile(t, subEnv, "TEST_PATH=${TEST_PATH}\n"); err != nil {
		t.Fatalf("failed to create tests/.env: %v", err)
	}

	if err := createTestFile(t, subSubEnv, "TEST_PATH=${TEST_PATH}\n"); err != nil {
		t.Fatalf("failed to create tests/sched/.env: %v", err)
	}

	cfg := &Config{
		WorkingDirectory: "tests/sched",
		EnvFiles:         []string{"../../.env", "../.env", ".env"},
	}

	basePath := filepath.Join(tmpDir, cfg.WorkingDirectory)

	if err := LoadLocalDotEnv(cfg, basePath); err != nil {
		t.Fatalf("LoadLocalDotEnv() returned an error: %v", err)
	}

	got := cfg.Internal.Environment["TEST_PATH"]
	want := "/tmp/doco-cd-scheduler-test"

	if got != want {
		t.Errorf("TEST_PATH = %q, want %q", got, want)
	}
}

// TestLoadLocalDotEnv_SingleFile verifies straightforward single-file loading
// still works as expected after switching parsers.
func TestLoadLocalDotEnv_SingleFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	envFile := filepath.Join(tmpDir, ".env")

	if err := createTestFile(t, envFile, "FOO=bar\nBAZ=qux\n"); err != nil {
		t.Fatalf("failed to create .env: %v", err)
	}

	cfg := &Config{
		EnvFiles: []string{".env"},
	}

	if err := LoadLocalDotEnv(cfg, tmpDir); err != nil {
		t.Fatalf("LoadLocalDotEnv() returned an error: %v", err)
	}

	if cfg.Internal.Environment["FOO"] != "bar" {
		t.Errorf("FOO = %q, want %q", cfg.Internal.Environment["FOO"], "bar")
	}

	if cfg.Internal.Environment["BAZ"] != "qux" {
		t.Errorf("BAZ = %q, want %q", cfg.Internal.Environment["BAZ"], "qux")
	}
}

// TestLoadLocalDotEnv_RemoteFilesPreserved ensures files prefixed with
// "remote:" are left untouched in EnvFiles (without the prefix) for later
// processing and are not read from the local filesystem.
func TestLoadLocalDotEnv_RemoteFilesPreserved(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	cfg := &Config{
		EnvFiles: []string{"remote:.env"},
	}

	if err := LoadLocalDotEnv(cfg, tmpDir); err != nil {
		t.Fatalf("LoadLocalDotEnv() returned an error: %v", err)
	}

	if len(cfg.EnvFiles) != 1 || cfg.EnvFiles[0] != ".env" {
		t.Errorf("EnvFiles = %v, want [\".env\"]", cfg.EnvFiles)
	}

	if len(cfg.Internal.Environment) != 0 {
		t.Errorf("Internal.Environment = %v, want empty", cfg.Internal.Environment)
	}
}
