package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadLocalDotEnvSupportsFilePrefix(t *testing.T) {
	repoDir := t.TempDir()

	localEnvPath := filepath.Join(repoDir, "local.env")
	if err := os.WriteFile(localEnvPath, []byte("LOCAL_KEY=local\n"), 0o600); err != nil {
		t.Fatalf("failed to write local env file: %v", err)
	}

	fileEnvDir := t.TempDir()
	fileEnvName := "host.env"

	fileEnvPath := filepath.Join(fileEnvDir, fileEnvName)
	if err := os.WriteFile(fileEnvPath, []byte("HOST_KEY=host\n"), 0o600); err != nil {
		t.Fatalf("failed to write host env file: %v", err)
	}

	deployConfig := &DeployConfig{
		EnvFiles: []string{
			"local.env",
			"remote:remote.env",
			"file:" + fileEnvName,
		},
	}

	if err := LoadLocalDotEnv(deployConfig, repoDir, fileEnvDir); err != nil {
		t.Fatalf("LoadLocalDotEnv returned error: %v", err)
	}

	if !reflect.DeepEqual(deployConfig.EnvFiles, []string{"remote.env"}) {
		t.Fatalf("unexpected env files: %v", deployConfig.EnvFiles)
	}

	if deployConfig.Internal.Environment["LOCAL_KEY"] != "local" {
		t.Fatalf("expected LOCAL_KEY to be loaded, got %q", deployConfig.Internal.Environment["LOCAL_KEY"])
	}

	if deployConfig.Internal.Environment["HOST_KEY"] != "host" {
		t.Fatalf("expected HOST_KEY to be loaded, got %q", deployConfig.Internal.Environment["HOST_KEY"])
	}
}

func TestLoadLocalDotEnvFilePrefixRequiresRelative(t *testing.T) {
	deployConfig := &DeployConfig{
		EnvFiles: []string{"file:/abs/env"},
	}

	if err := LoadLocalDotEnv(deployConfig, t.TempDir(), t.TempDir()); err == nil || !errors.Is(err, ErrInvalidFilePath) {
		if err == nil {
			t.Fatal("expected error for absolute file path, got nil")
		}

		t.Fatalf("expected ErrInvalidFilePath, got %v", err)
	}
}

func TestLoadLocalDotEnvFilePrefixRequiresConfiguredEnvDir(t *testing.T) {
	deployConfig := &DeployConfig{
		EnvFiles: []string{"file:relative.env"},
	}

	if err := LoadLocalDotEnv(deployConfig, t.TempDir(), ""); err == nil || !errors.Is(err, ErrInvalidFilePath) {
		if err == nil {
			t.Fatal("expected error when env dir is not configured, got nil")
		}

		t.Fatalf("expected ErrInvalidFilePath, got %v", err)
	}
}

func TestLoadLocalDotEnvFilePrefixPreventsTraversal(t *testing.T) {
	deployConfig := &DeployConfig{
		EnvFiles: []string{"file:../secret.env"},
	}

	if err := LoadLocalDotEnv(deployConfig, t.TempDir(), t.TempDir()); err == nil || !errors.Is(err, ErrInvalidFilePath) {
		if err == nil {
			t.Fatal("expected error for path traversal, got nil")
		}

		t.Fatalf("expected ErrInvalidFilePath, got %v", err)
	}
}
