package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kimdre/doco-cd/internal/encryption"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/source/store"
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

		if configs[0].AutoDiscovery.Delete {
			t.Fatal("expected default auto_discovery.delete to be false")
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

		if cfg.AutoDiscovery.Delete {
			t.Fatal("expected default auto_discovery.delete to be false")
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

	t.Run("MergeSwarmEnabled_NestedStruct", func(t *testing.T) {
		t.Parallel()

		base := &Config{Swarm: SwarmConfig{Enabled: new(true)}}
		override := &Config{Swarm: SwarmConfig{Enabled: new(false)}}

		mergeConfig(base, override)

		if base.Swarm.Enabled == nil || *base.Swarm.Enabled {
			t.Fatalf("expected nested swarm.enabled override to be false, got %v", base.Swarm.Enabled)
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

	configs, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
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

	configs, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
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

	_, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
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

	configs, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
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

	configs, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
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

func TestAutoDiscoverDeployments_DiskScanTracksChangesWithoutTreeIdentity(t *testing.T) {
	t.Parallel()

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

	first, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != 1 {
		t.Fatalf("expected 1 discovered config on first scan, got %d", len(first))
	}

	if err := os.Remove(filepath.Join(serviceDir, "compose.yaml")); err != nil {
		t.Fatal(err)
	}

	second, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(second) != 0 {
		t.Fatalf("disk scan must not reuse a stale Git HEAD result, got %d", len(second))
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  app:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	baseConfig.Swarm.Enabled = new(false)

	modeChanged, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(modeChanged) != 1 || modeChanged[0].Swarm.Enabled == nil || *modeChanged[0].Swarm.Enabled {
		t.Fatalf("expected swarm.enabled change to invalidate the cache, got %#v", modeChanged)
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

	third, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), baseConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(third) != 2 {
		t.Fatalf("expected cache invalidation after HEAD change, got %d configs", len(third))
	}
}

func TestAutoDiscoverDeployments_CacheSeparatesFullBaseConfig(t *testing.T) {
	t.Parallel()

	repoRoot := t.TempDir()

	repo, err := git.PlainInit(repoRoot, false)
	if err != nil {
		t.Fatal(err)
	}

	serviceDir := filepath.Join(repoRoot, "service")
	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services:\n  app:\n    image: nginx"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, repo, "initial"); err != nil {
		t.Fatal(err)
	}

	productionConfig := &Config{
		Context:          "production",
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}

	production, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), productionConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(production) != 1 || production[0].Context != "production" {
		t.Fatalf("expected production config, got %#v", production)
	}

	nasConfig := &Config{
		Context:          "nas",
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}

	nas, err := autoDiscoverDeployments(os.DirFS(repoRoot), repoRoot, revisionKeyForRepoRoot(repoRoot), nasConfig)
	if err != nil {
		t.Fatal(err)
	}

	if len(nas) != 1 || nas[0].Context != "nas" {
		t.Fatalf("expected nas config rather than a cached production config, got %#v", nas)
	}
}

type countingDiscoveryFS struct {
	fs.FS
	reads map[string]int
}

func (c *countingDiscoveryFS) ReadDir(name string) ([]fs.DirEntry, error) {
	c.reads[name]++

	return fs.ReadDir(c.FS, name)
}

type countingDiscoveryTreeFS struct {
	*gitInternal.TreeFS
	reads map[string]int
}

func (c *countingDiscoveryTreeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	c.reads[name]++

	return c.TreeFS.ReadDir(name)
}

func TestAutoDiscoverDeployments_ReadsEachVisitedDirectoryOnce(t *testing.T) {
	t.Parallel()

	fsys := &countingDiscoveryFS{
		FS: fstest.MapFS{
			"compose.yaml":                  {Data: []byte("services: {}")},
			"alpha/compose.yaml":            {Data: []byte("services: {}")},
			"alpha/.doco-cd.yaml":           {Data: []byte("environment:\n  SOURCE: yaml")},
			"alpha/.doco-cd.yml":            {Data: []byte("environment:\n  SOURCE: yml")},
			"alpha/child/compose.yaml":      {Data: []byte("services: {}")},
			"alpha/child/deep/compose.yaml": {Data: []byte("services: {}")},
			"beta/compose.yaml":             {Data: []byte("services: {}")},
			".git/compose.yaml":             {Data: []byte("services: {}")},
			"node_modules/compose.yaml":     {Data: []byte("services: {}")},
		},
		reads: make(map[string]int),
	}
	base := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true, ScanDepth: 2},
	}

	configs, err := autoDiscoverDeployments(fsys, "/some/repository", "any-revision", base)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, cfg := range configs {
		names = append(names, cfg.Name)
	}

	if !reflect.DeepEqual(names, []string{"repository", "alpha", "child", "beta"}) {
		t.Fatalf("unexpected traversal order or inventory: %v", names)
	}

	if configs[1].Environment["SOURCE"] != "yaml" {
		t.Fatalf(".yaml must take precedence over .yml: %#v", configs[1].Environment)
	}

	if want := map[string]int{".": 1, "alpha": 1, "alpha/child": 1, "beta": 1}; !reflect.DeepEqual(fsys.reads, want) {
		t.Fatalf("expected one ReadDir per visited directory, got %v, want %v", fsys.reads, want)
	}

	base.WorkingDirectory = ".git"

	ignoredRoot, err := autoDiscoverDeployments(fsys, "/some/repository", "", base)
	if err != nil || len(ignoredRoot) != 1 || ignoredRoot[0].WorkingDirectory != ".git" || fsys.reads[".git"] != 1 {
		t.Fatalf("explicitly selected ignored root must be scanned: configs %v, reads %v, err %v", ignoredRoot, fsys.reads, err)
	}
}

func TestAutoDiscoverDeployments_ReportsParentValidationBeforeChildParse(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"compose.yaml":           {Data: []byte("services: {}")},
		"child/compose.yaml":     {Data: []byte("services: {}")},
		"child/.doco-cd.yaml":    {Data: []byte("environment: [invalid")},
		"child/.doco-cd.yml":     {Data: []byte("environment:\n  SOURCE: fallback")},
		"child/deep/compose.yml": {Data: []byte("services: {}")},
	}
	base := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
		GitDepth:         -1,
	}

	_, err := autoDiscoverDeployments(fsys, "/some/repository", "", base)
	if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "git_depth") {
		t.Fatalf("parent validation must precede child YAML error, got %v", err)
	}

	base.GitDepth = 0

	_, err = autoDiscoverDeployments(fsys, "/some/repository", "", base)
	if err == nil || !strings.Contains(err.Error(), "child/.doco-cd.yaml") {
		t.Fatalf("child YAML error must name preferred .yaml file, got %v", err)
	}
}

func TestAutoDiscoverDeployments_TreeSubtreesAcrossRevisions(t *testing.T) {
	repoRoot := t.TempDir()

	repo, err := git.PlainInit(repoRoot, false)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alpha", "beta", "old"} {
		if err := os.MkdirAll(filepath.Join(repoRoot, name), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := createTestFile(t, filepath.Join(repoRoot, name, "compose.yaml"), "services: {}"); err != nil {
			t.Fatal(err)
		}
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "alpha", ".doco-cd.yaml"), "environment:\n  SOURCE: alpha\n"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "beta", ".doco-cd.yaml"), "environment:\n  SOURCE: old\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, repo, "first"); err != nil {
		t.Fatal(err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	tree, err := gitInternal.NewTreeFSAtCommit(repo, head.Hash())
	if err != nil {
		t.Fatal(err)
	}

	base := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}
	base.Internal.ConfigSourceRevision = "first"
	base.Internal.ConfigSourceWorkingDir = "/artifact/first"
	firstFS := &countingDiscoveryTreeFS{TreeFS: tree, reads: make(map[string]int)}

	first, err := autoDiscoverDeployments(firstFS, repoRoot, head.Hash().String(), base)
	if err != nil {
		t.Fatal(err)
	}

	if len(first) != 3 || len(firstFS.reads) != 4 {
		t.Fatalf("expected initial scan of root and three services, got %d services and reads %v", len(first), firstFS.reads)
	}

	if err := os.Rename(filepath.Join(repoRoot, "alpha"), filepath.Join(repoRoot, "zeta")); err != nil {
		t.Fatal(err)
	}

	if err := os.RemoveAll(filepath.Join(repoRoot, "old")); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "beta", ".doco-cd.yaml"), "environment:\n  SOURCE: updated\n"); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(repoRoot, "new"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "new", "compose.yaml"), "services: {}"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, repo, "second"); err != nil {
		t.Fatal(err)
	}

	head, err = repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	tree, err = gitInternal.NewTreeFSAtCommit(repo, head.Hash())
	if err != nil {
		t.Fatal(err)
	}

	base.Context = "new-context"
	base.Internal.ConfigSourceRevision = "second"
	base.Internal.ConfigSourceWorkingDir = "/artifact/second"
	secondFS := &countingDiscoveryTreeFS{TreeFS: tree, reads: make(map[string]int)}

	second, err := autoDiscoverDeployments(secondFS, repoRoot, head.Hash().String(), base)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, cfg := range second {
		names = append(names, cfg.Name)
		if cfg.Context != "new-context" || cfg.Internal.ConfigSourceRevision != "second" ||
			cfg.Internal.ConfigSourceWorkingDir != "/artifact/second" {
			t.Fatalf("cached subtree carried stale base config or internal metadata: %+v", cfg)
		}
	}

	if !reflect.DeepEqual(names, []string{"beta", "new", "zeta"}) {
		t.Fatalf("renamed, added or deleted service produced stale inventory: %v", names)
	}

	if second[0].Environment["SOURCE"] != "updated" || second[2].Environment["SOURCE"] != "alpha" ||
		second[2].WorkingDirectory != "zeta" {
		t.Fatalf("unexpected nested overrides or renamed path: %+v", second)
	}

	if want := map[string]int{".": 1, "beta": 1}; !reflect.DeepEqual(secondFS.reads, want) {
		t.Fatalf("unchanged renamed and identical added subtrees should be reused without ReadDir, got %v, want %v", secondFS.reads, want)
	}

	second[2].Environment["SOURCE"] = "caller-modified"
	base.Internal.ConfigSourceRevision = "same-tree-new-base"
	// Obsolete-stack cleanup reads these from every discovered config.
	base.AutoDiscovery.Delete = true
	base.AutoDiscovery.RemoveVolumes = true
	base.AutoDiscovery.RemoveImages = false
	repeatedFS := &countingDiscoveryTreeFS{TreeFS: tree, reads: make(map[string]int)}

	repeated, err := autoDiscoverDeployments(repeatedFS, repoRoot, head.Hash().String(), base)
	if err != nil || len(repeated) != 3 || len(repeatedFS.reads) != 0 ||
		repeated[2].Environment["SOURCE"] != "alpha" ||
		repeated[2].Internal.ConfigSourceRevision != "same-tree-new-base" {
		t.Fatalf("cached root must rehydrate independently: configs %+v, reads %v, err %v", repeated, repeatedFS.reads, err)
	}

	for _, cfg := range repeated {
		if cfg.AutoDiscovery != base.AutoDiscovery {
			t.Fatalf("cached subtree carried stale auto-discovery cleanup settings: got %+v, want %+v", cfg.AutoDiscovery, base.AutoDiscovery)
		}
	}

	base.ComposeFiles = []string{"other.yaml"}
	settingsFS := &countingDiscoveryTreeFS{TreeFS: tree, reads: make(map[string]int)}

	changedSettings, err := autoDiscoverDeployments(settingsFS, repoRoot, head.Hash().String(), base)
	if err != nil {
		t.Fatal(err)
	}

	if len(changedSettings) != 0 || len(settingsFS.reads) != 4 {
		t.Fatalf("compose settings must invalidate subtree matches: %d matches, reads %v", len(changedSettings), settingsFS.reads)
	}

	noRevisionFS := &countingDiscoveryTreeFS{TreeFS: tree, reads: make(map[string]int)}

	_, err = autoDiscoverDeployments(noRevisionFS, repoRoot, "", base)
	if err != nil || len(noRevisionFS.reads) != 4 {
		t.Fatalf("missing revision must scan fully: reads %v, err %v", noRevisionFS.reads, err)
	}
}

type failingDiscoveryTreeFS struct {
	*gitInternal.TreeFS
	fail string
}

func (f *failingDiscoveryTreeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.fail {
		return nil, fs.ErrPermission
	}

	return f.TreeFS.ReadDir(name)
}

// Obsolete-stack cleanup treats every stack missing from the inventory as
// deleted, so a failed read after cached subtrees must never yield a partial one.
func TestAutoDiscoverDeployments_TreeReadFailureReturnsNoInventory(t *testing.T) {
	repoRoot := t.TempDir()

	repo, err := git.PlainInit(repoRoot, false)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(repoRoot, name), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := createTestFile(t, filepath.Join(repoRoot, name, "compose.yaml"), "services: {}"); err != nil {
			t.Fatal(err)
		}
	}

	if err := commitAll(t, repo, "first"); err != nil {
		t.Fatal(err)
	}

	base := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true, Delete: true},
	}

	treeAt := func() (*gitInternal.TreeFS, string) {
		head, err := repo.Head()
		if err != nil {
			t.Fatal(err)
		}

		tree, err := gitInternal.NewTreeFSAtCommit(repo, head.Hash())
		if err != nil {
			t.Fatal(err)
		}

		return tree, head.Hash().String()
	}

	tree, revision := treeAt()
	if configs, err := autoDiscoverDeployments(tree, repoRoot, revision, base); err != nil || len(configs) != 2 {
		t.Fatalf("warm scan: configs %d, err %v", len(configs), err)
	}

	if err := createTestFile(t, filepath.Join(repoRoot, "beta", "compose.yaml"), "services:\n  web: {}\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, repo, "second"); err != nil {
		t.Fatal(err)
	}

	tree, revision = treeAt()

	configs, err := autoDiscoverDeployments(&failingDiscoveryTreeFS{TreeFS: tree, fail: "beta"}, repoRoot, revision, base)
	if !errors.Is(err, fs.ErrPermission) || configs != nil {
		t.Fatalf("failed subtree read must fail discovery without inventory: configs %v, err %v", configs, err)
	}

	configs, err = autoDiscoverDeployments(tree, repoRoot, revision, base)
	if err != nil || len(configs) != 2 {
		t.Fatalf("a failed scan must not poison the cache: configs %d, err %v", len(configs), err)
	}
}

func TestAutoDiscoveryCache_BoundedLRU(t *testing.T) {
	cache := discoveryCache{}
	key := func(i int) discoveryCacheKey { return discoveryCacheKey{tree: strconv.Itoa(i)} }

	for i := range maxAutoDiscoveryCacheEntries {
		cache.put(key(i), nil)
	}

	if _, ok := cache.get(key(0)); !ok {
		t.Fatal("expected oldest entry before eviction")
	}

	cache.put(key(maxAutoDiscoveryCacheEntries), nil)

	if _, ok := cache.get(key(1)); ok {
		t.Fatal("least recently used entry was not evicted")
	}

	if _, ok := cache.get(key(0)); !ok {
		t.Fatal("recently used entry was evicted")
	}

	if len(cache.entries) != maxAutoDiscoveryCacheEntries || cache.bytes > maxAutoDiscoveryCacheBytes {
		t.Fatalf("cache entry or byte budget exceeded: entries=%d bytes=%d", len(cache.entries), cache.bytes)
	}

	cache.put(key(-1), []discoveryMatch{{dir: "too-large", overrideSize: maxAutoDiscoveryCacheEntryBytes}})

	if _, ok := cache.get(key(-1)); ok {
		t.Fatal("oversized entry should never be cached")
	}

	for i := range maxAutoDiscoveryCacheEntries {
		cache.put(key(i+maxAutoDiscoveryCacheEntries+1), []discoveryMatch{{dir: "service", overrideSize: 96 << 10}})
	}

	if cache.bytes > maxAutoDiscoveryCacheBytes || len(cache.entries) >= maxAutoDiscoveryCacheEntries {
		t.Fatalf("cache contents were not bounded: entries=%d bytes=%d", len(cache.entries), cache.bytes)
	}
}

func TestAutoDiscoveryCache_Concurrent(t *testing.T) {
	t.Parallel()

	cache := discoveryCache{}

	var workers sync.WaitGroup

	for i := range 12 {
		workers.Go(func() {
			for j := range 64 {
				key := discoveryCacheKey{tree: strconv.Itoa(i*64 + j)}
				cache.put(key, []discoveryMatch{{dir: "service"}})
				cache.get(key)
			}
		})
	}

	workers.Wait()

	if len(cache.entries) > maxAutoDiscoveryCacheEntries || cache.bytes > maxAutoDiscoveryCacheBytes {
		t.Fatalf("concurrent cache access exceeded limits: entries=%d bytes=%d", len(cache.entries), cache.bytes)
	}
}

func TestAutoDiscoveryProofCache_BoundedLRU(t *testing.T) {
	cache := discoveryProofCache{}
	key := func(i int) discoveryProofKey {
		return discoveryProofKey{tree: plumbing.NewHash(fmt.Sprintf("%040x", i))}
	}

	for i := range maxAutoDiscoveryProofEntries {
		cache.put(key(i))
	}

	if !cache.get(key(0)) {
		t.Fatal("expected first proof before eviction")
	}

	cache.put(key(maxAutoDiscoveryProofEntries))

	if cache.get(key(1)) || !cache.get(key(0)) || len(cache.entries) != maxAutoDiscoveryProofEntries {
		t.Fatal("proof cache did not evict the least recently used entry")
	}
}

func TestPlainGitDiscoveryTree_ReusesUnchangedArtifactDirectories(t *testing.T) {
	sourceRoot := t.TempDir()

	source, err := git.PlainInit(sourceRoot, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(sourceRoot, "service"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "service", "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "service", ".doco-cd.yaml"),
		"environment:\n  SOURCE: plaintext\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "plain"); err != nil {
		t.Fatal(err)
	}

	verify := func() map[string]int {
		t.Helper()

		head, headErr := source.Head()
		if headErr != nil {
			t.Fatal(headErr)
		}

		tree, treeErr := gitInternal.NewTreeFSAtCommit(source, head.Hash())
		if treeErr != nil {
			t.Fatal(treeErr)
		}

		artifactRoot := t.TempDir()
		if exportErr := gitInternal.ExportTree(artifactRoot, source, head.Hash(), gitInternal.ExportOptions{}); exportErr != nil {
			t.Fatal(exportErr)
		}

		disk := &countingDiscoveryFS{FS: os.DirFS(artifactRoot), reads: make(map[string]int)}
		if !plainGitDiscoveryTree(tree, disk, sourceRoot, &Config{WorkingDirectory: "."}) {
			t.Fatal("plain exported Git artifact should match its tree")
		}

		return disk.reads
	}

	if reads := verify(); !reflect.DeepEqual(reads, map[string]int{".": 1, "service": 1}) {
		t.Fatalf("initial proof must read each artifact directory once, got %v", reads)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "README.md"), "unrelated change"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "unrelated"); err != nil {
		t.Fatal(err)
	}

	if reads := verify(); !reflect.DeepEqual(reads, map[string]int{".": 1}) {
		t.Fatalf("cross-revision proof should only read changed ancestor, got %v", reads)
	}

	if reads := verify(); len(reads) != 0 {
		t.Fatalf("same revision's proven root should need no disk directory reads, got %v", reads)
	}
}

func TestPlainGitDiscoveryTree_RejectsEncryptedConfigWithoutCompose(t *testing.T) {
	sourceRoot := t.TempDir()

	repo, err := git.PlainInit(sourceRoot, false)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(sourceRoot, "idle"), 0o750); err != nil {
		t.Fatal(err)
	}

	ciphertext, err := os.ReadFile(filepath.Join("..", "..", "encryption", "testdata", "encrypted.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "idle", ".doco-cd.yaml"), string(ciphertext)); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, repo, "encrypted config without compose"); err != nil {
		t.Fatal(err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	tree, err := gitInternal.NewTreeFSAtCommit(repo, head.Hash())
	if err != nil {
		t.Fatal(err)
	}

	artifactRoot := t.TempDir()
	if err := gitInternal.ExportTree(artifactRoot, repo, head.Hash(), gitInternal.ExportOptions{}); err != nil {
		t.Fatal(err)
	}

	if plainGitDiscoveryTree(tree, os.DirFS(artifactRoot), sourceRoot, &Config{WorkingDirectory: "."}) {
		t.Fatal("encrypted nested config must block proof even in a no-match subtree")
	}
}

func TestPlainGitDiscoveryTree_RespectsScanBoundary(t *testing.T) {
	sourceRoot := t.TempDir()

	repo, err := git.PlainInit(sourceRoot, false)
	if err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"work/stack/deep", "work/node_modules/heavy", "outside/other"} {
		if err := os.MkdirAll(filepath.Join(sourceRoot, dir), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := createTestFile(t, filepath.Join(sourceRoot, dir, "compose.yaml"), "services: {}\n"); err != nil {
			t.Fatal(err)
		}
	}

	if err := commitAll(t, repo, "discovery boundaries"); err != nil {
		t.Fatal(err)
	}

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	tree, err := gitInternal.NewTreeFSAtCommit(repo, head.Hash())
	if err != nil {
		t.Fatal(err)
	}

	artifact := t.TempDir()
	if err := gitInternal.ExportTree(artifact, repo, head.Hash(), gitInternal.ExportOptions{}); err != nil {
		t.Fatal(err)
	}

	checkReads := func(depth int, want map[string]int) {
		t.Helper()

		disk := &countingDiscoveryFS{FS: os.DirFS(artifact), reads: make(map[string]int)}

		base := &Config{WorkingDirectory: "work", AutoDiscovery: AutoDiscoveryConfig{ScanDepth: depth}}
		if !plainGitDiscoveryTree(tree, disk, sourceRoot, base) {
			t.Fatal("expected matching published tree and disk inventory")
		}

		if !reflect.DeepEqual(disk.reads, want) {
			t.Fatalf("proof reads for depth %d: got %v, want %v", depth, disk.reads, want)
		}
	}
	checkReads(1, map[string]int{".": 1, "work": 1, "work/stack": 1})
	checkReads(2, map[string]int{".": 1, "work": 1, "work/stack": 1, "work/stack/deep": 1})
}

func TestGetConfigs_PrimaryPublishedGitArtifactReusesSubtrees(t *testing.T) {
	sourceRoot := t.TempDir()

	source, err := git.PlainInitWithOptions(sourceRoot, &git.PlainInitOptions{
		DefaultBranch: DefaultReference,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, ".doco-cd.yaml"),
		"compose_files: [compose.yaml]\nauto_discovery: true\n"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alpha", "beta"} {
		if err := os.MkdirAll(filepath.Join(sourceRoot, name), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := createTestFile(t, filepath.Join(sourceRoot, name, "compose.yaml"), "services: {}\n"); err != nil {
			t.Fatal(err)
		}
	}

	if err := commitAll(t, source, "initial"); err != nil {
		t.Fatal(err)
	}

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        sourceRoot,
		BaseDir:         t.TempDir(),
		CloneSubmodules: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	lookup := func() ([]*Config, map[string]int) {
		t.Helper()

		revision, resolveErr := gitStore.Resolve(context.Background(), "main")
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}

		artifact, publishErr := gitStore.Publish(context.Background(), revision)
		if publishErr != nil {
			t.Fatal(publishErr)
		}

		counts := make(map[string]int)

		autoDiscoveryCacheObserverMu.RLock()

		previousObserver := autoDiscoveryCacheObserver

		autoDiscoveryCacheObserverMu.RUnlock()

		SetAutoDiscoveryCacheObserver(func(_, result string) { counts[result]++ })
		defer SetAutoDiscoveryCacheObserver(previousObserver)

		configs, discoverErr := GetConfigs(context.Background(), artifact.Path, ".", "", "main",
			gitStore.MirrorDir(), string(revision), &GitOptions{GitCloneSubmodules: true})
		if discoverErr != nil {
			t.Fatal(discoverErr)
		}

		return configs, counts
	}

	first, initialCounts := lookup()
	if len(first) != 2 || initialCounts["miss"] != 2 || initialCounts["hit"] != 1 || initialCounts["bypass"] != 0 {
		t.Fatalf("initial published scan should use object tree: configs=%v, counts=%v", first, initialCounts)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "README.md"), "unrelated revision"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "unrelated"); err != nil {
		t.Fatal(err)
	}

	second, nextCounts := lookup()
	if len(second) != 2 || nextCounts["hit"] < 2 || nextCounts["miss"] != 1 || nextCounts["bypass"] != 0 {
		t.Fatalf("unchanged services should be reused across Git revisions: configs=%v, counts=%v", second, nextCounts)
	}

	repeated, repeatCounts := lookup()
	if len(repeated) != 2 || repeatCounts["hit"] != 1 || repeatCounts["miss"] != 0 {
		t.Fatalf("same published revision should reuse the cached root: configs=%v, counts=%v", repeated, repeatCounts)
	}

	if err := os.Rename(filepath.Join(sourceRoot, "beta"), filepath.Join(sourceRoot, "zeta")); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "alpha", ".doco-cd.yaml"),
		"environment:\n  SOURCE: updated\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "renamed and changed nested config"); err != nil {
		t.Fatal(err)
	}

	changed, changedCounts := lookup()
	if len(changed) != 2 || changed[0].Name != "alpha" || changed[0].Environment["SOURCE"] != "updated" ||
		changed[1].Name != "zeta" || changedCounts["miss"] != 2 || changedCounts["hit"] != 1 ||
		changedCounts["bypass"] != 0 {
		t.Fatalf("primary Git cache must reparse changed configs and rename unchanged subtrees: configs=%v, counts=%v",
			changed, changedCounts)
	}

	head, err := source.Head()
	if err != nil {
		t.Fatal(err)
	}

	if isPublishedPrimaryGitArtifact(sourceRoot, gitStore.MirrorDir(), head.Hash()) {
		t.Fatal("a working-tree checkout must not be treated as a published immutable artifact")
	}
}

func TestGetConfigs_PrimaryPublishedEncryptedNestedConfigUsesDisk(t *testing.T) {
	encryption.SetupAgeKeyEnvVar(t)

	sourceRoot := t.TempDir()

	source, err := git.PlainInitWithOptions(sourceRoot, &git.PlainInitOptions{DefaultBranch: DefaultReference})
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, ".doco-cd.yaml"),
		"compose_files: [compose.yaml]\nauto_discovery: true\n"); err != nil {
		t.Fatal(err)
	}

	serviceDir := filepath.Join(sourceRoot, "service")
	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	ciphertext, err := os.ReadFile(filepath.Join("..", "..", "encryption", "testdata", "encrypted.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, ".doco-cd.yaml"), string(ciphertext)); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "encrypted nested config"); err != nil {
		t.Fatal(err)
	}

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        sourceRoot,
		BaseDir:         t.TempDir(),
		CloneSubmodules: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	revision, err := gitStore.Resolve(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := gitStore.Publish(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}

	plaintext, err := os.ReadFile(filepath.Join(artifact.Path, "service", ".doco-cd.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if string(plaintext) == string(ciphertext) {
		t.Fatal("fixture was not decrypted in the published artifact")
	}

	counts := make(map[string]int)

	autoDiscoveryCacheObserverMu.RLock()

	previousObserver := autoDiscoveryCacheObserver

	autoDiscoveryCacheObserverMu.RUnlock()

	SetAutoDiscoveryCacheObserver(func(_, result string) { counts[result]++ })
	defer SetAutoDiscoveryCacheObserver(previousObserver)

	configs, err := GetConfigs(context.Background(), artifact.Path, ".", "", "main",
		gitStore.MirrorDir(), string(revision), &GitOptions{GitCloneSubmodules: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 || configs[0].Name != "service" || counts["bypass"] != 1 ||
		counts["hit"] != 0 || counts["miss"] != 0 {
		t.Fatalf("encrypted nested config must be read from published disk: configs=%v, counts=%v", configs, counts)
	}
}

func TestGetConfigs_PrimaryPublishedSymlinkedNestedConfigUsesDisk(t *testing.T) {
	sourceRoot := t.TempDir()

	source, err := git.PlainInitWithOptions(sourceRoot, &git.PlainInitOptions{DefaultBranch: DefaultReference})
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, ".doco-cd.yaml"),
		"compose_files: [compose.yaml]\nauto_discovery: true\n"); err != nil {
		t.Fatal(err)
	}

	serviceDir := filepath.Join(sourceRoot, "service")
	if err := os.MkdirAll(serviceDir, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(serviceDir, "override.yaml"),
		"environment:\n  FROM_LINK: materialized\n"); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink("override.yaml", filepath.Join(serviceDir, ".doco-cd.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := commitAll(t, source, "symlinked nested config"); err != nil {
		t.Fatal(err)
	}

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        sourceRoot,
		BaseDir:         t.TempDir(),
		CloneSubmodules: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	revision, err := gitStore.Resolve(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := gitStore.Publish(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int)

	autoDiscoveryCacheObserverMu.RLock()

	previousObserver := autoDiscoveryCacheObserver

	autoDiscoveryCacheObserverMu.RUnlock()

	SetAutoDiscoveryCacheObserver(func(_, result string) { counts[result]++ })
	defer SetAutoDiscoveryCacheObserver(previousObserver)

	configs, err := GetConfigs(context.Background(), artifact.Path, ".", "", "main",
		gitStore.MirrorDir(), string(revision), &GitOptions{GitCloneSubmodules: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 1 || configs[0].Environment["FROM_LINK"] != "materialized" ||
		counts["bypass"] != 1 || counts["hit"] != 0 || counts["miss"] != 0 {
		t.Fatalf("symlinked nested config must be read via disk: configs=%v, counts=%v", configs, counts)
	}
}

func TestGetConfigs_PrimaryPublishedArtifactMismatchUsesDisk(t *testing.T) {
	sourceRoot := t.TempDir()

	source, err := git.PlainInitWithOptions(sourceRoot, &git.PlainInitOptions{DefaultBranch: DefaultReference})
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, ".doco-cd.yaml"),
		"compose_files: [compose.yaml]\nauto_discovery: true\n"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "README.md"), "unique mismatch fixture"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "artifact source"); err != nil {
		t.Fatal(err)
	}

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        sourceRoot,
		BaseDir:         t.TempDir(),
		CloneSubmodules: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	revision, err := gitStore.Resolve(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := gitStore.Publish(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a materialized addition before the artifact has been proven.
	extra := filepath.Join(artifact.Path, "extra")
	if err := os.MkdirAll(extra, 0o750); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(extra, "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int)

	autoDiscoveryCacheObserverMu.RLock()

	previousObserver := autoDiscoveryCacheObserver

	autoDiscoveryCacheObserverMu.RUnlock()

	SetAutoDiscoveryCacheObserver(func(_, result string) { counts[result]++ })
	defer SetAutoDiscoveryCacheObserver(previousObserver)

	configs, err := GetConfigs(context.Background(), artifact.Path, ".", "", "main",
		gitStore.MirrorDir(), string(revision), &GitOptions{GitCloneSubmodules: true})
	if err != nil {
		t.Fatal(err)
	}

	if len(configs) != 2 || configs[1].WorkingDirectory != "extra" || counts["bypass"] != 1 {
		t.Fatalf("mismatched artifact inventory must be scanned on disk: configs=%v, counts=%v", configs, counts)
	}
}

func TestGetConfigs_NoMatchSubtreeBecomesInvalidAfterMaterialization(t *testing.T) {
	sourceRoot := t.TempDir()

	source, err := git.PlainInitWithOptions(sourceRoot, &git.PlainInitOptions{DefaultBranch: DefaultReference})
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, ".doco-cd.yaml"),
		"compose_files: [compose.yaml]\nauto_discovery: true\n"); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"idle", "live"} {
		if err := os.MkdirAll(filepath.Join(sourceRoot, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "idle", ".doco-cd.yaml"), "environment: [invalid\n"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "live", "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "no compose in idle"); err != nil {
		t.Fatal(err)
	}

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        sourceRoot,
		BaseDir:         t.TempDir(),
		CloneSubmodules: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	counts := make(map[string]int)

	autoDiscoveryCacheObserverMu.RLock()

	previousObserver := autoDiscoveryCacheObserver

	autoDiscoveryCacheObserverMu.RUnlock()

	SetAutoDiscoveryCacheObserver(func(_, result string) { counts[result]++ })
	defer SetAutoDiscoveryCacheObserver(previousObserver)

	discover := func() ([]*Config, error) {
		t.Helper()

		revision, resolveErr := gitStore.Resolve(context.Background(), "main")
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}

		artifact, publishErr := gitStore.Publish(context.Background(), revision)
		if publishErr != nil {
			t.Fatal(publishErr)
		}

		return GetConfigs(context.Background(), artifact.Path, ".", "", "main",
			gitStore.MirrorDir(), string(revision), &GitOptions{GitCloneSubmodules: true})
	}

	first, err := discover()
	if err != nil || len(first) != 1 || first[0].Name != "live" ||
		counts["miss"] != 3 || counts["bypass"] != 0 {
		t.Fatalf("invalid nested config without compose should be ignored and cached as no-match: configs=%v, counts=%v, err=%v",
			first, counts, err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "idle", "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "compose added to idle"); err != nil {
		t.Fatal(err)
	}

	counts = make(map[string]int)

	_, err = discover()
	if err == nil || !strings.Contains(err.Error(), "idle/.doco-cd.yaml") ||
		counts["miss"] != 2 || counts["bypass"] != 0 {
		t.Fatalf("changed materialized subtree must report its new nested parse error: counts=%v, err=%v", counts, err)
	}
}

func TestGetConfigs_PrimaryPublishedSubmoduleUsesDisk(t *testing.T) {
	sourceRoot := t.TempDir()

	source, err := git.PlainInitWithOptions(sourceRoot, &git.PlainInitOptions{DefaultBranch: DefaultReference})
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, ".doco-cd.yaml"),
		"compose_files: [compose.yaml]\nauto_discovery: true\n"); err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "compose.yaml"), "services: {}\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "submodule content"); err != nil {
		t.Fatal(err)
	}

	pinned, err := source.Head()
	if err != nil {
		t.Fatal(err)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, ".gitmodules"),
		"[submodule \"module\"]\n  path = module\n  url = "+sourceRoot+"\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "submodule declaration"); err != nil {
		t.Fatal(err)
	}

	addTestGitlink(t, source, "module", pinned.Hash())

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL:        sourceRoot,
		BaseDir:         t.TempDir(),
		CloneSubmodules: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	revision, err := gitStore.Resolve(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}

	artifact, err := gitStore.Publish(context.Background(), revision)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(artifact.Path, "module", "compose.yaml")); err != nil {
		t.Fatalf("expected materialized submodule compose file: %v", err)
	}

	counts := make(map[string]int)

	autoDiscoveryCacheObserverMu.RLock()

	previousObserver := autoDiscoveryCacheObserver

	autoDiscoveryCacheObserverMu.RUnlock()

	SetAutoDiscoveryCacheObserver(func(_, result string) { counts[result]++ })
	defer SetAutoDiscoveryCacheObserver(previousObserver)

	configs, err := GetConfigs(context.Background(), artifact.Path, ".", "", "main",
		gitStore.MirrorDir(), string(revision), &GitOptions{GitCloneSubmodules: true})
	if err != nil {
		t.Fatal(err)
	}

	foundModule := false

	for _, cfg := range configs {
		if cfg.WorkingDirectory == "module" {
			foundModule = true
		}
	}

	if len(configs) != 2 || !foundModule || counts["bypass"] != 1 ||
		counts["hit"] != 0 || counts["miss"] != 0 {
		t.Fatalf("materialized submodule inventory must come from disk: configs=%v, counts=%v", configs, counts)
	}
}

func addTestGitlink(t *testing.T, repo *git.Repository, name string, pinned plumbing.Hash) {
	t.Helper()

	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}

	parent, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}

	tree, err := parent.Tree()
	if err != nil {
		t.Fatal(err)
	}

	tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Submodule, Hash: pinned})
	sort.Slice(tree.Entries, func(i, j int) bool { return tree.Entries[i].Name < tree.Entries[j].Name })

	encodedTree := repo.Storer.NewEncodedObject()
	if err := tree.Encode(encodedTree); err != nil {
		t.Fatal(err)
	}

	treeHash, err := repo.Storer.SetEncodedObject(encodedTree)
	if err != nil {
		t.Fatal(err)
	}

	signature := object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	commit := &object.Commit{
		TreeHash:     treeHash,
		ParentHashes: []plumbing.Hash{head.Hash()},
		Author:       signature,
		Committer:    signature,
		Message:      "add gitlink",
	}

	encodedCommit := repo.Storer.NewEncodedObject()
	if err := commit.Encode(encodedCommit); err != nil {
		t.Fatal(err)
	}

	commitHash, err := repo.Storer.SetEncodedObject(encodedCommit)
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(head.Name(), commitHash)); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConfigs_InlinePublishedGitArtifactReusesSubtrees(t *testing.T) {
	sourceRoot := t.TempDir()

	source, err := git.PlainInitWithOptions(sourceRoot, &git.PlainInitOptions{DefaultBranch: DefaultReference})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"alpha", "beta"} {
		if err := os.Mkdir(filepath.Join(sourceRoot, name), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := createTestFile(t, filepath.Join(sourceRoot, name, "compose.yaml"), "services: {}\n"); err != nil {
			t.Fatal(err)
		}
	}

	if err := commitAll(t, source, "initial"); err != nil {
		t.Fatal(err)
	}

	gitStore, err := store.NewGitStore(store.GitStoreOptions{
		CloneURL: sourceRoot, BaseDir: t.TempDir(), CloneSubmodules: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	autoDiscoveryCacheObserverMu.RLock()

	previousObserver := autoDiscoveryCacheObserver

	autoDiscoveryCacheObserverMu.RUnlock()

	defer SetAutoDiscoveryCacheObserver(previousObserver)

	discover := func() ([]*Config, map[string]int) {
		t.Helper()

		revision, err := gitStore.Resolve(context.Background(), "main")
		if err != nil {
			t.Fatal(err)
		}

		artifact, err := gitStore.Publish(context.Background(), revision)
		if err != nil {
			t.Fatal(err)
		}

		counts := make(map[string]int)

		SetAutoDiscoveryCacheObserver(func(_, result string) { counts[result]++ })

		configs, err := ResolveConfigs(context.Background(), []*Config{{
			WorkingDirectory: ".", ComposeFiles: []string{"compose.yaml"},
			AutoDiscovery: AutoDiscoveryConfig{Enabled: true},
		}}, "", "main", artifact.Path, ".", gitStore.MirrorDir(), string(revision),
			&GitOptions{GitCloneSubmodules: true})
		if err != nil {
			t.Fatal(err)
		}

		return configs, counts
	}

	first, firstCounts := discover()
	if len(first) != 2 || firstCounts["bypass"] != 0 ||
		firstCounts["miss"] != 2 || firstCounts["hit"] != 1 {
		t.Fatalf("initial inline discovery must scan the published tree: configs=%v, counts=%v", first, firstCounts)
	}

	if err := createTestFile(t, filepath.Join(sourceRoot, "README.md"), "unrelated\n"); err != nil {
		t.Fatal(err)
	}

	if err := commitAll(t, source, "unrelated"); err != nil {
		t.Fatal(err)
	}

	second, secondCounts := discover()
	if len(second) != 2 || secondCounts["bypass"] != 0 ||
		secondCounts["hit"] != 2 || secondCounts["miss"] != 1 {
		t.Fatalf("inline discovery must reuse unchanged subtrees across revisions: configs=%v, counts=%v", second, secondCounts)
	}
}

func BenchmarkAutoDiscoverDeployments_ManyStacksAcrossRevisions(b *testing.B) {
	const projects = 2700 // The aggregated root exceeds the per-entry content limit.

	repoRoot := b.TempDir()

	repo, err := git.PlainInit(repoRoot, false)
	if err != nil {
		b.Fatal(err)
	}

	for i := range projects {
		name := "stack-" + strconv.Itoa(i)

		directory := filepath.Join(repoRoot, name)
		if err := os.Mkdir(directory, 0o750); err != nil {
			b.Fatal(err)
		}

		contents := "services:\n  svc" + strconv.Itoa(i) + ":\n    image: nginx\n"
		if err := os.WriteFile(filepath.Join(directory, "compose.yaml"), []byte(contents), 0o600); err != nil {
			b.Fatal(err)
		}
	}

	if err := commitAll(b, repo, "initial stacks"); err != nil {
		b.Fatal(err)
	}

	branch, err := repo.Head()
	if err != nil {
		b.Fatal(err)
	}

	tree, err := gitInternal.NewTreeFSAtCommit(repo, branch.Hash())
	if err != nil {
		b.Fatal(err)
	}

	base := &Config{
		WorkingDirectory: ".",
		ComposeFiles:     []string{"compose.yaml"},
		AutoDiscovery:    AutoDiscoveryConfig{Enabled: true},
	}
	firstFS := &countingDiscoveryTreeFS{TreeFS: tree, reads: make(map[string]int)}

	first, err := autoDiscoverDeployments(firstFS, repoRoot, branch.Hash().String(), base)
	if err != nil || len(first) != projects || len(firstFS.reads) != projects+1 {
		b.Fatalf("initial inventory: stacks=%d, directories=%d, err=%v", len(first), len(firstFS.reads), err)
	}

	rootHash, err := tree.SubtreeHash(".")
	if err != nil {
		b.Fatal(err)
	}

	settings, _ := json.Marshal(struct {
		ComposeFiles []string
		ConfigFiles  []string
	}{base.ComposeFiles, DefaultDeploymentConfigFileNames})
	if _, cached := autoDiscoveryCache.get(discoveryCacheKey{repoRoot, rootHash.String(), string(settings), -1}); cached {
		b.Fatal("expected aggregated root to exceed the per-entry cache limit")
	}

	if err := os.WriteFile(filepath.Join(repoRoot, "README.md"), []byte("unrelated"), 0o600); err != nil {
		b.Fatal(err)
	}

	if err := commitAll(b, repo, "unrelated revision"); err != nil {
		b.Fatal(err)
	}

	branch, err = repo.Head()
	if err != nil {
		b.Fatal(err)
	}

	tree, err = gitInternal.NewTreeFSAtCommit(repo, branch.Hash())
	if err != nil {
		b.Fatal(err)
	}

	var reads int

	b.ResetTimer()

	for b.Loop() {
		fsys := &countingDiscoveryTreeFS{TreeFS: tree, reads: make(map[string]int)}

		configs, scanErr := autoDiscoverDeployments(fsys, repoRoot, branch.Hash().String(), base)
		if scanErr != nil || len(configs) != projects {
			b.Fatalf("cached inventory: stacks=%d, err=%v", len(configs), scanErr)
		}

		for _, count := range fsys.reads {
			reads += count
		}
	}

	b.StopTimer()
	b.ReportMetric(float64(reads)/float64(b.N), "dir_reads/op")
}

func commitAll(t testing.TB, repo *git.Repository, message string) error {
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
