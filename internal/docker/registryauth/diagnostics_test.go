package registryauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/cli/cli/config/configfile"
	configtypes "github.com/docker/cli/cli/config/types"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

func TestCheckDockerConfigReadable(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		cfg     *configfile.ConfigFile
		wantErr bool
		errMsg  string
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: true,
			errMsg:  "docker config is nil",
		},
		{
			name: "config with non-existent file is acceptable",
			cfg: &configfile.ConfigFile{
				Filename: "/nonexistent/path/.docker/config.json",
			},
			wantErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckDockerConfigReadable(tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckDockerConfigReadable() error = %v, wantErr %v", err, tc.wantErr)
			}

			if tc.wantErr && err != nil && tc.errMsg != "" && !strings.Contains(err.Error(), tc.errMsg) {
				t.Fatalf("CheckDockerConfigReadable() error = %v, want error containing %q", err, tc.errMsg)
			}
		})
	}

	t.Run("config with readable and valid config file", func(t *testing.T) {
		tmpFile := t.TempDir()
		tmpConfigPath := filepath.Join(tmpFile, "config.json")
		// Valid docker config format
		err := os.WriteFile(tmpConfigPath, []byte(`{"auths":{}}`), filesystem.PermOwner)
		if err != nil {
			t.Fatalf("failed to create temp config file: %v", err)
		}

		cfg := loadDockerConfigFile(t, tmpConfigPath)

		err = CheckDockerConfigReadable(cfg)
		if err != nil {
			t.Fatalf("CheckDockerConfigReadable() error = %v, wantErr false", err)
		}
	})

	t.Run("config with unreadable file", func(t *testing.T) {
		tmpFile := t.TempDir()
		tmpConfigPath := filepath.Join(tmpFile, "config.json")

		err := os.WriteFile(tmpConfigPath, []byte(`{"auths":{}}`), filesystem.PermOwner)
		if err != nil {
			t.Fatalf("failed to create temp config file: %v", err)
		}

		// Change permissions to make unreadable
		err = os.Chmod(tmpConfigPath, 0o000)
		if err != nil {
			t.Fatalf("failed to change file permissions: %v", err)
		}
		defer os.Chmod(tmpConfigPath, filesystem.PermOwner) // nolint:errcheck

		cfg := &configfile.ConfigFile{
			Filename: tmpConfigPath,
		}

		err = CheckDockerConfigReadable(cfg)
		if err == nil {
			t.Fatalf("CheckDockerConfigReadable() expected error for unreadable file, got nil")
		}

		if !strings.Contains(err.Error(), "not readable") {
			t.Fatalf("CheckDockerConfigReadable() error = %v, want error containing 'not readable'", err)
		}
	})

	t.Run("config with invalid JSON", func(t *testing.T) {
		tmpFile := t.TempDir()
		tmpConfigPath := filepath.Join(tmpFile, "config.json")
		// Invalid JSON format
		err := os.WriteFile(tmpConfigPath, []byte(`{invalid json content}`), filesystem.PermOwner)
		if err != nil {
			t.Fatalf("failed to create temp config file: %v", err)
		}

		cfg := &configfile.ConfigFile{
			Filename: tmpConfigPath,
		}

		err = CheckDockerConfigReadable(cfg)
		if err == nil {
			t.Fatalf("CheckDockerConfigReadable() expected error for invalid config, got nil")
		}

		if !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("CheckDockerConfigReadable() error = %v, want error containing 'invalid'", err)
		}
	})

	t.Run("config with missing credential helper binaries", func(t *testing.T) {
		tmpFile := t.TempDir()
		tmpConfigPath := filepath.Join(tmpFile, "config.json")
		// Valid config with global and registry-specific helpers that are unavailable.
		err := os.WriteFile(tmpConfigPath, []byte(`{"credsStore":"fake-global-helper-12345","credHelpers":{"ghcr.io":"fake-registry-helper-12345"}}`), filesystem.PermOwner)
		if err != nil {
			t.Fatalf("failed to create temp config file: %v", err)
		}

		cfg := loadDockerConfigFile(t, tmpConfigPath)

		err = CheckDockerConfigReadable(cfg)
		if err == nil {
			t.Fatalf("CheckDockerConfigReadable() expected error for missing credential helper, got nil")
		}

		if !strings.Contains(err.Error(), "missing credential helper binaries") {
			t.Fatalf("CheckDockerConfigReadable() error = %v, want error containing 'missing credential helper binaries'", err)
		}

		for _, helper := range []string{"fake-global-helper-12345", "fake-registry-helper-12345"} {
			if !strings.Contains(err.Error(), helper) {
				t.Fatalf("CheckDockerConfigReadable() error = %v, want error containing %q", err, helper)
			}
		}
	})
}

func loadDockerConfigFile(t *testing.T, configPath string) *configfile.ConfigFile {
	t.Helper()

	file, err := os.Open(configPath)
	if err != nil {
		t.Fatalf("failed to open temp config file: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("failed to close temp config file: %v", err)
		}
	}()

	cfg := configfile.New(configPath)
	if err := cfg.LoadFromReader(file); err != nil {
		t.Fatalf("failed to load temp config file: %v", err)
	}

	return cfg
}

func TestIsAuthRelatedError(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		err  error
		want bool
	}{
		{err: nil, want: false},
		{err: errors.New("network timeout"), want: false},
		{err: errors.New("pull access denied for private/app"), want: true},
		{err: errors.New("Error response from daemon: unauthorized"), want: true},
		{err: errors.New("error getting credentials - err: exit status 1"), want: true},
	}

	for _, tc := range testCases {
		if got := IsAuthRelatedError(tc.err); got != tc.want {
			t.Fatalf("IsAuthRelatedError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestBuildFailureHint(t *testing.T) {
	t.Parallel()

	cfg := &configfile.ConfigFile{
		Filename:         "/root/.docker/config.json",
		CredentialsStore: "__missing_helper__",
	}

	hint := BuildFailureHint(cfg, []string{"ghcr.io/example/private:latest"}, errors.New("pull access denied"))
	if !strings.Contains(hint, "registry auth hint:") {
		t.Fatalf("BuildFailureHint() missing prefix: %q", hint)
	}

	if !strings.Contains(hint, "docker config path: /root/.docker/config.json") {
		t.Fatalf("BuildFailureHint() missing config path hint: %q", hint)
	}

	if !strings.Contains(hint, "missing helper binaries in container:") {
		t.Fatalf("BuildFailureHint() missing helper-binary hint: %q", hint)
	}

	if !strings.Contains(hint, "helper-based credentials require matching docker-credential-* binaries") {
		t.Fatalf("BuildFailureHint() missing helper requirement hint: %q", hint)
	}
}

func TestBuildFailureHint_NoAuthError(t *testing.T) {
	t.Parallel()

	cfg := &configfile.ConfigFile{
		AuthConfigs: map[string]configtypes.AuthConfig{
			"ghcr.io": {Username: "u", Password: "p"},
		},
	}

	if hint := BuildFailureHint(cfg, []string{"ghcr.io/example/private:latest"}, errors.New("connection reset by peer")); hint != "" {
		t.Fatalf("BuildFailureHint() = %q, want empty hint", hint)
	}
}

func TestWrapLookupError(t *testing.T) {
	t.Parallel()

	cfg := &configfile.ConfigFile{
		Filename:         "/root/.docker/config.json",
		CredentialsStore: "__missing_helper__",
	}

	err := WrapLookupError(cfg, "ghcr.io/example/private:latest", errors.New("error getting credentials"))
	if err == nil {
		t.Fatal("WrapLookupError() returned nil error")
	}

	if !strings.Contains(err.Error(), "retrieve auth token from image") {
		t.Fatalf("WrapLookupError() missing base context: %v", err)
	}

	if !strings.Contains(err.Error(), "registry auth hint:") {
		t.Fatalf("WrapLookupError() missing hint: %v", err)
	}
}
