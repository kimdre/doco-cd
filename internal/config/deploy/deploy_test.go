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

	"github.com/creasty/defaults"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"gopkg.in/validator.v2"

	"github.com/kimdre/doco-cd/internal/config"

	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/hook"
)

func createTestFile(t *testing.T, fileName string, content string) error {
	t.Helper()

	err := os.WriteFile(fileName, []byte(content), filesystem.PermOwner)
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
		workingDirectory := "/test"
		composeFiles := []string{"test.compose.yaml"}
		customTarget := ""

		dc := fmt.Sprintf(`name: %s
reference: %s
working_dir: %s
compose_files:
  - %s
`, t.Name(), reference, workingDirectory, composeFiles[0])

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

// TestGetConfigs_DuplicateProjectName checks if the function returns an error
// when there are duplicate project names in the config files.
func TestGetConfigs_DuplicateProjectName(t *testing.T) {
	t.Parallel()

	dc := Config{
		Name:             t.Name(),
		Reference:        "refs/heads/test",
		WorkingDirectory: "/test",
		ComposeFiles:     []string{"test.compose.yaml"},
	}

	configs := []*Config{&dc, &dc}

	err := ValidateUniqueProjectNames(configs)
	if !errors.Is(err, ErrDuplicateProjectName) {
		t.Fatal("expected error for duplicate project names, got nil")
	}
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

			err := validator.Validate(dc)
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

func TestConfig_Hooks(t *testing.T) {
	t.Parallel()

	t.Run("parses success and failure hooks from yaml", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		file := filepath.Join(dir, ".doco-cd.yaml")
		content := `name: app
reference: main
hooks:
  on_success:
    - url: https://example.com/ok
      headers:
        X-Token: secret
  on_failure:
    - url: http://example.com/fail
      method: PUT
`

		if err := createTestFile(t, file, content); err != nil {
			t.Fatalf("write file: %v", err)
		}

		configs, err := GetConfigFromYAML(file, true)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}

		dc := configs[0]
		if err := dc.Validate(); err != nil {
			t.Fatalf("validate: %v", err)
		}

		if len(dc.Hooks.OnSuccess) != 1 || dc.Hooks.OnSuccess[0].URL != "https://example.com/ok" {
			t.Fatalf("unexpected on_success hooks: %+v", dc.Hooks.OnSuccess)
		}

		if dc.Hooks.OnSuccess[0].Headers["X-Token"] != "secret" {
			t.Fatalf("expected header X-Token=secret, got %+v", dc.Hooks.OnSuccess[0].Headers)
		}

		if len(dc.Hooks.OnFailure) != 1 || dc.Hooks.OnFailure[0].Method != "PUT" {
			t.Fatalf("unexpected on_failure hooks: %+v", dc.Hooks.OnFailure)
		}
	})

	t.Run("rejects invalid hook url", func(t *testing.T) {
		t.Parallel()

		dc := Config{
			Name:  "app",
			Hooks: hook.Config{OnSuccess: []hook.Webhook{{URL: "ftp://example.com"}}},
		}

		if err := defaults.Set(&dc); err != nil {
			t.Fatalf("defaults: %v", err)
		}

		err := dc.Validate()
		if err == nil || !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("expected ErrInvalidConfig, got %v", err)
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
		Bare: false,
		InitOptions: git.InitOptions{
			DefaultBranch: DefaultReference,
		},
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

	// Two deploy configs in one file using YAML document separator
	dc := `
# Config for main branch - should discover 1 deployment with name 'test'
name: main-stack
repository_url: https://github.com/kimdre/doco-cd_tests.git
reference: main
auto_discovery:
  enabled: true
---
# Config for doco-cd repo - should discover 1 deployment with name 'test''
name: test-stack
repository_url: https://github.com/kimdre/doco-cd.git
reference: main
compose_files: ["test.compose.yaml"]
working_dir: test
auto_discovery:
  enabled: true
---
# Config for dual branch - should discover 2 deployments with names 'app1' and 'app2'
name: dual-stack
repository_url: https://github.com/kimdre/doco-cd_tests.git
reference: dual
auto_discovery:
  enabled: true
`

	filePath := filepath.Join(repoRoot, ".doco-cd.yaml")

	err := createTestFile(t, filePath, dc)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := GetConfigs(repoRoot, ".", "", "main", nil)
	if err != nil {
		t.Fatal(err)
	}

	// First config (main branch) should discover 1, second config (dual branch) should discover 2
	expectedTotal := 4
	if len(configs) != expectedTotal {
		t.Fatalf("expected %d configs, got %d", expectedTotal, len(configs))
	}

	found := 0

	for _, cfg := range configs {
		t.Logf("Discovered config: Name=%s, Reference=%s", cfg.Name, cfg.Reference)

		switch cfg.RepositoryUrl {
		case "https://github.com/kimdre/doco-cd.git":
			if cfg.Name == "test" && cfg.Reference == "main" {
				found++
			}
		case "https://github.com/kimdre/doco-cd_tests.git":
			if (cfg.Name == "app1" || cfg.Name == "app2") && cfg.Reference == "dual" {
				found++
			} else if cfg.Name == "test-deploy" && cfg.Reference == "main" {
				// Name overridden by nested .doco-cd.yaml in the remote repo (was "main-stack")
				found++
			}
		}
	}

	if found != expectedTotal {
		t.Errorf("expected to find %d configs with correct properties, found %d", expectedTotal, found)
	}
}
