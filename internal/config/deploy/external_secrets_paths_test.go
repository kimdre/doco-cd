package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

func TestConfig_ExternalSecretsPaths_Parse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := `name: bulk
external_secrets_paths:
  - path: "proj:prod:/apps/"
    prefix: "APP_"
    recursive: true
  - path: "proj:prod:/shared"
`

	if err := createTestFile(t, filepath.Join(dir, ".doco-cd.yaml"), content); err != nil {
		t.Fatalf("create config: %v", err)
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config directory: %v", err)
	}

	configs, err := getConfigsFromFile(dir, files, ".doco-cd.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	want := []secrettypes.ExternalSecretsPath{
		{Path: "proj:prod:/apps/", Prefix: "APP_", Recursive: true},
		{Path: "proj:prod:/shared"},
	}

	if !slices.Equal(configs[0].ExternalSecretsPaths, want) {
		t.Errorf("ExternalSecretsPaths = %+v, want %+v", configs[0].ExternalSecretsPaths, want)
	}

	// The cache must hand out independent copies.
	configs[0].ExternalSecretsPaths[0].Path = "mutated"

	again, err := getConfigsFromFile(dir, files, ".doco-cd.yaml")
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if again[0].ExternalSecretsPaths[0].Path != "proj:prod:/apps/" {
		t.Errorf("cached config was mutated: %q", again[0].ExternalSecretsPaths[0].Path)
	}
}

func TestConfig_ExternalSecretsPaths_DefaultEmpty(t *testing.T) {
	t.Parallel()

	dc := Config{Name: "test"}

	if err := defaults.Set(&dc); err != nil {
		t.Fatalf("defaults: %v", err)
	}

	if len(dc.ExternalSecretsPaths) != 0 {
		t.Errorf("expected no secret paths by default, got %+v", dc.ExternalSecretsPaths)
	}
}

func TestConfig_Validate_ExternalSecretsPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		paths   []secrettypes.ExternalSecretsPath
		wantErr string
	}{
		{name: "valid", paths: []secrettypes.ExternalSecretsPath{{Path: "a"}, {Path: "b", Prefix: "B_", Recursive: true}}},
		{name: "none"},
		{name: "empty path", paths: []secrettypes.ExternalSecretsPath{{Path: "a"}, {Path: "  "}}, wantErr: "external_secrets_paths[1]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dc := Config{Name: "test", ExternalSecretsPaths: tt.paths}

			if err := defaults.Set(&dc); err != nil {
				t.Fatalf("defaults: %v", err)
			}

			err := dc.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				return
			}

			if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected ErrInvalidConfig containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestAutoDiscoverDeployments_ExternalSecretsPathsOverride(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	serviceDir := filepath.Join(repoRoot, "service1")

	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  web:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	nestedCfg := "external_secrets_paths:\n  - path: \"service/path\"\n"
	if err := createTestFile(t, filepath.Join(serviceDir, ".doco-cd.yaml"), nestedCfg); err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory:     ".",
		ComposeFiles:         []string{"compose.yaml"},
		AutoDiscovery:        AutoDiscoveryConfig{Enabled: true},
		ExternalSecretsPaths: []secrettypes.ExternalSecretsPath{{Path: "base/path", Recursive: true}},
	}

	configs, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	// Slices replace the base value, like external_secrets_files.
	want := []secrettypes.ExternalSecretsPath{{Path: "service/path"}}
	if !slices.Equal(configs[0].ExternalSecretsPaths, want) {
		t.Errorf("ExternalSecretsPaths = %+v, want %+v", configs[0].ExternalSecretsPaths, want)
	}
}
