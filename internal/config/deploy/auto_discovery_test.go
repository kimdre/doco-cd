package deploy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

func TestConfig_AutoDiscoveryBoolOrObject(t *testing.T) {
	t.Parallel()

	t.Run("yaml bool true uses defaults", func(t *testing.T) {
		t.Parallel()

		filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

		err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
auto_discovery: true
`)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetConfigFromYAML(filePath, true)
		if err != nil {
			t.Fatal(err)
		}

		if !configs[0].AutoDiscovery.Enabled {
			t.Fatal("expected auto_discovery.enabled to be true")
		}

		if configs[0].AutoDiscovery.ScanDepth != 0 {
			t.Fatalf("expected default auto_discovery.depth 0, got %d", configs[0].AutoDiscovery.ScanDepth)
		}

		if !configs[0].AutoDiscovery.Delete {
			t.Fatal("expected default auto_discovery.delete to be true")
		}
	})

	t.Run("yaml object still works", func(t *testing.T) {
		t.Parallel()

		filePath := filepath.Join(t.TempDir(), ".doco-cd.yaml")

		err := createTestFile(t, filePath, `name: test
compose_files: ["compose.yaml"]
auto_discovery:
  enabled: true
  depth: 2
  delete: false
`)
		if err != nil {
			t.Fatal(err)
		}

		configs, err := GetConfigFromYAML(filePath, true)
		if err != nil {
			t.Fatal(err)
		}

		if !configs[0].AutoDiscovery.Enabled {
			t.Fatal("expected auto_discovery.enabled to be true")
		}

		if configs[0].AutoDiscovery.ScanDepth != 2 {
			t.Fatalf("expected auto_discovery.depth 2, got %d", configs[0].AutoDiscovery.ScanDepth)
		}

		if configs[0].AutoDiscovery.Delete {
			t.Fatal("expected auto_discovery.delete to be false")
		}
	})

	t.Run("json bool true uses defaults", func(t *testing.T) {
		t.Parallel()

		var cfg Config
		if err := json.Unmarshal([]byte(`{"name":"test","compose_files":["compose.yaml"],"auto_discovery":true}`), &cfg); err != nil {
			t.Fatal(err)
		}

		if !cfg.AutoDiscovery.Enabled {
			t.Fatal("expected auto_discovery.enabled to be true")
		}

		if cfg.AutoDiscovery.ScanDepth != 0 {
			t.Fatalf("expected default auto_discovery.depth 0, got %d", cfg.AutoDiscovery.ScanDepth)
		}

		if !cfg.AutoDiscovery.Delete {
			t.Fatal("expected default auto_discovery.delete to be true")
		}
	})
}

// ---------------------------------------------------------------------------
// mergeConfig tests
// ---------------------------------------------------------------------------

func TestMergeConfig(t *testing.T) {
	t.Parallel()

	t.Run("MergeExternalSecrets_KeyByKey", func(t *testing.T) {
		t.Parallel()

		base := &Config{
			Name: "base",
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"BASE_SECRET": {LegacyRef: "base-ref"},
			},
		}
		override := &Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"OVERRIDE_SECRET": {LegacyRef: "override-ref"},
			},
		}

		mergeConfig(base, override)

		if base.ExternalSecrets["BASE_SECRET"].LegacyRef != "base-ref" {
			t.Error("base key should be preserved")
		}

		if base.ExternalSecrets["OVERRIDE_SECRET"].LegacyRef != "override-ref" {
			t.Error("override key should be merged in")
		}
	})

	t.Run("MergeExternalSecrets_OverrideWinsOnCollision", func(t *testing.T) {
		t.Parallel()

		base := &Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"SECRET": {LegacyRef: "base-ref"},
			},
		}
		override := &Config{
			ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
				"SECRET": {LegacyRef: "override-ref"},
			},
		}

		mergeConfig(base, override)

		if base.ExternalSecrets["SECRET"].LegacyRef != "override-ref" {
			t.Errorf("override value should win, got %q", base.ExternalSecrets["SECRET"].LegacyRef)
		}
	})

	t.Run("MergeEnvironment_KeyByKey", func(t *testing.T) {
		t.Parallel()

		base := &Config{
			Environment: map[string]string{"BASE_VAR": "base"},
		}
		override := &Config{
			Environment: map[string]string{"OVERRIDE_VAR": "override"},
		}

		mergeConfig(base, override)

		if base.Environment["BASE_VAR"] != "base" {
			t.Error("base env var should be preserved")
		}

		if base.Environment["OVERRIDE_VAR"] != "override" {
			t.Error("override env var should be merged")
		}
	})

	t.Run("MergeBuildArgs_KeyByKey", func(t *testing.T) {
		t.Parallel()

		base := &Config{}
		base.BuildOpts.Args = map[string]string{"BASE_ARG": "base"}

		override := &Config{}
		override.BuildOpts.Args = map[string]string{"OVERRIDE_ARG": "override"}

		mergeConfig(base, override)

		if base.BuildOpts.Args["BASE_ARG"] != "base" {
			t.Error("base build arg should be preserved")
		}

		if base.BuildOpts.Args["OVERRIDE_ARG"] != "override" {
			t.Error("override build arg should be merged")
		}
	})

	t.Run("MergeSlice_ReplacedWhenNonEmpty", func(t *testing.T) {
		t.Parallel()

		base := &Config{Profiles: []string{"base-profile"}}
		override := &Config{Profiles: []string{"override-profile"}}

		mergeConfig(base, override)

		if len(base.Profiles) != 1 || base.Profiles[0] != "override-profile" {
			t.Errorf("profiles should be replaced, got %v", base.Profiles)
		}
	})

	t.Run("MergeSlice_UnchangedWhenEmpty", func(t *testing.T) {
		t.Parallel()

		base := &Config{Profiles: []string{"base-profile"}}
		override := &Config{} // no profiles set

		mergeConfig(base, override)

		if len(base.Profiles) != 1 || base.Profiles[0] != "base-profile" {
			t.Errorf("profiles should be unchanged, got %v", base.Profiles)
		}
	})

	t.Run("MergeScalar_Timeout", func(t *testing.T) {
		t.Parallel()

		base := &Config{Timeout: 180}
		override := &Config{Timeout: 60}

		mergeConfig(base, override)

		if base.Timeout != 60 {
			t.Errorf("timeout should be overridden to 60, got %d", base.Timeout)
		}
	})

	t.Run("MergeScalar_Name", func(t *testing.T) {
		t.Parallel()

		base := &Config{Name: "base-name"}
		override := &Config{Name: "override-name"}

		mergeConfig(base, override)

		if base.Name != "override-name" {
			t.Errorf("name should be overridden, got %q", base.Name)
		}
	})

	t.Run("ProtectedFields_NotOverridden", func(t *testing.T) {
		t.Parallel()

		base := &Config{
			Reference:     "refs/heads/main",
			RepositoryUrl: "https://example.com/base.git",
			GitDepth:      5,
		}
		base.AutoDiscovery.ScanDepth = 3

		override := &Config{
			Reference:     "refs/heads/other",
			RepositoryUrl: "https://example.com/override.git",
			GitDepth:      99,
		}
		override.AutoDiscovery.ScanDepth = 99

		mergeConfig(base, override)

		if base.Reference != "refs/heads/main" {
			t.Errorf("Reference should not be overridden, got %q", base.Reference)
		}

		if base.RepositoryUrl != "https://example.com/base.git" {
			t.Errorf("RepositoryUrl should not be overridden, got %q", base.RepositoryUrl)
		}

		if base.GitDepth != 5 {
			t.Errorf("GitDepth should not be overridden, got %d", base.GitDepth)
		}

		if base.AutoDiscovery.ScanDepth != 3 {
			t.Errorf("AutoDiscovery.ScanDepth should not be overridden, got %d", base.AutoDiscovery.ScanDepth)
		}
	})

	t.Run("MergeReconciliation_NestedStruct", func(t *testing.T) {
		t.Parallel()

		base := &Config{}
		base.Reconciliation.RestartLimit = 5
		base.Reconciliation.RestartWindow = 300

		override := &Config{}
		override.Reconciliation.RestartLimit = 10

		mergeConfig(base, override)

		if base.Reconciliation.RestartLimit != 10 {
			t.Errorf("RestartLimit should be overridden to 10, got %d", base.Reconciliation.RestartLimit)
		}

		if base.Reconciliation.RestartWindow != 300 {
			t.Errorf("RestartWindow should remain 300, got %d", base.Reconciliation.RestartWindow)
		}
	})
}

// ---------------------------------------------------------------------------
// autoDiscoverDeployments with nested config tests
// ---------------------------------------------------------------------------

func TestAutoDiscoverDeployments_WithNestedConfig(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	serviceDir := filepath.Join(repoRoot, "service1")

	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  web:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	// Write a nested .doco-cd.yaml in service1/ that adds external secrets
	nestedCfg := `external_secrets:
  MY_SECRET: "op://vault/item/field"
environment:
  EXTRA_VAR: "hello"
`
	if err := createTestFile(t, filepath.Join(serviceDir, ".doco-cd.yaml"), nestedCfg); err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
		ExternalSecrets: map[string]secrettypes.ExternalSecretRef{
			"BASE_SECRET": {LegacyRef: "base-ref"},
		},
	}

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	cfg := configs[0]

	// base secret should be preserved
	if cfg.ExternalSecrets["BASE_SECRET"].LegacyRef != "base-ref" {
		t.Errorf("base secret should be preserved, got %q", cfg.ExternalSecrets["BASE_SECRET"].LegacyRef)
	}

	// nested secret should be merged in
	if cfg.ExternalSecrets["MY_SECRET"].LegacyRef != "op://vault/item/field" {
		t.Errorf("nested secret should be merged, got %q", cfg.ExternalSecrets["MY_SECRET"].LegacyRef)
	}

	// nested environment should be merged in
	if cfg.Environment["EXTRA_VAR"] != "hello" {
		t.Errorf("nested env var should be merged, got %q", cfg.Environment["EXTRA_VAR"])
	}
}

func TestAutoDiscoverDeployments_WithNestedConfig_EnvironmentOnly_DoesNotOverrideComposeFiles(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	serviceDir := filepath.Join(repoRoot, "service1")

	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "test.compose.yaml"), "services:\n  web:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, ".doco-cd.yml"), "environment:\n  SUB: nested\n"); err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"test.compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
		Environment:      map[string]string{"BASE": "root"},
	}

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	cfg := configs[0]

	if cfg.Name != "service1" {
		t.Errorf("expected discovered name 'service1', got %q", cfg.Name)
	}

	if cfg.WorkingDirectory != "service1" {
		t.Errorf("expected working directory 'service1', got %q", cfg.WorkingDirectory)
	}

	if !reflect.DeepEqual(cfg.ComposeFiles, []string{"test.compose.yaml"}) {
		t.Errorf("expected compose_files to remain [test.compose.yaml], got %v", cfg.ComposeFiles)
	}

	if cfg.Environment["BASE"] != "root" {
		t.Errorf("expected base env BASE=root to be preserved, got %q", cfg.Environment["BASE"])
	}

	if cfg.Environment["SUB"] != "nested" {
		t.Errorf("expected nested env SUB=nested to be merged, got %q", cfg.Environment["SUB"])
	}
}

func TestAutoDiscoverDeployments_NestedConfig_MultipleDocumentsError(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	serviceDir := filepath.Join(repoRoot, "service1")

	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  web:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	// Two YAML documents in the nested config – should error
	multiDoc := `external_secrets:
  SECRET1: ref1
---
external_secrets:
  SECRET2: ref2
`
	if err := createTestFile(t, filepath.Join(serviceDir, ".doco-cd.yaml"), multiDoc); err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}

	_, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err == nil {
		t.Fatal("expected error for multiple YAML documents in nested config, got nil")
	}

	if !errors.Is(err, ErrMultipleYAMLDocuments) {
		t.Errorf("expected ErrMultipleYAMLDocuments, got %v", err)
	}
}

func TestAutoDiscoverDeployments_NoNestedConfig_BackwardsCompatible(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()
	serviceDir := filepath.Join(repoRoot, "myservice")

	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  web:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
		Timeout:          300,
	}

	configs, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	if configs[0].Timeout != 300 {
		t.Errorf("expected timeout 300 from base config, got %d", configs[0].Timeout)
	}

	if configs[0].Name != "myservice" {
		t.Errorf("expected name 'myservice', got %q", configs[0].Name)
	}
}

func TestAutoDiscoverDeployments_SkipHeavyDirectories(t *testing.T) {
	t.Parallel()

	resetAutoDiscoveryCache()

	repoRoot := t.TempDir()

	if err := os.MkdirAll(filepath.Join(repoRoot, ".git", "objects"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, "node_modules", "pkg"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, "service1"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, ".git", "compose.yaml"), "services:\n  gitservice:\n    image: busybox"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "node_modules", "pkg", "compose.yaml"), "services:\n  dep:\n    image: busybox"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "service1", "compose.yaml"), "services:\n  app:\n    image: nginx"); err != nil {
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

	if len(configs) != 1 {
		t.Fatalf("expected 1 discovered config, got %d", len(configs))
	}

	if configs[0].Name != "service1" {
		t.Fatalf("expected discovered stack 'service1', got %q", configs[0].Name)
	}
}

func TestAutoDiscoverDeployments_CacheKeyedByHeadAndSettings(t *testing.T) {
	t.Parallel()

	resetAutoDiscoveryCache()

	repoRoot := t.TempDir()

	repo, err := git.PlainInit(repoRoot, false)
	if err != nil {
		t.Fatal(err)
	}

	serviceDir := filepath.Join(repoRoot, "service1")
	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  app:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, repo, "initial"); err != nil {
		t.Fatal(err)
	}

	baseConfig := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}

	first, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != 1 {
		t.Fatalf("expected 1 discovered config on first scan, got %d", len(first))
	}

	if err := os.Remove(filepath.Join(serviceDir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}

	second, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(second) != 1 {
		t.Fatalf("expected cached result with 1 config when HEAD is unchanged, got %d", len(second))
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  app:\n    image: nginx:alpine"); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, "service2"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "service2", "compose.yaml"), "services:\n  app2:\n    image: busybox"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, repo, "add service2"); err != nil {
		t.Fatal(err)
	}

	third, err := autoDiscoverDeployments(repoRoot, baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(third) != 2 {
		t.Fatalf("expected cache invalidation after HEAD change, got %d configs", len(third))
	}
}

func resetAutoDiscoveryCache() {
	autoDiscoveryCache.mu.Lock()
	defer autoDiscoveryCache.mu.Unlock()

	autoDiscoveryCache.entries = map[string][]*Config{}
}

func commitAll(t *testing.T, repo *git.Repository, message string) error {
	t.Helper()

	wt, err := repo.Worktree()
	if err != nil {
		return err
	}

	if err := wt.AddGlob("."); err != nil {
		return err
	}

	_, err = wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "test",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})

	return err
}
