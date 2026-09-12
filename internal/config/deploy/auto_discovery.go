package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/clone"
	"github.com/kimdre/doco-cd/internal/common/types/set"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

// AutoDiscoveryConfig holds auto-discovery settings for a deployment.
type AutoDiscoveryConfig struct {
	ScanDepth     int  `yaml:"depth" json:"depth" default:"0"`                       // ScanDepth is the maximum depth of subdirectories to scan for docker-compose files
	Enabled       bool `yaml:"enabled" json:"enabled" default:"false"`               // Enabled enables autodiscovery of services to deploy in the working directory
	Delete        bool `yaml:"delete" json:"delete" default:"false"`                 // Delete removes obsolete auto-discovered deployments that are no longer present in the repository
	RemoveVolumes bool `yaml:"remove_volumes" json:"remove_volumes" default:"false"` // RemoveVolumes removes the volumes of an auto-discovered deployment when it is deleted
	RemoveImages  bool `yaml:"remove_images" json:"remove_images" default:"true"`    // RemoveImages removes the images of an auto-discovered deployment when it is deleted
}

var autoDiscoveryCache = struct {
	mu      sync.RWMutex
	entries map[string][]*Config
}{
	entries: map[string][]*Config{},
}

const maxAutoDiscoveryCacheEntries = 64

func (c *AutoDiscoveryConfig) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return errors.New("invalid auto_discovery value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	case yaml.MappingNode:
		type plain AutoDiscoveryConfig

		decoded := plain(*c)
		if err := node.Decode(&decoded); err != nil {
			return err
		}

		*c = AutoDiscoveryConfig(decoded)

		return nil
	default:
		return errors.New("invalid auto_discovery value: expected bool or object")
	}
}

func (c *AutoDiscoveryConfig) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("true")) || bytes.Equal(bytes.TrimSpace(data), []byte("false")) {
		var enabled bool
		if err := json.Unmarshal(data, &enabled); err != nil {
			return errors.New("invalid auto_discovery value: expected bool or object")
		}

		c.Enabled = enabled

		return nil
	}

	type plain AutoDiscoveryConfig

	decoded := plain(*c)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	*c = AutoDiscoveryConfig(decoded)

	return nil
}

// expandInlineAutoDiscoverConfigs replaces inline deployments that have auto-discovery
// enabled with the discovered deployments rooted at repoRoot.
func expandInlineAutoDiscoverConfigs(repoRoot string, deployments []*Config) ([]*Config, error) {
	expanded := make([]*Config, 0, len(deployments))

	fsys := os.DirFS(repoRoot)
	revisionKey := revisionKeyForRepoRoot(repoRoot)

	for _, deployment := range deployments {
		if !deployment.AutoDiscovery.Enabled {
			expanded = append(expanded, deployment)
			continue
		}

		discoveredConfigs, err := autoDiscoverDeployments(fsys, repoRoot, revisionKey, deployment)
		if err != nil {
			return nil, fmt.Errorf("failed to auto-discover deployment configurations: %w", err)
		}

		expanded = append(expanded, discoveredConfigs...)
	}

	return expanded, nil
}

// revisionKeyForRepoRoot returns the current HEAD commit hash for repoRoot,
// or "" when repoRoot is not a git repository (e.g. an OCI-sourced
// deployment). A "" key disables the auto-discovery cache for the call,
// matching the previous cacheable=false behavior for non-git sources.
func revisionKeyForRepoRoot(repoRoot string) string {
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return ""
	}

	head, err := repo.Head()
	if err != nil {
		return ""
	}

	return head.Hash().String()
}

// autoDiscoverDeployments scans for subdirectories containing docker-compose files
// and generates Config entries for each.
//
// fsys is the filesystem to scan, rooted at repoRoot: either the repository's
// on-disk working tree (os.DirFS) or a read-only view of a single commit's
// tree (*gitInternal.TreeFS) when the target reference differs from the one
// currently checked out. Scanning never checks a repository out to a
// different reference, so it cannot race with concurrent readers/writers of
// a shared working tree.
//
// repoRoot is the absolute path to the repository root; it is used only for
// labeling (cache keys, metrics, and matching baseConfig.Name against the
// repository's own directory name) and is never used for I/O directly.
//
// revisionKey identifies the exact content snapshot fsys exposes (a git
// commit SHA, or "" when the snapshot cannot be identified, which disables
// caching for that call). baseConfig.WorkingDirectory is treated as
// repo-root-relative.
func autoDiscoverDeployments(fsys fs.FS, repoRoot, revisionKey string, baseConfig *Config) ([]*Config, error) {
	repositoryLabel := filepath.Base(filepath.Clean(repoRoot))

	cacheKey, cacheable := autoDiscoveryCacheKey(repoRoot, revisionKey, baseConfig)
	if cacheable {
		autoDiscoveryCache.mu.RLock()
		cached, ok := autoDiscoveryCache.entries[cacheKey]
		autoDiscoveryCache.mu.RUnlock()

		if ok {
			recordAutoDiscoveryCacheLookup(repositoryLabel, "hit")
			return cloneConfigSlice(cached), nil
		}

		recordAutoDiscoveryCacheLookup(repositoryLabel, "miss")
	}

	var configs []*Config

	searchPath := path.Clean(baseConfig.WorkingDirectory)
	if searchPath == "" {
		searchPath = "."
	}

	composeFileNames := set.New(baseConfig.ComposeFiles...)

	err := fs.WalkDir(fsys, searchPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Calculate the depth of the current path relative to the search path.
		// fs.WalkDir paths are always "/"-separated and rooted at fsys, so a
		// simple prefix trim replaces filepath.Rel here.
		rel := "."
		if p != searchPath {
			rel = strings.TrimPrefix(p, searchPath+"/")
		}

		depth := 0
		if rel != "." {
			depth = strings.Count(rel, "/") + 1
		}

		// Skip directories that exceed the maximum depth if ScanDepth is set greater than 0
		if d.IsDir() && depth > baseConfig.AutoDiscovery.ScanDepth && baseConfig.AutoDiscovery.ScanDepth > 0 {
			return fs.SkipDir
		}

		if d.IsDir() {
			if filesystem.IsIgnoredDir(d.Name()) && p != searchPath {
				return fs.SkipDir
			}
		}

		if !d.IsDir() {
			return nil
		}

		// Read directory entries once, avoiding one stat per candidate compose filename.
		dirEntries, err := fs.ReadDir(fsys, p)
		if err != nil {
			return err
		}

		if !dirContainsAnyComposeFile(dirEntries, composeFileNames) {
			return nil
		}

		c := clone.New(baseConfig)

		// Get the stack name from the directory name where the compose file is
		// located. At the search root with no configured WorkingDirectory, p is
		// "." (fs.FS has no notion of the repository's own directory name), so
		// fall back to the repository label computed from repoRoot.
		stackDirName := path.Base(p)
		if p == "." {
			stackDirName = repositoryLabel
		}

		if baseConfig.Name != "" && stackDirName == repositoryLabel {
			c.Name = baseConfig.Name
		} else {
			c.Name = stackDirName
		}

		c.WorkingDirectory = p

		// Check for a nested .doco-cd config file alongside the compose file and
		// merge any overridable fields from it on top of the base config copy.
		// Reuse the already-read dirEntries instead of issuing additional Stat calls.
		for _, cfgName := range DefaultDeploymentConfigFileNames {
			if !dirHasFile(dirEntries, cfgName) {
				continue
			}

			localCfgPath := path.Join(p, cfgName)

			b, readErr := fs.ReadFile(fsys, localCfgPath)
			if readErr != nil {
				return fmt.Errorf("failed to read nested .doco-cd config at %s: %w", localCfgPath, readErr)
			}

			localConfigs, parseErr := getConfigFromYAMLBytes(b, localCfgPath, false)
			if parseErr != nil {
				return fmt.Errorf("failed to parse nested .doco-cd config at %s: %w", localCfgPath, parseErr)
			}

			if len(localConfigs) > 1 {
				return fmt.Errorf("%w: %s contains %d documents", ErrMultipleYAMLDocuments, localCfgPath, len(localConfigs))
			}

			mergeConfig(c, localConfigs[0])

			break // use first found config file name (.yaml preferred over .yml)
		}

		if err = c.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidConfig, err)
		}

		configs = append(configs, c)

		return nil
	})
	if err != nil {
		return nil, err
	}

	if cacheable {
		autoDiscoveryCache.mu.Lock()
		if _, exists := autoDiscoveryCache.entries[cacheKey]; !exists && len(autoDiscoveryCache.entries) >= maxAutoDiscoveryCacheEntries {
			for key := range autoDiscoveryCache.entries {
				delete(autoDiscoveryCache.entries, key)
				break
			}
		}

		autoDiscoveryCache.entries[cacheKey] = cloneConfigSlice(configs)
		autoDiscoveryCache.mu.Unlock()
	}

	return configs, nil
}

// autoDiscoveryCacheKey generates a unique cache key for the auto-discovery
// results. revisionKey identifies the exact content snapshot that was
// scanned (e.g. a resolved git commit SHA); an empty revisionKey means the
// snapshot cannot be identified reliably, so caching is disabled rather than
// risking a cache key collision between different content.
func autoDiscoveryCacheKey(repoRoot, revisionKey string, baseConfig *Config) (string, bool) {
	if revisionKey == "" {
		return "", false
	}

	configHash, err := baseConfig.Hash()
	if err != nil {
		return "", false
	}

	return strings.Join([]string{
		repoRoot,
		revisionKey,
		configHash,
		baseConfig.Internal.File,
		baseConfig.Internal.ConfigTarget,
		baseConfig.Internal.Hash,
		strconv.FormatBool(baseConfig.Internal.OciTrustPolicyOverrideTrusted),
	}, "|"), true
}

// cloneConfigSlice creates a deep copy of a slice of Config pointers.
func cloneConfigSlice(configs []*Config) []*Config {
	if configs == nil {
		return nil
	}

	cloned := make([]*Config, 0, len(configs))

	for _, cfg := range configs {
		if cfg == nil {
			cloned = append(cloned, nil)
			continue
		}

		cloned = append(cloned, clone.New(cfg))
	}

	return cloned
}

// dirContainsAnyComposeFile returns true when a compose filename is present as a
// regular file in the pre-read directory entries.
func dirContainsAnyComposeFile(entries []os.DirEntry, composeFileNames set.Set[string]) bool {
	if len(entries) == 0 || len(composeFileNames) == 0 {
		return false
	}

	for _, entry := range entries {
		if !entry.IsDir() && composeFileNames.Contains(entry.Name()) {
			return true
		}
	}

	return false
}

// dirHasFile returns true when the given filename exists as a non-directory
// entry in the pre-read slice.
func dirHasFile(entries []os.DirEntry, name string) bool {
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() == name {
			return true
		}
	}

	return false
}

// mergeConfig merges Config fields from override into base, but only for fields
// tagged with `doco:"allowOverride"`. Protected fields (reference, repository_url,
// auto_discovery, git_depth) are never overridden.
// Merge semantics:
//   - Maps: merged key-by-key (override wins on key collision)
//   - Slices: replaced entirely if the override slice is non-empty
//   - Nested structs: all sub-fields are merged (parent tag opts them in)
//   - Scalars: replaced if the override holds a non-zero value.
func mergeConfig(base, override *Config) {
	mergeStructByTag(reflect.ValueOf(base).Elem(), reflect.ValueOf(override).Elem())
}

// mergeStructByTag iterates a struct's fields and merges only those tagged doco:"allowOverride".
func mergeStructByTag(base, override reflect.Value) {
	t := base.Type()
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("doco") != "allowOverride" {
			continue
		}

		mergeField(base.Field(i), override.Field(i))
	}
}

// mergeField applies a single field merge from override into base.
// For structs the merge recurses into all sub-fields (no tag check – parent has opted in).
func mergeField(base, override reflect.Value) {
	switch base.Kind() {
	case reflect.Map:
		if override.IsNil() || override.Len() == 0 {
			return
		}

		if base.IsNil() {
			base.Set(reflect.MakeMap(base.Type()))
		}

		for _, k := range override.MapKeys() {
			base.SetMapIndex(k, override.MapIndex(k))
		}

	case reflect.Slice:
		if override.IsNil() || override.Len() == 0 {
			return
		}

		base.Set(override)

	case reflect.Struct:
		// Recurse into all sub-fields; the parent tag already opted them in.
		mergeAllStructFields(base, override)

	default:
		// Scalar: apply only when the override holds a non-zero value.
		if !override.IsZero() {
			base.Set(override)
		}
	}
}

// mergeAllStructFields merges every field of override into base without tag checks.
func mergeAllStructFields(base, override reflect.Value) {
	for i := 0; i < base.NumField(); i++ {
		mergeField(base.Field(i), override.Field(i))
	}
}
