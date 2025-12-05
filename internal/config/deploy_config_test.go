package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/validator.v2"

	"github.com/kimdre/doco-cd/internal/filesystem"
)

var projectName = "test"

func createTestFile(fileName string, content string) error {
	err := os.WriteFile(fileName, []byte(content), filesystem.PermOwner)
	if err != nil {
		return err
	}

	return nil
}

func createTmpDir(t *testing.T) string {
	dirName, err := os.MkdirTemp(os.TempDir(), "test-*")
	if err != nil {
		t.Fatal(err)
	}

	return dirName
}

func TestGetDeployConfigs(t *testing.T) {
	t.Run("Valid Config", func(t *testing.T) {
		fileName := ".doco-cd.yaml"
		reference := "refs/heads/test"
		workingDirectory := "/test"
		composeFiles := []string{"test.compose.yaml"}
		customTarget := ""

		deployConfig := fmt.Sprintf(`name: %s
reference: %s
working_dir: %s
compose_files:
  - %s
`, projectName, reference, workingDirectory, composeFiles[0])

		dirName := createTmpDir(t)
		t.Cleanup(func() {
			err := os.RemoveAll(dirName)
			if err != nil {
				t.Fatal(err)
			}
		})

		filePath := filepath.Join(dirName, fileName)

		err := createTestFile(filePath, deployConfig)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetDeployConfigs(dirName, ".", projectName, customTarget, reference)
		if err != nil {
			t.Fatal(err)
		}

		if len(configs) != 1 {
			t.Fatalf("expected 1 config, got %d", len(configs))
		}

		config := configs[0]

		if config.Name != projectName {
			t.Errorf("expected name to be %v, got %s", projectName, config.Name)
		}

		if config.Reference != reference {
			t.Errorf("expected reference to be %v, got %s", reference, config.Reference)
		}

		if config.WorkingDirectory != filepath.Join(".", workingDirectory) {
			t.Errorf("expected working directory to be '%v', got '%s'", workingDirectory, config.WorkingDirectory)
		}

		if !reflect.DeepEqual(config.ComposeFiles, composeFiles) {
			t.Errorf("expected compose files to be %v, got %v", composeFiles, config.ComposeFiles)
		}
	})
}

func TestGetDeployConfigs_DefaultValues(t *testing.T) {
	defaultConfig := DefaultDeployConfig(projectName, DefaultReference)

	dirName := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(dirName)
		if err != nil {
			t.Fatal(err)
		}
	})

	configs, err := GetDeployConfigs(dirName, ".", projectName, "", "")
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	config := configs[0]

	if config.Name != projectName {
		t.Errorf("expected name to be %v, got %s", projectName, config.Name)
	}

	if config.Reference != defaultConfig.Reference {
		t.Errorf("expected reference to be %s, got %s", defaultConfig.Reference, config.Reference)
	}

	if config.WorkingDirectory != defaultConfig.WorkingDirectory {
		t.Errorf("expected working directory to be %s, got %s", defaultConfig.WorkingDirectory, config.WorkingDirectory)
	}

	if !reflect.DeepEqual(config.ComposeFiles, defaultConfig.ComposeFiles) {
		t.Errorf("expected compose files to be %v, got %v", defaultConfig.ComposeFiles, config.ComposeFiles)
	}
}

// TestGetDeployConfigs_DuplicateProjectName checks if the function returns an error
// when there are duplicate project names in the config files.
func TestGetDeployConfigs_DuplicateProjectName(t *testing.T) {
	config := DeployConfig{
		Name:             "test",
		Reference:        "refs/heads/test",
		WorkingDirectory: "/test",
		ComposeFiles:     []string{"test.compose.yaml"},
	}

	configs := []*DeployConfig{&config, &config}

	err := validateUniqueProjectNames(configs)
	if !errors.Is(err, ErrDuplicateProjectName) {
		t.Fatal("expected error for duplicate project names, got nil")
	}
}

// TestGetDeployConfigs_InvalidRepositoryURL checks if the function returns an error when the repository URL is an SSH URL
// The init function panics if the validator for HttpUrl is not registered correctly.
func TestGetDeployConfigs_RepositoryURL(t *testing.T) {
	testCases := []struct {
		name        string
		repoUrl     HttpUrl
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
			expectedErr: fmt.Errorf("RepositoryUrl: %w", ErrInvalidHttpUrl),
		},
		{
			name:        "Invalid SSH URL", // SSH Urls are not supported
			repoUrl:     "git@github.com:kimdre/doco-cd.git",
			expectedErr: fmt.Errorf("RepositoryUrl: %w", ErrInvalidHttpUrl),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			config := DeployConfig{
				Name:          tc.name,
				RepositoryUrl: tc.repoUrl,
			}

			err := validator.Validate(config)
			if err == nil && tc.expectedErr != nil {
				t.Fatalf("expected error %v, got nil", tc.expectedErr)
			}

			if err != nil && strings.Contains(tc.expectedErr.Error(), err.Error()) {
				t.Fatalf("expected error %v, got %v", tc.expectedErr, err)
			}
		})
	}
}

func TestResolveDeployConfigs_InlineOverride(t *testing.T) {
	dirName := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(dirName)
		if err != nil {
			t.Fatal(err)
		}
	})

	poll := PollConfig{
		CloneUrl:    "https://example.com/repo.git",
		Reference:   "refs/heads/main",
		Interval:    60,
		Deployments: []*DeployConfig{{Name: "inline-stack"}},
	}

	// Validate poll config to ensure inline deployments are validated
	if err := poll.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	configs, err := ResolveDeployConfigs(poll, dirName, ".", "repo")
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
	if cfg.Reference != poll.Reference {
		t.Errorf("expected reference to be '%s', got '%s'", poll.Reference, cfg.Reference)
	}

	// Verify defaults applied
	if cfg.WorkingDirectory != "." {
		t.Errorf("expected working directory '.', got '%s'", cfg.WorkingDirectory)
	}

	if len(cfg.ComposeFiles) == 0 {
		t.Errorf("expected default compose files to be set")
	}
}

func TestResolveDeployConfigs_InlineMissingName(t *testing.T) {
	poll := PollConfig{
		CloneUrl:    "https://example.com/repo.git",
		Reference:   "refs/heads/main",
		Interval:    60,
		Deployments: []*DeployConfig{{}}, // Missing name should error
	}

	err := poll.Validate()
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("expected error %v, got %v", ErrInvalidConfig, err)
	}
}

func TestResolveDeployConfigs_InlineAutoDiscover(t *testing.T) {
	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		if err := os.RemoveAll(repoRoot); err != nil {
			t.Fatal(err)
		}
	})

	servicesDir := filepath.Join(repoRoot, "services")
	serviceOneDir := filepath.Join(servicesDir, "service-one")
	serviceTwoDir := filepath.Join(servicesDir, "service-two")

	for _, dir := range []string{serviceOneDir, serviceTwoDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("failed to create service dir %s: %v", dir, err)
		}

		composeFile := filepath.Join(dir, "compose.yaml")
		if err := createTestFile(composeFile, "services:\n  app:\n    image: alpine"); err != nil {
			t.Fatalf("failed to write compose file for %s: %v", dir, err)
		}
	}

	poll := PollConfig{
		CloneUrl:  "https://example.com/repo.git",
		Reference: "refs/heads/main",
		Interval:  60,
		Deployments: []*DeployConfig{
			{WorkingDirectory: "services", AutoDiscover: true},
		},
	}

	if err := poll.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}

	configs, err := ResolveDeployConfigs(poll, repoRoot, ".", projectName)
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

func TestGetDeployConfigs_WithSubdirectory(t *testing.T) {
	fileName := ".doco-cd.yaml"
	reference := "refs/heads/main"
	deployConfigBaseDir := "configs"
	customTarget := ""

	deployConfig := fmt.Sprintf(`name: %s
reference: %s
`, projectName, reference)

	// Create temporary repo root
	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Create subdirectory for configs
	configDir := filepath.Join(repoRoot, deployConfigBaseDir)

	err := os.MkdirAll(configDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	// Create config file in subdirectory
	filePath := filepath.Join(configDir, fileName)

	err = createTestFile(filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	// Test with subdirectory as deployConfigBaseDir
	configs, err := GetDeployConfigs(repoRoot, deployConfigBaseDir, projectName, customTarget, reference)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	config := configs[0]
	if config.Name != projectName {
		t.Errorf("expected name to be %v, got %s", projectName, config.Name)
	}

	if config.Reference != reference {
		t.Errorf("expected reference to be %v, got %s", reference, config.Reference)
	}
}

func TestGetDeployConfigs_WithRootDirectory(t *testing.T) {
	fileName := ".doco-cd.yaml"
	reference := "refs/heads/main"
	deployConfigBaseDir := "."
	customTarget := ""

	deployConfig := fmt.Sprintf(`name: %s
reference: %s
`, projectName, reference)

	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

	filePath := filepath.Join(repoRoot, fileName)

	err := createTestFile(filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	// Test with root directory as deployConfigBaseDir
	configs, err := GetDeployConfigs(repoRoot, deployConfigBaseDir, projectName, customTarget, reference)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	config := configs[0]
	if config.Name != projectName {
		t.Errorf("expected name to be %v, got %s", projectName, config.Name)
	}
}

func TestResolveDeployConfigs_WithSubdirectory(t *testing.T) {
	fileName := ".doco-cd.yaml"
	reference := "refs/heads/main"
	deployConfigBaseDir := "config"

	deployConfig := fmt.Sprintf(`name: %s
reference: %s
`, projectName, reference)

	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

	configDir := filepath.Join(repoRoot, deployConfigBaseDir)

	err := os.MkdirAll(configDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(configDir, fileName)

	err = createTestFile(filePath, deployConfig)
	if err != nil {
		t.Fatal(err)
	}

	poll := PollConfig{
		CloneUrl:  "https://example.com/repo.git",
		Reference: reference,
		Interval:  60,
	}

	configs, err := ResolveDeployConfigs(poll, repoRoot, deployConfigBaseDir, projectName)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Name != projectName {
		t.Errorf("expected name to be %v, got %s", projectName, configs[0].Name)
	}
}

func TestAutoDiscoverDeployments_BasicDiscovery(t *testing.T) {
	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

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

	err = createTestFile(filepath.Join(service1Dir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(filepath.Join(service2Dir, "docker-compose.yml"), "services:\n  db:\n    image: postgres")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &DeployConfig{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml", "docker-compose.yml"},
		AutoDiscover:     true,
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
	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Create a services subdirectory
	servicesDir := filepath.Join(repoRoot, "services")
	service1Dir := filepath.Join(servicesDir, "service1")

	err := os.MkdirAll(service1Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(filepath.Join(service1Dir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &DeployConfig{
		WorkingDirectory: "services",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscover:     true,
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
	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Create nested directories
	level1Dir := filepath.Join(repoRoot, "level1")
	level2Dir := filepath.Join(level1Dir, "level2")
	level3Dir := filepath.Join(level2Dir, "level3")

	err := os.MkdirAll(level3Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	// Create compose files at different levels

	err = createTestFile(filepath.Join(level1Dir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(filepath.Join(level2Dir, "compose.yaml"), "services:\n  db:\n    image: postgres")
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(filepath.Join(level3Dir, "compose.yaml"), "services:\n  cache:\n    image: redis")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &DeployConfig{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscover:     true,
	}
	baseConfig.AutoDiscoverOpts.ScanDepth = 2

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
	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

	// Create subdirectories without compose files
	service1Dir := filepath.Join(repoRoot, "service1")

	err := os.MkdirAll(service1Dir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &DeployConfig{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscover:     true,
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
	repoRoot := createTmpDir(t)
	t.Cleanup(func() {
		err := os.RemoveAll(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
	})

	serviceDir := filepath.Join(repoRoot, "service1")

	err := os.MkdirAll(serviceDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = createTestFile(filepath.Join(serviceDir, "compose.yaml"), "services:\n  web:\n    image: nginx")
	if err != nil {
		t.Fatal(err)
	}

	baseConfig := &DeployConfig{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscover:     true,
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
