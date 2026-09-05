package deploy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const remoteAutoDiscoveryFixtureCommit = "ee6dda09a7cef86ace9e5991dcf3c4b9a56716d3"

func createTestFile(t *testing.T, fileName string, content string) error {
	t.Helper()

	//nolint:gosec // test helper writes fixture files to test-controlled paths
	err := os.WriteFile(filepath.Clean(fileName), []byte(content), filesystem.PermOwner)
	if err != nil {
		return err
	}

	return nil
}

func TestGetConfigs(t *testing.T) {
	t.Parallel()

	t.Run("Valid Config", func(t *testing.T) {
		t.Parallel()

		fileName := ".doco-cd.yaml"
		reference := "refs/heads/test"
		contextName := "prod"
		workingDirectory := "/test"
		composeFiles := []string{"test.compose.yaml"}
		customTarget := ""

		dc := fmt.Sprintf(`name: %s
reference: %s
context: %s
working_dir: %s
compose_files:
  - %s
`, t.Name(), reference, contextName, workingDirectory, composeFiles[0])

		dirName := t.TempDir()

		createTestRepo(t, dirName)

		filePath := filepath.Join(dirName, fileName)

		err := createTestFile(t, filePath, dc)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetConfigs(dirName, ".", customTarget, reference, nil)
		if err != nil {
			t.Fatal(err)
		}

		if len(configs) != 1 {
			t.Fatalf("expected 1 config, got %d", len(configs))
		}

		c := configs[0]

		if c.Name != t.Name() {
			t.Errorf("expected name to be %v, got %s", t.Name(), c.Name)
		}

		if c.Reference != reference {
			t.Errorf("expected reference to be %v, got %s", reference, c.Reference)
		}

		if c.Context != contextName {
			t.Errorf("expected context to be %v, got %s", contextName, c.Context)
		}

		if c.WorkingDirectory != filepath.Join(".", workingDirectory) {
			t.Errorf("expected working directory to be '%v', got '%s'", workingDirectory, c.WorkingDirectory)
		}

		if !reflect.DeepEqual(c.ComposeFiles, composeFiles) {
			t.Errorf("expected compose files to be %v, got %v", composeFiles, c.ComposeFiles)
		}
	})
}

func TestGetConfigs_NonGitRepo(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	dc := `name: oci-stack
working_dir: .
compose_files:
  - compose.yaml
`

	if err := createTestFile(t, filepath.Join(repoRoot, ".doco-cd.yaml"), dc); err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigs(repoRoot, ".", "", "", nil)
	if err != nil {
		t.Fatalf("expected no error for non-git repo, got %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Name != "oci-stack" {
		t.Fatalf("expected config name %q, got %q", "oci-stack", configs[0].Name)
	}
}

func TestConfig_Validate_ContextTrim(t *testing.T) {
	t.Parallel()

	dc := Config{
		Name:    "test",
		Context: "  remote-prod  ",
	}

	if err := defaults.Set(&dc); err != nil {
		t.Fatalf("defaults: %v", err)
	}

	if err := dc.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if dc.Context != "remote-prod" {
		t.Fatalf("expected trimmed context %q, got %q", "remote-prod", dc.Context)
	}
}

func TestConfig_Validate_RepositoryURLAbsolutePathNormalization(t *testing.T) {
	t.Parallel()

	dc := Config{
		Name:          "test",
		RepositoryUrl: "/local-repos/my-app.git",
	}

	if err := defaults.Set(&dc); err != nil {
		t.Fatalf("defaults: %v", err)
	}

	if err := dc.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	if got := string(dc.RepositoryUrl); got != "file:///local-repos/my-app.git" {
		t.Fatalf("expected normalized repository_url %q, got %q", "file:///local-repos/my-app.git", got)
	}
}

func TestConfig_Validate_SwarmRetention(t *testing.T) {
	t.Parallel()

	t.Run("defaults to global when unset", func(t *testing.T) {
		t.Parallel()

		dc := Config{Name: "stack-a"}
		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		if err := dc.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}

		if dc.Swarm.ConfigRetention != nil {
			t.Fatalf("expected default swarm.config_retention to be unset (nil), got %v", dc.Swarm.ConfigRetention)
		}

		if dc.Swarm.SecretRetention != nil {
			t.Fatalf("expected default swarm.secret_retention to be unset (nil), got %v", dc.Swarm.SecretRetention)
		}

		if got := dc.ResolveSwarmConfigRetention(3); got != 3 {
			t.Fatalf("expected global config retention 3, got %d", got)
		}

		if got := dc.ResolveSwarmSecretRetention(4); got != 4 {
			t.Fatalf("expected global secret retention 4, got %d", got)
		}
	})

	t.Run("deploy overrides take precedence including zero", func(t *testing.T) {
		t.Parallel()

		dc := Config{Name: "stack-a"}
		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		dc.Swarm.ConfigRetention = new(0)
		dc.Swarm.SecretRetention = new(2)

		if err := dc.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}

		if got := dc.ResolveSwarmConfigRetention(5); got != 0 {
			t.Fatalf("expected deploy config retention 0 to override global, got %d", got)
		}

		if got := dc.ResolveSwarmSecretRetention(5); got != 2 {
			t.Fatalf("expected deploy secret retention 2 to override global, got %d", got)
		}
	})

	t.Run("allows minus one to disable pruning", func(t *testing.T) {
		t.Parallel()

		dc := Config{Name: "stack-a"}
		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		dc.Swarm.ConfigRetention = new(-1)
		dc.Swarm.SecretRetention = new(-1)

		if err := dc.Validate(); err != nil {
			t.Fatalf("expected -1 to be valid, got %v", err)
		}

		if got := dc.ResolveSwarmConfigRetention(3); got != -1 {
			t.Fatalf("expected deploy config retention -1 to override global, got %d", got)
		}

		if got := dc.ResolveSwarmSecretRetention(4); got != -1 {
			t.Fatalf("expected deploy secret retention -1 to override global, got %d", got)
		}
	})

	t.Run("rejects config retention less than minus one", func(t *testing.T) {
		t.Parallel()

		dc := Config{Name: "stack-a"}
		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		dc.Swarm.ConfigRetention = new(-2)

		err := dc.Validate()
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}

		if !strings.Contains(err.Error(), "swarm.config_retention must be >= -1") {
			t.Fatalf("expected swarm.config_retention validation error, got %v", err)
		}
	})

	t.Run("rejects secret retention less than minus one", func(t *testing.T) {
		t.Parallel()

		dc := Config{Name: "stack-a"}
		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		dc.Swarm.SecretRetention = new(-2)

		err := dc.Validate()
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}

		if !strings.Contains(err.Error(), "swarm.secret_retention must be >= -1") {
			t.Fatalf("expected swarm.secret_retention validation error, got %v", err)
		}
	})
}

func TestConfig_ResolveSwarmMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		enabled        *bool
		swarmAvailable bool
		want           bool
		wantErr        error
	}{
		{name: "inherits available swarm mode", swarmAvailable: true, want: true},
		{name: "inherits unavailable swarm mode", swarmAvailable: false, want: false},
		{name: "explicit compose on swarm", enabled: new(false), swarmAvailable: true, want: false},
		{name: "explicit swarm when available", enabled: new(true), swarmAvailable: true, want: true},
		{name: "explicit swarm when unavailable", enabled: new(true), swarmAvailable: false, wantErr: ErrSwarmModeUnavailable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Config{Swarm: SwarmConfig{Enabled: tt.enabled}}

			got, err := cfg.ResolveSwarmMode(tt.swarmAvailable)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected error %v, got %v", tt.wantErr, err)
			}

			if got != tt.want {
				t.Fatalf("expected swarm mode %t, got %t", tt.want, got)
			}
		})
	}
}

func TestGetConfigs_MissingDefaultConfigFile(t *testing.T) {
	t.Parallel()

	dirName := t.TempDir()

	createTestRepo(t, dirName)

	_, err := GetConfigs(dirName, ".", "", "", nil)
	if err == nil {
		t.Fatal("expected error when no default deployment config file exists, got nil")
	}

	if !errors.Is(err, ErrConfigFileNotFound) {
		t.Fatalf("expected ErrConfigFileNotFound, got %v", err)
	}

	if !strings.Contains(err.Error(), ".doco-cd.y(a)ml") {
		t.Fatalf("expected missing default config hint in error, got %v", err)
	}
}

func TestGetConfigs_MissingTargetConfigFile(t *testing.T) {
	t.Parallel()

	dirName := t.TempDir()

	createTestRepo(t, dirName)

	_, err := GetConfigs(dirName, ".", "nas", "", nil)
	if err == nil {
		t.Fatal("expected error when no target deployment config file exists, got nil")
	}

	if !errors.Is(err, ErrConfigFileNotFound) {
		t.Fatalf("expected ErrConfigFileNotFound, got %v", err)
	}

	if !strings.Contains(err.Error(), ".doco-cd.nas.y(a)ml") {
		t.Fatalf("expected missing target config hint in error, got %v", err)
	}
}

// TestGetConfigs_DuplicateProjectName checks that project names are unique per Docker context.
func TestGetConfigs_DuplicateProjectName(t *testing.T) {
	t.Parallel()

	dc := &Config{
		Name:             t.Name(),
		Reference:        "refs/heads/test",
		WorkingDirectory: "/test",
		ComposeFiles:     []string{"test.compose.yaml"},
	}

	t.Run("same context", func(t *testing.T) {
		err := ValidateUniqueProjectNames([]*Config{dc, dc})
		if !errors.Is(err, ErrDuplicateProjectName) {
			t.Fatal("expected error for duplicate project names in the same context, got nil")
		}
	})

	t.Run("different contexts", func(t *testing.T) {
		dc1 := *dc
		dc1.Context = "docker01"
		dc2 := *dc
		dc2.Context = "docker02"

		if err := ValidateUniqueProjectNames([]*Config{&dc1, &dc2}); err != nil {
			t.Fatalf("expected duplicate project names in different contexts to be valid, got %v", err)
		}
	})
}

// TestGetConfigs_RepositoryURL checks if the repository URL field validates Git URLs correctly.
// The init function panics if the validator for GitUrl is not registered correctly.
func TestGetConfigs_RepositoryURL(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name        string
		repoUrl     config.GitUrl
		expectedErr error
	}{
		{
			name:        "Valid HTTP URL",
			repoUrl:     "https://github.com/kimdre/doco-cd.git",
			expectedErr: nil,
		},
		{
			name:        "Valid HTTPS URL",
			repoUrl:     "https://github.com/kimdre/doco-cd.git",
			expectedErr: nil,
		},
		{
			name:        "Invalid HTTP URL",
			repoUrl:     "github.com/kimdre/doco-cd",
			expectedErr: fmt.Errorf("RepositoryUrl: %w", config.ErrInvalidGitUrl),
		},
		{
			name:        "SSH URL",
			repoUrl:     "git@github.com:kimdre/doco-cd.git",
			expectedErr: nil,
		},
		{
			name:        "SSH URL in ssh:// format",
			repoUrl:     "ssh://git@github.com:22/kimdre/doco-cd.git",
			expectedErr: nil,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dc := Config{
				Name:          tc.name,
				RepositoryUrl: tc.repoUrl,
			}

			err := validation.Validate(dc)
			if err == nil && tc.expectedErr != nil {
				t.Fatalf("expected error %v, got nil", tc.expectedErr)
			}

			if err != nil && strings.Contains(tc.expectedErr.Error(), err.Error()) {
				t.Fatalf("expected error %v, got %v", tc.expectedErr, err)
			}
		})
	}
}

func TestConfig_Validate_OciVersionField(t *testing.T) {
	t.Parallel()

	t.Run("defaults version to doco.v1", func(t *testing.T) {
		t.Parallel()

		dc := Config{
			Name:   "app",
			Source: config.SourceTypeOCI,
		}

		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		if err := dc.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}

		if dc.Version != config.OciArtifactLayoutV1 {
			t.Fatalf("expected version %q, got %q", config.OciArtifactLayoutV1, dc.Version)
		}
	})

	t.Run("rejects unsupported OCI version", func(t *testing.T) {
		t.Parallel()

		dc := Config{
			Name:    "app",
			Source:  config.SourceTypeOCI,
			Version: "doco.v2",
		}

		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		err := dc.Validate()
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}

		if !strings.Contains(err.Error(), "unsupported oci version") {
			t.Fatalf("expected unsupported oci version error, got %v", err)
		}
	})
}

func TestResolveConfigs_InlineOverride(t *testing.T) {
	t.Parallel()

	dirName := t.TempDir()

	deployment := &Config{Name: "inline-stack"}
	// Apply defaults to deployment
	if err := defaults.Set(deployment); err != nil {
		t.Fatalf("failed to set defaults: %v", err)
	}

	deployments := []*Config{deployment}
	customTarget := ""
	reference := "refs/heads/main"

	configs, err := ResolveConfigs(deployments, customTarget, reference, dirName, ".", nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Name != "inline-stack" {
		t.Errorf("expected name to be 'inline-stack', got '%s'", cfg.Name)
	}

	// Reference defaults to poll reference when unset inline
	if cfg.Reference != reference {
		t.Errorf("expected reference to be '%s', got '%s'", reference, cfg.Reference)
	}

	// Verify defaults applied
	if cfg.WorkingDirectory != "." {
		t.Errorf("expected working directory '.', got '%s'", cfg.WorkingDirectory)
	}

	if len(cfg.ComposeFiles) == 0 {
		t.Errorf("expected default compose files to be set")
	}

	if !cfg.Internal.OciTrustPolicyOverrideTrusted {
		t.Errorf("expected inline deployment OCI trust policy override to be trusted")
	}
}

func TestGetConfigsFromFileCachesDecodedConfigByContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fileName := filepath.Join(dir, ".doco-cd.yaml")
	initialConfig := "name: initial\ncontext: default\n"

	if err := createTestFile(t, fileName, initialConfig); err != nil {
		t.Fatalf("create config: %v", err)
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config directory: %v", err)
	}

	first, err := getConfigsFromFile(dir, files, ".doco-cd.yaml")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}

	first[0].Name = "mutated"

	second, err := getConfigsFromFile(dir, files, ".doco-cd.yaml")
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	if second[0].Name != "initial" {
		t.Fatalf("cached config was mutated: got %q, want %q", second[0].Name, "initial")
	}

	if err := createTestFile(t, fileName, "name: updated\ncontext: default\n"); err != nil {
		t.Fatalf("update config: %v", err)
	}

	third, err := getConfigsFromFile(dir, files, ".doco-cd.yaml")
	if err != nil {
		t.Fatalf("load updated config: %v", err)
	}

	if third[0].Name != "updated" {
		t.Fatalf("content change did not invalidate cache: got %q, want %q", third[0].Name, "updated")
	}
}

func TestCloneConfigSliceDeepCopiesMutableFields(t *testing.T) {
	t.Parallel()

	swarmEnabled := true
	configRetention := 3
	verifyOCI := true
	ignoreTlog := true
	configs := []*Config{{
		ComposeFiles: []string{"compose.yaml"},
		Environment:  map[string]string{"ENV": "original"},
		Swarm: SwarmConfig{
			Enabled:         &swarmEnabled,
			ConfigRetention: &configRetention,
		},
		Reconciliation: ReconciliationConfig{Events: []string{"unhealthy"}},
		Oci: config.OciTrustPolicyOverride{
			Verify:            &verifyOCI,
			IgnoreTlog:        &ignoreTlog,
			KeylessIdentities: []config.OciKeylessIdentity{{Issuer: "issuer"}},
			PublicKeys:        []string{"public-key"},
		},
		ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
			"SECRET": {RemoteRef: map[string]any{"nested": map[string]any{"value": "original"}}},
		},
	}}

	cloned := cloneConfigSlice(configs)
	configs[0].ComposeFiles[0] = "changed.yaml"
	configs[0].Environment["ENV"] = "changed"
	*configs[0].Swarm.Enabled = false
	*configs[0].Swarm.ConfigRetention = 4
	configs[0].Reconciliation.Events[0] = "die"
	*configs[0].Oci.Verify = false
	*configs[0].Oci.IgnoreTlog = false
	configs[0].Oci.KeylessIdentities[0].Issuer = "changed"
	configs[0].Oci.PublicKeys[0] = "changed"
	configs[0].ExternalSecrets["SECRET"] = secrettypes.ExternalSecretRef{
		RemoteRef: map[string]any{"nested": map[string]any{"value": "changed"}},
	}

	got := cloned[0]
	if got.ComposeFiles[0] != "compose.yaml" || got.Environment["ENV"] != "original" ||
		!*got.Swarm.Enabled || *got.Swarm.ConfigRetention != 3 ||
		got.Reconciliation.Events[0] != "unhealthy" || !*got.Oci.Verify ||
		!*got.Oci.IgnoreTlog || got.Oci.KeylessIdentities[0].Issuer != "issuer" ||
		got.Oci.PublicKeys[0] != "public-key" ||
		got.ExternalSecrets["SECRET"].RemoteRef["nested"].(map[string]any)["value"] != "original" {
		t.Fatalf("cloneConfigSlice did not isolate mutable configuration fields: %#v", got)
	}
}

func TestResolveConfigs_InlineMissingName(t *testing.T) {
	t.Parallel()

	deployments := []*Config{{}}

	// Empty name deployment should error when validated
	for _, d := range deployments {
		if err := d.Validate(); err == nil {
			t.Fatalf("expected error for missing name, got nil")
		}
	}
}

func TestResolveConfigs_InlineAutoDiscover(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	servicesDir := filepath.Join(repoRoot, "services")
	serviceOneDir := filepath.Join(servicesDir, "service-one")
	serviceTwoDir := filepath.Join(servicesDir, "service-two")

	for _, dir := range []string{serviceOneDir, serviceTwoDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("failed to create service dir %s: %v", dir, err)
		}

		composeFile := filepath.Join(dir, "compose.yaml")
		if err := createTestFile(t, composeFile, "services:\n  app:\n    image: alpine"); err != nil {
			t.Fatalf("failed to write compose file for %s: %v", dir, err)
		}
	}

	deployment := &Config{
		WorkingDirectory: "services",
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}
	// Apply defaults to deployment
	if err := defaults.Set(deployment); err != nil {
		t.Fatalf("failed to set defaults: %v", err)
	}

	deployments := []*Config{deployment}
	customTarget := ""
	reference := "refs/heads/main"

	configs, err := ResolveConfigs(deployments, customTarget, reference, repoRoot, ".", nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}

	found := map[string]bool{}
	for _, cfg := range configs {
		found[cfg.Name] = true
		if !strings.HasPrefix(cfg.WorkingDirectory, "services") {
			t.Errorf("expected working directory to stay within services/, got %s", cfg.WorkingDirectory)
		}
	}

	if !found["service-one"] {
		t.Errorf("expected to discover service-one deployment")
	}

	if !found["service-two"] {
		t.Errorf("expected to discover service-two deployment")
	}
}

func TestGetConfigs_WithSubdirectory(t *testing.T) {
	t.Parallel()

	fileName := ".doco-cd.yaml"
	reference := "refs/heads/main"
	configBaseDir := "configs"
	customTarget := ""

	dc := fmt.Sprintf(`name: %s
reference: %s
`, t.Name(), reference)

	// Create temporary repo root
	repoRoot := t.TempDir()

	createTestRepo(t, repoRoot)

	// Create subdirectory for configs
	configDir := filepath.Join(repoRoot, configBaseDir)

	err := os.MkdirAll(configDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	// Create config file in subdirectory
	filePath := filepath.Join(configDir, fileName)

	err = createTestFile(t, filePath, dc)
	if err != nil {
		t.Fatal(err)
	}

	// Test with subdirectory as configBaseDir
	configs, err := GetConfigs(repoRoot, configBaseDir, customTarget, reference, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	c := configs[0]
	if c.Name != t.Name() {
		t.Errorf("expected name to be %v, got %s", t.Name(), c.Name)
	}

	if c.Reference != reference {
		t.Errorf("expected reference to be %v, got %s", reference, c.Reference)
	}
}

func TestGetConfigs_WithRootDirectory(t *testing.T) {
	t.Parallel()

	fileName := ".doco-cd.yaml"
	reference := "refs/heads/main"
	configBaseDir := "."
	customTarget := ""

	dc := fmt.Sprintf(`name: %s
reference: %s
`, t.Name(), reference)

	repoRoot := t.TempDir()

	createTestRepo(t, repoRoot)

	filePath := filepath.Join(repoRoot, fileName)

	err := createTestFile(t, filePath, dc)
	if err != nil {
		t.Fatal(err)
	}

	// Test with root directory as configBaseDir
	configs, err := GetConfigs(repoRoot, configBaseDir, customTarget, reference, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	c := configs[0]
	if c.Name != t.Name() {
		t.Errorf("expected name to be %v, got %s", t.Name(), c.Name)
	}
}

func TestGetConfigs_WithAutoDiscovery(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	createTestRepo(t, repoRoot)

	// Create a compose file in random subdirectory to trigger auto-discovery
	subDir := filepath.Join(repoRoot, t.Name())

	err := os.MkdirAll(subDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(subDir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	dc := fmt.Sprintf(`name: %s
reference: main
auto_discovery:
  enabled: true
`, t.Name())

	filePath := filepath.Join(repoRoot, ".doco-cd.yaml")

	err = createTestFile(t, filePath, dc)
	if err != nil {
		t.Fatal(err)
	}

	// Test with auto-discovery enabled
	configs, err := GetConfigs(repoRoot, ".", "", "main", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Name != t.Name() {
		t.Errorf("expected name to be %v, got %s", t.Name(), configs[0].Name)
	}

	if !configs[0].AutoDiscovery.Enabled {
		t.Errorf("expected AutoDiscovery.Enabled to be true, got false")
	}
}

func TestGetConfigs_WithAutoDiscovery_NoComposeFiles(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	createTestRepo(t, repoRoot)

	configFile := filepath.Join(repoRoot, ".doco-cd.yaml")

	err := createTestFile(t, configFile, `auto_discovery:
  enabled: true
`)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigs(repoRoot, ".", "", "main", nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(configs) != 0 {
		t.Fatalf("expected 0 configs, got %d", len(configs))
	}
}

func TestGetConfigs_WithAutoDiscovery_OnDifferentBranch(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	repo := createTestRepo(t, repoRoot)

	// Create a new branch and switch to it
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	err = worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature-branch"),
		Create: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fake remote reference for feature-branch
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	ref := plumbing.NewHashReference("refs/remotes/origin/feature-branch", head.Hash())

	err = repo.Storer.SetReference(ref)
	if err != nil {
		t.Fatal(err)
	}

	// Create a compose file in random subdirectory to trigger auto-discovery
	subDir := filepath.Join(repoRoot, t.Name())

	err = os.MkdirAll(subDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(subDir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	dc := fmt.Sprintf(`name: %s
reference: refs/heads/feature-branch
auto_discovery:
  enabled: true
`, t.Name())

	filePath := filepath.Join(repoRoot, ".doco-cd.yaml")

	err = createTestFile(t, filePath, dc)
	if err != nil {
		t.Fatal(err)
	}

	// Test with auto-discovery enabled on feature branch
	configs, err := GetConfigs(repoRoot, ".", "", "refs/heads/feature-branch", nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Name != t.Name() {
		t.Errorf("expected name to be %v, got %s", t.Name(), configs[0].Name)
	}

	if !configs[0].AutoDiscovery.Enabled {
		t.Errorf("expected AutoDiscovery.Enabled to be true, got false")
	}
}

func TestGetConfigs_WithAutoDiscovery_WithRemoteUrl(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		branch          string
		expectedConfigs int
	}{
		{
			name:            "Main Branch",
			branch:          "main",
			expectedConfigs: 1,
		},
		{
			name:            "Dual Branch",
			branch:          "dual",
			expectedConfigs: 2,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoRoot := t.TempDir()
			// Create subdirectory for configs
			subDir := filepath.Join(repoRoot, t.Name())

			createTestRepo(t, subDir)

			dc := fmt.Sprintf(`name: %s
reference: %s
auto_discovery:
  enabled: true
repository_url: https://github.com/kimdre/doco-cd_tests.git
`, t.Name(), tc.branch)

			filePath := filepath.Join(subDir, ".doco-cd.yaml")

			err := createTestFile(t, filePath, dc)
			if err != nil {
				t.Fatal(err)
			}

			// Test with auto-discovery enabled and repository URL set (should ignore repository URL for discovery)
			configs, err := GetConfigs(subDir, ".", "", "main", nil)
			if err != nil {
				t.Fatal(err)
			}

			if len(configs) != tc.expectedConfigs {
				t.Fatalf("expected 1 config, got %d", len(configs))
			}

			if tc.expectedConfigs == 1 && configs[0].Name != "test-deploy" {
				t.Errorf("expected name to be 'test-deploy' (from nested config), got %s", configs[0].Name)
			} else if tc.expectedConfigs == 2 {
				if configs[0].Name != "app1" && configs[1].Name != "app2" {
					t.Fatalf("expected names to be 'app1' and 'app2', got '%s' and '%s'", configs[0].Name, configs[1].Name)
				}
			}

			if !configs[0].AutoDiscovery.Enabled {
				t.Errorf("expected AutoDiscovery.Enabled to be true, got false")
			}

			if configs[0].Reference != tc.branch {
				t.Errorf("expected reference to be '^main', got '%s'", configs[0].Reference)
			}
		})
	}
}

func TestResolveConfigs_WithSubdirectory(t *testing.T) {
	t.Parallel()

	fileName := ".doco-cd.yaml"
	reference := "refs/heads/main"
	configBaseDir := "config"

	dc := fmt.Sprintf(`name: %s
reference: %s
`, t.Name(), reference)

	repoRoot := t.TempDir()

	createTestRepo(t, repoRoot)

	configDir := filepath.Join(repoRoot, configBaseDir)

	err := os.MkdirAll(configDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(configDir, fileName)

	err = createTestFile(t, filePath, dc)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := ResolveConfigs(nil, "", reference, repoRoot, configBaseDir, nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Name != t.Name() {
		t.Errorf("expected name to be %v, got %s", t.Name(), configs[0].Name)
	}

	if configs[0].Internal.OciTrustPolicyOverrideTrusted {
		t.Errorf("expected repository config OCI trust policy override to be untrusted")
	}
}

func TestResolveConfigs_MissingRepositoryConfigFile(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	createTestRepo(t, repoRoot)

	_, err := ResolveConfigs(nil, "", "refs/heads/main", repoRoot, ".", nil)
	if err == nil {
		t.Fatal("expected error when repository deploy config file is missing, got nil")
	}

	if !errors.Is(err, ErrConfigFileNotFound) {
		t.Fatalf("expected ErrConfigFileNotFound, got %v", err)
	}

	if !strings.Contains(err.Error(), ".doco-cd.y(a)ml") {
		t.Fatalf("expected missing default config hint in error, got %v", err)
	}
}

func TestResolveConfigs_MissingRepositoryTargetConfigFile(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	createTestRepo(t, repoRoot)

	_, err := ResolveConfigs(nil, "nas", "refs/heads/main", repoRoot, ".", nil)
	if err == nil {
		t.Fatal("expected error when repository target deploy config file is missing, got nil")
	}

	if !errors.Is(err, ErrConfigFileNotFound) {
		t.Fatalf("expected ErrConfigFileNotFound, got %v", err)
	}

	if !strings.Contains(err.Error(), ".doco-cd.nas.y(a)ml") {
		t.Fatalf("expected missing target config hint in error, got %v", err)
	}
}

func TestAutoDiscoverDeployments_BasicDiscovery(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	// Create subdirectories with compose files
	service1Dir := filepath.Join(repoRoot, "service1")
	service2Dir := filepath.Join(repoRoot, "service2")

	err := os.MkdirAll(service1Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(service2Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	// Create compose files

	err = createTestFile(t, filepath.Join(service1Dir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(service2Dir, "docker-compose.yml"), "services:\n  db:\n    image: postgres")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml", "docker-compose.yml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}

	// Check that both services were discovered
	foundService1 := false
	foundService2 := false

	for _, cfg := range configs {
		if cfg.Name == "service1" {
			foundService1 = true

			if cfg.WorkingDirectory != "service1" {
				t.Errorf("expected working directory to be 'service1', got '%s'", cfg.WorkingDirectory)
			}
		}

		if cfg.Name == "service2" {
			foundService2 = true

			if cfg.WorkingDirectory != "service2" {
				t.Errorf("expected working directory to be 'service2', got '%s'", cfg.WorkingDirectory)
			}
		}
	}

	if !foundService1 {
		t.Error("service1 was not discovered")
	}

	if !foundService2 {
		t.Error("service2 was not discovered")
	}
}

func TestAutoDiscoverDeployments_WithWorkingDirectory(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	// Create a services subdirectory
	servicesDir := filepath.Join(repoRoot, "services")
	service1Dir := filepath.Join(servicesDir, "service1")

	err := os.MkdirAll(service1Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(service1Dir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: "services",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Name != "service1" {
		t.Errorf("expected name to be 'service1', got '%s'", configs[0].Name)
	}

	// WorkingDirectory should be repo-root-relative
	if configs[0].WorkingDirectory != filepath.Join("services", "service1") {
		t.Errorf("expected working directory to be 'services/service1', got '%s'", configs[0].WorkingDirectory)
	}
}

func TestAutoDiscoverDeployments_WithDepthLimit(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	// Create nested directories
	level1Dir := filepath.Join(repoRoot, "level1")
	level2Dir := filepath.Join(level1Dir, "level2")
	level3Dir := filepath.Join(level2Dir, "level3")

	err := os.MkdirAll(level3Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	// Create compose files at different levels

	err = createTestFile(t, filepath.Join(level1Dir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(level2Dir, "compose.yaml"), "services:\n  db:\n    image: postgres")
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(level3Dir, "compose.yaml"), "services:\n  cache:\n    image: redis")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}
	baseConfig.AutoDiscovery.ScanDepth = 2

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	// Should only find level1 and level2, not level3 (depth limit is 2)
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs (depth limited), got %d", len(configs))
	}

	foundLevel3 := false

	for _, cfg := range configs {
		if cfg.Name == "level3" {
			foundLevel3 = true
		}
	}

	if foundLevel3 {
		t.Error("level3 should not have been discovered due to depth limit")
	}
}

func TestAutoDiscoverDeployments_NoComposeFiles(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	// Create subdirectories without compose files
	service1Dir := filepath.Join(repoRoot, "service1")

	err := os.MkdirAll(service1Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 0 {
		t.Fatalf("expected 0 configs, got %d", len(configs))
	}
}

func TestAutoDiscoverDeployments_InheritBaseConfig(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	serviceDir := filepath.Join(repoRoot, "service1")

	err := os.MkdirAll(serviceDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
		Reference:        "refs/heads/main",
		RemoveOrphans:    false,
		ForceRecreate:    true,
		Timeout:          300,
		Profiles:         []string{"prod"},
	}

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	cfg := configs[0]

	// Check that base config properties were inherited
	if cfg.Reference != baseConfig.Reference {
		t.Errorf("expected reference to be inherited: %s, got %s", baseConfig.Reference, cfg.Reference)
	}

	if cfg.RemoveOrphans != baseConfig.RemoveOrphans {
		t.Errorf("expected RemoveOrphans to be inherited: %v, got %v", baseConfig.RemoveOrphans, cfg.RemoveOrphans)
	}

	if cfg.ForceRecreate != baseConfig.ForceRecreate {
		t.Errorf("expected ForceRecreate to be inherited: %v, got %v", baseConfig.ForceRecreate, cfg.ForceRecreate)
	}

	if cfg.Timeout != baseConfig.Timeout {
		t.Errorf("expected Timeout to be inherited: %d, got %d", baseConfig.Timeout, cfg.Timeout)
	}

	if !reflect.DeepEqual(cfg.Profiles, baseConfig.Profiles) {
		t.Errorf("expected Profiles to be inherited: %v, got %v", baseConfig.Profiles, cfg.Profiles)
	}
}

// createTestRepo initializes a git repository at the specified path with a single commit on the main branch.
func createTestRepo(t *testing.T, repoPath string) (repo *git.Repository) {
	t.Helper()

	// Init git repo at repoRoot with main branch
	repo, err := git.PlainInitWithOptions(repoPath, &git.PlainInitOptions{
		Bare:          false,
		DefaultBranch: DefaultReference,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create initial commit to main branch
	w, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(t, filepath.Join(repoPath, "README.md"), "Test repository for auto-discovery")
	if err != nil {
		t.Fatal(err)
	}

	_, err = w.Add("README.md")
	if err != nil {
		t.Fatal(err)
	}

	_, err = w.Commit("Initial commit", &git.CommitOptions{
		All: true,
		Author: &object.Signature{
			Name:  "Test Author",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// After the commit, create a fake remote reference
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	// Create a remote-style reference that GetReferenceSet expects
	ref := plumbing.NewHashReference("refs/remotes/origin/main", head.Hash())

	err = repo.Storer.SetReference(ref)
	if err != nil {
		t.Fatal(err)
	}

	return repo
}

func TestGetConfigs_WithAutoDiscovery_WithRemoteUrl_WithMultipleConfigs(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	createTestRepo(t, repoRoot)

	// Three deploy configs in one file using YAML document separators
	dc := fmt.Sprintf(`
# Main branch fixture - should discover 1 deployment with name 'test-deploy'
name: main-stack
repository_url: https://github.com/kimdre/doco-cd_tests.git
reference: main
auto_discovery:
  enabled: true
---
# Pinned remote fixture - should discover 1 deployment with name 'test-deploy1'
name: remote-stack
repository_url: https://github.com/kimdre/doco-cd_tests.git
reference: %s
compose_files: ["test.compose.yaml"]
auto_discovery:
  enabled: true
---
# Dual branch fixture - should discover 2 deployments with names 'app1' and 'app2'
name: dual-stack
repository_url: https://github.com/kimdre/doco-cd_tests.git
reference: dual
auto_discovery:
  enabled: true
`, remoteAutoDiscoveryFixtureCommit)

	filePath := filepath.Join(repoRoot, ".doco-cd.yaml")

	err := createTestFile(t, filePath, dc)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigs(repoRoot, ".", "", "main", nil)
	if err != nil {
		t.Fatal(err)
	}

	expected := map[string]struct{}{
		"test-deploy@main": {},
		"test-deploy1@" + remoteAutoDiscoveryFixtureCommit: {},
		"app1@dual": {},
		"app2@dual": {},
	}

	if len(configs) != len(expected) {
		t.Fatalf("expected %d configs, got %d", len(expected), len(configs))
	}

	seen := make(map[string]int, len(configs))

	for _, cfg := range configs {
		t.Logf("Discovered config: Name=%s, Reference=%s", cfg.Name, cfg.Reference)

		if cfg.RepositoryUrl != "https://github.com/kimdre/doco-cd_tests.git" {
			t.Errorf("unexpected repository URL %q", cfg.RepositoryUrl)
		}

		seen[cfg.Name+"@"+cfg.Reference]++
	}

	for key := range expected {
		if seen[key] != 1 {
			t.Errorf("expected config %q exactly once, found %d", key, seen[key])
		}
	}
}
